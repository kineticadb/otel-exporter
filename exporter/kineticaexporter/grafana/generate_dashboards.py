#!/usr/bin/env python3
# Copyright The OpenTelemetry Authors
# SPDX-License-Identifier: Apache-2.0
"""Generates the Grafana dashboards of the metric tables into
grafana/dashboards. Run it from the exporter's directory after changing a
panel, then run the tests (TestDashboards) and tools/sqlcheck/check.sh:

    python3 grafana/generate_dashboards.py grafana/dashboards

The Kinetica Grafana plugin does not interpolate dashboard variables, so the
queries name the schema (otel) and use only the plugin's own macros. It marks
a result column as time only when it has the name of a TIMESTAMP column of the
first table named after FROM, so every time series query starts from a point
table (or metric_series) and calls its time column after one of that table's.
TIME_BUCKET returns a datetime string, which Grafana would read in the
browser's time zone, so each time column is MSECS_SINCE_EPOCH of its bucket:
milliseconds since the epoch, in UTC.
"""
import json
import sys
from pathlib import Path

DS = {"type": "kinetica-grafana-datasource", "uid": "${DS_KINETICA}"}
TIME_RANGE = "BETWEEN $__timeFrom() AND $__timeTo()"
BUCKET = 60000
HOST = "COALESCE(NULLIF(JSON_VALUE(s.labels, '$[\"host.name\"]'), ''), 'unknown')"
SERVICE = "COALESCE(s.service_name, 'unknown')"


def lag(col, alias):
    return f"LAG({col}) OVER (PARTITION BY {alias}.series_id ORDER BY {alias}.time_unix)"


def increase(alias, col, guard=None):
    """The increase of a cumulative column over the previous sample of its
    series; the value itself after a reset, detected on the guard column; null
    for a series' first sample, which has no previous one."""
    guard = guard or col
    previous = lag(f'{alias}.{guard}', alias)
    return (f"CASE WHEN {previous} IS NULL THEN NULL\n"
            f"                     WHEN {alias}.{guard} >= {previous}\n"
            f"                     THEN {alias}.{col} - {lag(f'{alias}.{col}', alias)}\n"
            f"                     ELSE {alias}.{col} END")


def per_second(alias, col):
    # A LONG times a decimal literal is a DECIMAL(28,6): cast to DOUBLE first
    return (f"CAST({increase(alias, col)} AS DOUBLE) * 1000\n"
            f"               / (MSECS_SINCE_EPOCH({alias}.time_unix)\n"
            f"                  - {lag(f'MSECS_SINCE_EPOCH({alias}.time_unix)', alias)})")


def series_filter(metric, extra="", temporality=None, cols="series_id, service_name, labels"):
    where = f"metric_name = '{metric}'"
    if temporality is not None:
        where += f" AND temporality = {temporality}"
    if extra:
        where += f"\n                 AND {extra}"
    return f"(SELECT {cols} FROM otel.metric_series\n               WHERE {where}) s"


def rate_query(table, alias, col, metric, groups, value_alias, extra_filter="", outer=None, keep=None, having=""):
    """Per-second rate of a cumulative column, summed per bucket for each
    group: a sample's rate is its increase over the time since the previous
    sample of its series; a series' rate in a bucket is the average of its
    samples' rates."""
    group_cols = ", ".join(name for name, _ in groups)
    group_exprs = ",\n               ".join(f"{expr} AS {name}" for name, expr in groups)
    select_outer = outer or f"SUM(series_rate) AS {value_alias}"
    outer_cols = ", ".join(keep) if keep else group_cols
    having_clause = f"\nHAVING {having}" if having else ""
    return f"""SELECT MSECS_SINCE_EPOCH(bucket) AS time_unix, {outer_cols}, {select_outer}
FROM (
    SELECT TIME_BUCKET({BUCKET}, time_unix) AS bucket, series_id, {group_cols},
           AVG(sample_rate) AS series_rate
    FROM (
        SELECT {alias}.series_id, {alias}.time_unix,
               {group_exprs},
               {per_second(alias, col)} AS sample_rate
        FROM otel.{table} {alias}
        JOIN {series_filter(metric, extra_filter, temporality=2)}
          ON s.series_id = {alias}.series_id
        WHERE {alias}.time_unix {TIME_RANGE}
    ) d
    WHERE sample_rate IS NOT NULL
    GROUP BY TIME_BUCKET({BUCKET}, time_unix), series_id, {group_cols}
) b
GROUP BY bucket, {outer_cols}{having_clause}
ORDER BY bucket, {outer_cols}"""


def gauge_query(metric, groups, value_alias, extra_filter="", aggregate="SUM"):
    """A gauge or non-monotonic sum: each series averaged per bucket, then
    aggregated across series per group."""
    group_cols = ", ".join(name for name, _ in groups)
    group_exprs = ",\n           ".join(f"{expr} AS {name}" for name, expr in groups)
    group_by = ", ".join(expr for _, expr in groups)
    return f"""SELECT MSECS_SINCE_EPOCH(bucket) AS time_unix, {group_cols}, {aggregate}(series_value) AS {value_alias}
FROM (
    SELECT TIME_BUCKET({BUCKET}, p.time_unix) AS bucket,
           p.series_id,
           {group_exprs},
           AVG(p.value) AS series_value
    FROM otel.metric_number_point p
    JOIN {series_filter(metric, extra_filter)}
      ON s.series_id = p.series_id
    WHERE p.time_unix {TIME_RANGE}
    GROUP BY TIME_BUCKET({BUCKET}, p.time_unix), p.series_id, {group_by}
) b
GROUP BY bucket, {group_cols}
ORDER BY bucket, {group_cols}"""


TIME_SERIES_TRANSFORMS = [
    {"id": "convertFieldType",
     "options": {"conversions": [{"targetField": "time_unix", "destinationType": "time"}], "fields": {}}},
    {"id": "prepareTimeSeries", "options": {"format": "multi"}},
]


class Dashboard:
    def __init__(self, uid, title, description, intro):
        self.uid, self.title, self.description = uid, title, description
        self.panels = []
        self.y = 0
        self.x = 0
        self.add_text(intro)

    def _place(self, w, h):
        if self.x + w > 24:
            self.x, self.y = 0, self.y + self.row_h
        pos = {"x": self.x, "y": self.y, "w": w, "h": h}
        self.x += w
        self.row_h = h
        if self.x >= 24:
            self.x, self.y = 0, self.y + h
        return pos

    def add_text(self, content, h=4):
        self.row_h = h
        self.panels.append({
            "id": len(self.panels) + 1, "type": "text", "title": "", "gridPos": self._place(24, h),
            "options": {"mode": "markdown", "content": content},
        })

    def add_timeseries(self, title, description, sql, unit, w=12, h=8, stack=False):
        defaults = {"unit": unit, "custom": {"fillOpacity": 10, "showPoints": "never", "spanNulls": True}}
        if stack:
            defaults["custom"]["stacking"] = {"mode": "normal", "group": "A"}
        self.panels.append({
            "id": len(self.panels) + 1, "type": "timeseries", "title": title, "description": description,
            "datasource": DS, "gridPos": self._place(w, h),
            "fieldConfig": {"defaults": defaults, "overrides": []},
            "options": {"legend": {"displayMode": "list", "placement": "bottom", "showLegend": True},
                        "tooltip": {"mode": "multi", "sort": "desc"}},
            "targets": [{"datasource": DS, "refId": "A", "rawSql": sql}],
            "transformations": TIME_SERIES_TRANSFORMS,
        })

    def add_table(self, title, description, sql, w=12, h=8, overrides=None):
        self.panels.append({
            "id": len(self.panels) + 1, "type": "table", "title": title, "description": description,
            "datasource": DS, "gridPos": self._place(w, h),
            "fieldConfig": {"defaults": {}, "overrides": overrides or []},
            "options": {"showHeader": True, "cellHeight": "sm"},
            "targets": [{"datasource": DS, "refId": "A", "rawSql": sql}],
        })

    def json(self):
        return {
            "annotations": {"list": []},
            "description": self.description,
            "editable": True,
            "fiscalYearStartMonth": 0,
            "graphTooltip": 1,
            "links": [],
            "panels": self.panels,
            "refresh": "1m",
            "schemaVersion": 39,
            "tags": ["opentelemetry", "kinetica", "metrics"],
            "templating": {"list": [{
                "current": {}, "hide": 0, "includeAll": False, "label": "Kinetica", "multi": False,
                "name": "DS_KINETICA", "options": [], "query": "kinetica-grafana-datasource", "refresh": 1,
                "regex": "", "skipUrlSync": False, "type": "datasource",
            }]},
            "time": {"from": "now-6h", "to": "now"},
            "timepicker": {},
            "timezone": "",
            "title": self.title,
            "uid": self.uid,
            "version": 1,
            "weekStart": "",
        }


def unit_override(field, unit):
    return {"matcher": {"id": "byName", "options": field}, "properties": [{"id": "unit", "value": unit}]}


COMMON_NOTE = ("Reads the metric tables the Kinetica exporter writes, in the schema **otel**; "
               "for another schema, replace `otel.` in the panel queries. Rates assume cumulative temporality "
               "(the default of most SDKs; for delta, run the collector's `deltatocumulative` processor). "
               "Each series is averaged per minute.")


def http_dashboard():
    d = Dashboard(
        "kinetica-otel-http", "OTel metrics on Kinetica / HTTP services",
        "HTTP server metrics of the OpenTelemetry semantic conventions, from the Kinetica exporter's metric tables.",
        "### HTTP services\nRequest rate, errors and duration per service, from `http.server.request.duration` "
        "(a histogram, in seconds) and `http.server.active_requests`. " + COMMON_NOTE)

    duration = "http.server.request.duration"
    d.add_timeseries(
        "Requests per second", "Observations of http.server.request.duration per second, per service.",
        rate_query("metric_histogram_point", "h", "data_count", duration, [("service", SERVICE)], "requests_per_second"),
        "reqps")
    d.add_timeseries(
        "5xx responses per second",
        "Requests answered with a 5xx status, found with text_match on the series' labels.",
        rate_query("metric_histogram_point", "h", "data_count", duration, [("service", SERVICE)], "errors_per_second",
                   extra_filter="text_match(labels, 'labels.http.response.status_code:/5../')"),
        "reqps")
    d.add_timeseries(
        "Error ratio", "Share of requests answered with a 5xx status, per service.",
        rate_query("metric_histogram_point", "h", "data_count", duration,
                   [("service", SERVICE),
                    ("is_error", "CASE WHEN JSON_VALUE(s.labels, '$[\"http.response.status_code\"]') LIKE '5%' "
                                 "THEN 1 ELSE 0 END")],
                   "error_ratio",
                   outer="SUM(CASE WHEN is_error = 1 THEN series_rate ELSE 0 END) / SUM(series_rate) AS error_ratio",
                   keep=["service"], having="SUM(series_rate) > 0"),
        "percentunit")
    d.add_timeseries(
        "Mean request duration",
        "Increase of the histogram's sum over the increase of its count, per service.",
        f"""SELECT MSECS_SINCE_EPOCH(bucket) AS time_unix, service, SUM(sum_increase) / SUM(count_increase) AS mean_duration
FROM (
    SELECT TIME_BUCKET({BUCKET}, h.time_unix) AS bucket,
           {SERVICE} AS service,
           {increase('h', 'data_sum', guard='data_count')} AS sum_increase,
           {increase('h', 'data_count')} AS count_increase
    FROM otel.metric_histogram_point h
    JOIN {series_filter(duration, temporality=2)}
      ON s.series_id = h.series_id
    WHERE h.time_unix {TIME_RANGE}
) d
WHERE count_increase > 0
GROUP BY bucket, service
ORDER BY bucket, service""",
        "s")
    d.add_timeseries(
        "Active requests", "http.server.active_requests, summed over each service's series.",
        gauge_query("http.server.active_requests", [("service", SERVICE)], "active_requests"),
        "short")
    d.add_table(
        "Busiest routes",
        "Requests, 5xx ratio and mean duration per service and route over the time range.",
        f"""SELECT service, route,
       SUM(count_increase) AS requests,
       CAST(SUM(CASE WHEN is_error = 1 THEN count_increase ELSE 0 END) AS DOUBLE) / SUM(count_increase) AS error_ratio,
       SUM(sum_increase) / SUM(count_increase) AS mean_duration
FROM (
    SELECT {SERVICE} AS service,
           COALESCE(NULLIF(JSON_VALUE(s.labels, '$["http.route"]'), ''), '(none)') AS route,
           CASE WHEN JSON_VALUE(s.labels, '$["http.response.status_code"]') LIKE '5%' THEN 1 ELSE 0 END AS is_error,
           {increase('h', 'data_sum', guard='data_count')} AS sum_increase,
           {increase('h', 'data_count')} AS count_increase
    FROM otel.metric_histogram_point h
    JOIN {series_filter(duration, temporality=2)}
      ON s.series_id = h.series_id
    WHERE h.time_unix {TIME_RANGE}
) d
WHERE count_increase > 0
GROUP BY service, route
ORDER BY requests DESC
LIMIT 25""",
        overrides=[unit_override("error_ratio", "percentunit"), unit_override("mean_duration", "s")])
    d.add_table(
        "Slowest exemplars",
        "The exemplars of http.server.request.duration with the largest values; look their trace_id up in "
        "the trace tables.",
        f"""SELECT e.time_unix AS point_time_unix, e.value AS duration, {SERVICE} AS service,
       JSON_VALUE(s.labels, '$["http.route"]') AS route, e.trace_id, e.span_id
FROM otel.metric_exemplar e
JOIN {series_filter(duration)}
  ON s.series_id = e.series_id
WHERE e.time_unix {TIME_RANGE}
ORDER BY e.value DESC
LIMIT 25""",
        overrides=[unit_override("duration", "s")])
    return d


def hosts_dashboard():
    d = Dashboard(
        "kinetica-otel-hosts", "OTel metrics on Kinetica / Hosts",
        "Host metrics of the collector's hostmetrics receiver, from the Kinetica exporter's metric tables.",
        "### Hosts\nCPU, memory, filesystem, network and disk metrics of the collector's `hostmetrics` "
        "receiver, per `host.name` (set by the `resourcedetection` processor). " + COMMON_NOTE)

    d.add_timeseries(
        "CPU utilization", "Share of CPU time not idle, across all CPUs of each host (system.cpu.time).",
        rate_query("metric_number_point", "p", "value", "system.cpu.time",
                   [("host", HOST), ("state", "JSON_VALUE(s.labels, '$[\"state\"]')")], "cpu_utilization",
                   outer="SUM(CASE WHEN state <> 'idle' THEN series_rate ELSE 0 END) / SUM(series_rate) "
                         "AS cpu_utilization",
                   keep=["host"], having="SUM(series_rate) > 0"),
        "percentunit")
    d.add_timeseries(
        "Load average (1m)", "system.cpu.load_average.1m per host.",
        gauge_query("system.cpu.load_average.1m", [("host", HOST)], "load_average", aggregate="AVG"),
        "short")
    d.add_timeseries(
        "Memory used", "system.memory.usage in the used state, found with text_match on the series' labels.",
        gauge_query("system.memory.usage", [("host", HOST)], "used_bytes",
                    extra_filter="text_match(labels, 'labels.state:\"used\"')"),
        "bytes")
    d.add_timeseries(
        "Filesystem utilization", "Used share of each filesystem (system.filesystem.usage), per host and mountpoint.",
        f"""SELECT MSECS_SINCE_EPOCH(bucket) AS time_unix, host, mountpoint,
       SUM(CASE WHEN state = 'used' THEN series_value ELSE 0 END) / SUM(series_value) AS utilization
FROM (
    SELECT TIME_BUCKET({BUCKET}, p.time_unix) AS bucket,
           p.series_id,
           {HOST} AS host,
           JSON_VALUE(s.labels, '$["mountpoint"]') AS mountpoint,
           JSON_VALUE(s.labels, '$["state"]') AS state,
           AVG(p.value) AS series_value
    FROM otel.metric_number_point p
    JOIN {series_filter("system.filesystem.usage")}
      ON s.series_id = p.series_id
    WHERE p.time_unix {TIME_RANGE}
    GROUP BY TIME_BUCKET({BUCKET}, p.time_unix), p.series_id, {HOST},
             JSON_VALUE(s.labels, '$["mountpoint"]'), JSON_VALUE(s.labels, '$["state"]')
) b
GROUP BY bucket, host, mountpoint
HAVING SUM(series_value) > 0
ORDER BY bucket, host, mountpoint""",
        "percentunit")
    d.add_timeseries(
        "Network throughput", "Bytes per second per host and direction (system.network.io).",
        rate_query("metric_number_point", "p", "value", "system.network.io",
                   [("host", HOST), ("direction", "JSON_VALUE(s.labels, '$[\"direction\"]')")],
                   "bytes_per_second"),
        "Bps")
    d.add_timeseries(
        "Disk throughput", "Bytes per second per host and direction (system.disk.io).",
        rate_query("metric_number_point", "p", "value", "system.disk.io",
                   [("host", HOST), ("direction", "JSON_VALUE(s.labels, '$[\"direction\"]')")],
                   "bytes_per_second"),
        "Bps")
    return d


def inventory_dashboard():
    d = Dashboard(
        "kinetica-otel-inventory", "OTel metrics on Kinetica / Inventory",
        "What the Kinetica exporter has written to its metric tables: metrics, series, labels and ingest volume.",
        "### Inventory\nThe metrics, series and labels in the metric tables, and the points written. "
        "Reads the schema **otel**; for another schema, replace `otel.` in the panel queries.")

    d.add_table(
        "Metrics", "Every metric, with its number of series and services.",
        """SELECT metric_name, metric_type, unit,
       COUNT(*) AS series,
       COUNT(DISTINCT service_name) AS services,
       MIN(first_seen) AS first_seen
FROM otel.metric_series
GROUP BY metric_name, metric_type, unit
ORDER BY series DESC, metric_name""",
        w=24, h=10, overrides=[unit_override("first_seen", "dateTimeAsIso")])
    d.add_table(
        "Series per service", "Series and metrics of each service (service.name).",
        """SELECT COALESCE(service_name, '(none)') AS service,
       COUNT(*) AS series,
       COUNT(DISTINCT metric_name) AS metric_names
FROM otel.metric_series
GROUP BY COALESCE(service_name, '(none)')
ORDER BY series DESC""")
    d.add_table(
        "Label keys", "Every label key, with the number of series having it: the cardinality to watch.",
        """SELECT label, COUNT(*) AS series
FROM UNNEST_JSON_OBJECT(SELECT labels FROM otel.metric_series) t(label, label_value)
GROUP BY label
ORDER BY series DESC, label""")
    d.add_timeseries(
        "Points written per minute", "Rows written to each point table.",
        f"""SELECT MSECS_SINCE_EPOCH(TIME_BUCKET({BUCKET}, time_unix)) AS time_unix, 'number' AS point_table, COUNT(*) AS points
FROM otel.metric_number_point
WHERE time_unix {TIME_RANGE}
GROUP BY TIME_BUCKET({BUCKET}, time_unix)
UNION ALL
SELECT MSECS_SINCE_EPOCH(TIME_BUCKET({BUCKET}, time_unix)) AS time_unix, 'histogram' AS point_table, COUNT(*) AS points
FROM otel.metric_histogram_point
WHERE time_unix {TIME_RANGE}
GROUP BY TIME_BUCKET({BUCKET}, time_unix)
UNION ALL
SELECT MSECS_SINCE_EPOCH(TIME_BUCKET({BUCKET}, time_unix)) AS time_unix, 'exp_histogram' AS point_table, COUNT(*) AS points
FROM otel.metric_exp_histogram_point
WHERE time_unix {TIME_RANGE}
GROUP BY TIME_BUCKET({BUCKET}, time_unix)
UNION ALL
SELECT MSECS_SINCE_EPOCH(TIME_BUCKET({BUCKET}, time_unix)) AS time_unix, 'summary' AS point_table, COUNT(*) AS points
FROM otel.metric_summary_point
WHERE time_unix {TIME_RANGE}
GROUP BY TIME_BUCKET({BUCKET}, time_unix)""",
        "short", stack=True)
    d.add_timeseries(
        "New series per hour", "Series seen for the first time, per metric type.",
        f"""SELECT MSECS_SINCE_EPOCH(TIME_BUCKET(3600000, first_seen)) AS first_seen, metric_type, COUNT(*) AS new_series
FROM otel.metric_series
WHERE first_seen {TIME_RANGE}
GROUP BY TIME_BUCKET(3600000, first_seen), metric_type
ORDER BY first_seen""",
        "short", stack=True)
    # This panel's time column is first_seen, the TIMESTAMP column of metric_series
    d.panels[-1]["transformations"] = [
        {"id": "convertFieldType",
         "options": {"conversions": [{"targetField": "first_seen", "destinationType": "time"}], "fields": {}}},
        {"id": "prepareTimeSeries", "options": {"format": "multi"}},
    ]
    return d


def main(out_dir):
    out = Path(out_dir)
    out.mkdir(parents=True, exist_ok=True)
    for dashboard, name in [(http_dashboard(), "otel-metrics-http.json"),
                            (hosts_dashboard(), "otel-metrics-hosts.json"),
                            (inventory_dashboard(), "otel-metrics-inventory.json")]:
        (out / name).write_text(json.dumps(dashboard.json(), indent=2) + "\n")
        print("wrote", out / name, len(dashboard.panels), "panels")


if __name__ == "__main__":
    main(sys.argv[1] if len(sys.argv) > 1 else str(Path(__file__).parent / "dashboards"))
