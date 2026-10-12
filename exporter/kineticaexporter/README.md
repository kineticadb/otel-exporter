<h3 align="center" style="margin:0px">
 <img width="200" src="https://www.kinetica.com/wp-content/uploads/2018/08/kinetica_logo.svg" alt="Kinetica Logo"/>
</h3>
<h5 align="center" style="margin:0px">
 <a href="https://www.kinetica.com/">Website</a>
 |
 <a href="https://docs.kinetica.com/7.1/">Docs</a>
 |
 <a href="https://join.slack.com/t/kinetica-community/shared_invite/zt-1bt9x3mvr-uMKrXlSDXfy3oU~sKi84qg">Community Slack</a>
</h5>

# Kinetica OpenTelemetry Collector Exporter Plug-In

- [Overview](#overview)
- [Installation](#installation)
- [Documentation](#documentation)
- [Support](#support)
- [Contact Us](#contact-us)

## Overview

# Kinetica Opentelemetry Exporter

 This exporter could be used to as part of an `Opentelemetry` collector to persist data related to
 `logs`, `traces` and `metrics` to the `Kinetica` database.

 This component is under `alpha` status.

## Steps to build the collector binary

- Please refer to <https://github.com/open-telemetry/opentelemetry-collector/tree/main/cmd/builder> for instructions on how to get the latest binary and install locally. This link mentions - "Download the binary for your respective platform under the "Releases" page. If install an official release build, the binary is named ocb, but if you installed by using go install, it will be called builder".
- As this link describes install the Collector Builder as a package; it will be available as `ocb` after installation if installed
  from an official release.
- An example config file for building the collector binary is as follows:

```yaml
dist:
  name: otelcol-kinetica
  description: Otel collector with Kinetica exporter
  output_path: /home/kinetica/otelexporter_utils/collector-binary
  otelcol_version: 0.78.2

exporters:
  - gomod:
      github.com/kineticadb/otel-exporter/exporter/kineticaexporter v0.78.0

processors:
  - gomod:
      go.opentelemetry.io/collector/processor/batchprocessor v0.78.2

receivers:
  - gomod:
      go.opentelemetry.io/collector/receiver/otlpreceiver v0.78.2
  - gomod:
      github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusreceiver v0.78.0
```

- Save the config file as e.g., `config_file.yaml`.
- Once the builder config file has been created the binary can be build using the command `ocb --config config_file.yaml`.

## Configuring the collector

The OTEL collector has a number of components at play.

1. The Receiver - this will receive the telemetry data from one of the sources which has been instrumented to produce logs, traces and metrics.
2. The Processor - the metric data received from the `Receiver`s will be passed on to this component.
3. The Exporter - this component is fed by the processor and is used to persist the metrics data to a data store (kike Kinetica) or some other component for further processing.

A sample configuration for the collector binary is as follows:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: localhost:4317

  prometheus:
      config:
        scrape_configs:
          - job_name: 'ki_stats'
            honor_labels: true
            static_configs:
              - targets: ['172.31.32.21:9010', '172.31.32.15:9010', '172.31.32.16:9010', '172.31.32.18:9010', '172.31.33.29:9010', '172.31.32.19:9010', '172.31.32.26:9010', '172.31.32.20:9010', '172.31.32.17:9010']

processors:
  batch:

exporters:
  kinetica:
    host: http://localhost:9191/
    schema: otel
    username: admin
    password: password
    bypasssslcertcheck: true
    logconfigfile: log_config.yaml


service:
  pipelines:
    traces:
      receivers:
      - otlp
      processors:
      - batch
      exporters:
      - kinetica
    metrics:
      receivers:
      - otlp
      - prometheus
      processors:
      - batch
      exporters:
      - kinetica
    logs:
      receivers:
      - otlp
      processors:
      - batch
      exporters:
      - kinetica
```

The first section of the config file describes the components (receivers,
exporters etc.) with their configurations. The second section describes the
service details which defines the pipeline stages for logs, traces, and metrics.

Save the config file as `collector-config.yaml`.

The command to run the collector binary produced by the steps described above is:

  `collector-binary --config collector-config.yaml`

### Configuring the Kinetica Exporter

The following parameters will be necessary to define the connection for the
*Kinetica OpenTelemetry Collector Exporter*.

#### Configuration parameters for the Kinetica Exporter

| Name | Description |
|--|--|
| host | Kinetica host URL |
| schema | Kinetica OpenTelemetry schema, if omitted the default schema will be used |
| username | Kinetica User |
| password | Kinetica Password |
| bypasssslcertcheck | Whether to skip SSL certificate check or not (true or false)|
| logconfigfile | Name of the log config file |
| metricsschema | The table layout metrics are written in: `v1` (default) or `v2` (see below) |
| metricsv2.seriescachesize | v2 only: the number of series remembered as already written, so their rows are not sent again (default `1000000`; `0` remembers none) |
| metricsv2.labelsanalyzer | v2 only: the text-search analyzer of the `labels` column (default `keyword`, exact and case-sensitive); empty uses the server's default |
| metricsv2.excludelabelkeys | v2 only: attribute keys left out of each series' identity and labels, such as high-cardinality ones. Series that differ only by these keys become one series, and only one of their points at a given time is kept |

#### The v2 metrics layout

With `metricsschema: v2` the exporter writes six tables instead of the 38 of `v1`:

| Table | Holds |
|--|--|
| `metric_series` | One row per time series, keyed by `series_id`, a hash of its name, type, unit, temporality and attributes. `labels` is the union of its resource, scope and data point attributes as flat JSON (dotted keys kept, values as strings), declared `JSON(text_search)` |
| `metric_number_point` | Gauge and sum data points |
| `metric_histogram_point` | Histogram data points, with the bounds and bucket counts as JSON arrays |
| `metric_exp_histogram_point` | Exponential histogram data points |
| `metric_summary_point` | Summary data points, with the quantiles as JSON arrays |
| `metric_exemplar` | Exemplars of every type, with their trace and span IDs |

Every table is sharded on `series_id`. Search the series by label, then join their points:

```sql
SELECT s.series_id, p.time_unix, p.value
FROM (SELECT series_id FROM otel.metric_series
       WHERE metric_name = 'http.server.active_requests'
         AND text_match(labels, 'labels.service.name:"checkout"')
         AND text_match(labels, 'labels.http.response.status_code:/5../')) s
JOIN otel.metric_number_point p ON p.series_id = s.series_id;
```

`v2` needs a Kinetica server with full-text search enabled (`enable_text_search = true`).

#### Querying the v2 layout

| Path | What |
|--|--|
| `sql/metrics_v2/queries.sql` | Query templates: series search (exact, regex, negated), metric catalog, label names and values, gauges, counter and delta rates, histogram rates and means, histogram buckets and quantiles (experimental, delta histograms), exemplars, reads of the rollup |
| `sql/metrics_v2/rollups.sql` | One-minute and hourly rollups of `metric_number_point`, kept up to date by scheduled SQL procedures, and an optional retention procedure |
| `grafana/dashboards/` | Grafana dashboards for the Kinetica datasource plugin (`kinetica-datasource`): HTTP services (semantic conventions), hosts (`hostmetrics` receiver) and an inventory of metrics, series and labels |
| `grafana/generate_dashboards.py` | Generates the dashboards; edit it rather than the JSON |
| `tools/sqlcheck/` | Checks the syntax of all of this SQL, and of the table DDL, with Kinetica's own SQL parser |

Each template starts with a `-- name:` line and lists its `{{placeholders}}` on a `-- params:` line.
Label filters are `text_match` calls on `metric_series.labels`, in a subquery whose series are then
joined to a point table:

```sql
-- PromQL {service.name="checkout", http.response.status_code=~"5.."}
WHERE text_match(labels, 'labels.service.name:"checkout"')
  AND text_match(labels, 'labels.http.response.status_code:/5../')
```

Run `rollups.sql` once after the exporter has created its tables, replacing `{{schema}}`.

The dashboards read the schema `otel`, and rates assume cumulative temporality (for delta, run the
collector's `deltatocumulative` processor). They are written for the plugin as it is: it expands only
`$__timeFrom()`, `$__timeTo()`, `$__timeFilter()` and the epoch macros, not dashboard variables, so the
panels break series down by service or host instead of filtering on a variable, and the schema is
written in each query (replace `otel.` for another one). The plugin marks a column as time only when
it has the name of a TIMESTAMP column of the first table after `FROM`, so each time series query reads
a point table first and calls its time bucket `time_unix`; the panels' `convertFieldType` and
`prepareTimeSeries` transformations then make one series per breakdown value. Import them, or provision
them with `grafana/provisioning/dashboards.yml`.

To check the SQL without a server, point `KINETICA_SQL_JAR` at the `gpudbsql` jar of a Kinetica
install (it is in `/opt/gpudb/sql` of a Kinetica container) and run:

```sh
KINETICA_SQL_JAR=/path/to/gpudbsql-7.2.3.0.jar tools/sqlcheck/check.sh
```

To run everything against a server instead, set `KINETICA_TEST_URL`, `KINETICA_TEST_USER` and
`KINETICA_TEST_PASSWORD`, then run `go test -run 'TestMetricsV2Integration|TestMetricsV2Spike' .`. Each
test writes metrics to a schema of its own, which it drops at the end. `TestMetricsV2Integration` runs
every template and dashboard query; `TestMetricsV2Spike` writes metrics whose results are known and
checks the exact values the label searches, templates, rollups and dashboard queries return. Setting
`KINETICA_TEST_LOAD` to a number of points (e.g. `2000000`) also runs `TestMetricsV2Load`, which
reports the write rate with and without concurrent label searches, and with a stream of new series.

```yaml
exporters:
  kinetica:
    host: http://localhost:9191/
    schema: otel
    username: admin
    password: password
    metricsschema: v2
    metricsv2:
      seriescachesize: 1000000
      labelsanalyzer: keyword
      excludelabelkeys: []
```

### Configuring Logging

The logging is done using Uber zap package and `lumberjack` for rotating files based on size. Time based rotation is not supported yet. The configuration for `lumberjack` can be found here - <https://pkg.go.dev/gopkg.in/natefinch/lumberjack.v2>

#### Default Log Config file

This is included and will be used in case a user defined config file is not found. The name of the file is `config_log_zap.yaml`.

```yaml
level: 'info'
development: true
disableCaller: false
disableStacktrace: false
encoding: 'console'
encoderConfig:
  messageKey: 'msg'
  levelKey: 'level'
  timeKey: 'ts'
  nameKey: 'logger'
  callerKey: 'caller'
  functionKey: 'function'
  stacktraceKey: 'stacktrace'
  skipLineEnding: false
  lineEnding: "\n"
  levelEncoder: 'capital'
  timeEncoder: 'iso8601'
  durationEncoder: 'string'
  callerEncoder: 'full'
  nameEncoder: 'full'
  consoleSeparator: ' | '
outputPaths:
  # Implements loggin to the console
  - 'stdout'
  # Implements rolling logs using lumberjack logger; config parameters are supplied as
  # query params. Here maxSize is 10MB after which the logger rolls over; maximum
  # number of backups (maxBackups) kept is 5 and maxAge is 10 days.
  # The name of the log file in this case is "logs/kinetica-exporter.log" where the
  # "logs" directory is under the current directory on the local machine.
  - 'lumberjack://localhost/logs/kinetica-exporter.log?maxSize=10&maxBackups=5&maxAge=10'
errorOutputPaths:
  - 'stderr'
  - './logs/error_logs'
initialFields:
  app: 'kinetica-exporter'
```

## Documentation

- [Full Documentation](https://docs.kinetica.com/7.1/)
- [OpenTelemetry Docs](https://opentelemetry.io/docs/)
- [OpenTelemetry Exporter Registry](https://opentelemetry.io/ecosystem/registry/?component=exporter)

## Support

For support, you can post on
[stackoverflow](https://stackoverflow.com/questions/tagged/kinetica) under the
``kinetica`` tag or
[Slack](https://join.slack.com/t/kinetica-community/shared_invite/zt-1bt9x3mvr-uMKrXlSDXfy3oU~sKi84qg).

## Contact Us

- Ask a question on Slack:
  [Slack](https://join.slack.com/t/kinetica-community/shared_invite/zt-1bt9x3mvr-uMKrXlSDXfy3oU~sKi84qg)
- Follow on GitHub:
  [Follow @kineticadb](https://github.com/kineticadb)
- Email us:  <support@kinetica.com>
- Visit:  <https://www.kinetica.com/contact/>
