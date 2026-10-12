-- Query templates for the metric tables
--
-- Each template starts with a "-- name:" line, followed by comment lines
-- describing it and a "-- params:" line listing its placeholders. A
-- placeholder is written {{name}} and is replaced by plain text substitution:
--
--   schema     the schema the exporter writes to, e.g. otel
--   metric     a metric name, e.g. http.server.request.duration
--   label      a label key, dots kept, e.g. http.route
--   value      a label value, matched exactly and case-sensitively
--   regex      a regular expression matched against a whole label value
--   from, to   the time range, as 'YYYY-MM-DD HH:MM:SS' in UTC
--   bucket_ms  the width of a time bucket, in milliseconds
--   quantile   a fraction between 0 and 1, e.g. 0.95
--   limit      a maximum number of rows
--
-- Values are not escaped: a value holding ' or " must be escaped by the
-- caller, or matched with JSON_VALUE instead of text_match.
--
-- Label filters use text_match on metric_series.labels, a JSON(text_search)
-- column holding each series' resource, scope and data point attributes as
-- strings. A text-search function evaluates against a single table, so it is
-- always used in a subquery on metric_series, whose series_ids are then
-- joined to a point table. Rates assume cumulative temporality (2) unless
-- the template says otherwise. A LONG times a decimal literal is a DECIMAL(28,6)
-- in Kinetica, so counts are cast to DOUBLE before any division.

-- name: series_search
-- The series of a metric with a label of an exact value.
-- params: schema, metric, label, value, limit
SELECT series_id, metric_name, metric_type, unit, service_name, labels
FROM {{schema}}.metric_series
WHERE metric_name = '{{metric}}'
  AND text_match(labels, 'labels.{{label}}:"{{value}}"')
ORDER BY series_id
LIMIT {{limit}};

-- name: series_search_regex
-- The series of a metric with a label whose whole value matches a regex,
-- as a PromQL =~ matcher does.
-- params: schema, metric, label, regex, limit
SELECT series_id, metric_name, metric_type, unit, service_name, labels
FROM {{schema}}.metric_series
WHERE metric_name = '{{metric}}'
  AND text_match(labels, 'labels.{{label}}:/{{regex}}/')
ORDER BY series_id
LIMIT {{limit}};

-- name: series_search_negated
-- The series of a metric without a label of the given value, including the
-- series that lack the label, as a PromQL != matcher does.
-- params: schema, metric, label, value, limit
SELECT series_id, metric_name, metric_type, unit, service_name, labels
FROM {{schema}}.metric_series
WHERE metric_name = '{{metric}}'
  AND NOT text_match(labels, 'labels.{{label}}:"{{value}}"')
ORDER BY series_id
LIMIT {{limit}};

-- name: metric_catalog
-- Every metric, with its number of series and services.
-- params: schema
SELECT metric_name, metric_type, unit,
       COUNT(*) AS series,
       COUNT(DISTINCT service_name) AS services,
       MIN(first_seen) AS first_seen
FROM {{schema}}.metric_series
GROUP BY metric_name, metric_type, unit
ORDER BY series DESC, metric_name;

-- name: label_names
-- The label keys of a metric's series, with the number of series having each.
-- params: schema, metric
SELECT label, COUNT(*) AS series
FROM UNNEST_JSON_OBJECT(SELECT labels FROM {{schema}}.metric_series
                         WHERE metric_name = '{{metric}}') t(label, label_value)
GROUP BY label
ORDER BY series DESC, label;

-- name: label_values
-- The values of a label across a metric's series.
-- params: schema, metric, label
SELECT JSON_VALUE(labels, '$["{{label}}"]') AS label_value, COUNT(*) AS series
FROM {{schema}}.metric_series
WHERE metric_name = '{{metric}}'
GROUP BY JSON_VALUE(labels, '$["{{label}}"]')
ORDER BY series DESC;

-- name: gauge_by_label
-- A gauge, or a non-monotonic sum, averaged per time bucket for each value
-- of a label.
-- params: schema, metric, label, from, to, bucket_ms
SELECT bucket AS time_unix, label_value, AVG(series_value) AS value
FROM (
    SELECT TIME_BUCKET({{bucket_ms}}, p.time_unix) AS bucket,
           p.series_id,
           JSON_VALUE(s.labels, '$["{{label}}"]') AS label_value,
           AVG(p.value) AS series_value
    FROM {{schema}}.metric_number_point p
    JOIN (SELECT series_id, labels FROM {{schema}}.metric_series
           WHERE metric_name = '{{metric}}') s
      ON s.series_id = p.series_id
    WHERE p.time_unix BETWEEN '{{from}}' AND '{{to}}'
    GROUP BY TIME_BUCKET({{bucket_ms}}, p.time_unix), p.series_id, JSON_VALUE(s.labels, '$["{{label}}"]')
) b
GROUP BY bucket, label_value
ORDER BY bucket, label_value;

-- name: counter_rate_by_label
-- The per-second rate of a cumulative monotonic sum, summed per time bucket
-- for each value of a label. Each sample's rate is its increase over the
-- previous sample of its series (the value itself after a counter reset);
-- a series' rate in a bucket is the average of its samples' rates.
-- params: schema, metric, label, from, to, bucket_ms
SELECT bucket AS time_unix, label_value, SUM(series_rate) AS per_second
FROM (
    SELECT TIME_BUCKET({{bucket_ms}}, time_unix) AS bucket, series_id, label_value,
           AVG(sample_rate) AS series_rate
    FROM (
        SELECT p.series_id, p.time_unix,
               JSON_VALUE(s.labels, '$["{{label}}"]') AS label_value,
               CAST(CASE WHEN LAG(p.value) OVER (PARTITION BY p.series_id ORDER BY p.time_unix) IS NULL THEN NULL
                     WHEN p.value >= LAG(p.value) OVER (PARTITION BY p.series_id ORDER BY p.time_unix)
                     THEN p.value - LAG(p.value) OVER (PARTITION BY p.series_id ORDER BY p.time_unix)
                     ELSE p.value END AS DOUBLE) * 1000
               / (MSECS_SINCE_EPOCH(p.time_unix)
                  - LAG(MSECS_SINCE_EPOCH(p.time_unix)) OVER (PARTITION BY p.series_id ORDER BY p.time_unix)) AS sample_rate
        FROM {{schema}}.metric_number_point p
        JOIN (SELECT series_id, labels FROM {{schema}}.metric_series
               WHERE metric_name = '{{metric}}' AND temporality = 2) s
          ON s.series_id = p.series_id
        WHERE p.time_unix BETWEEN '{{from}}' AND '{{to}}'
    ) d
    WHERE sample_rate IS NOT NULL
    GROUP BY TIME_BUCKET({{bucket_ms}}, time_unix), series_id, label_value
) b
GROUP BY bucket, label_value
ORDER BY bucket, label_value;

-- name: delta_rate_by_label
-- The per-second rate of a delta sum: each point's value over the time since
-- its start time, summed per time bucket for each value of a label.
-- params: schema, metric, label, from, to, bucket_ms
SELECT bucket AS time_unix, label_value, SUM(series_rate) AS per_second
FROM (
    SELECT TIME_BUCKET({{bucket_ms}}, p.time_unix) AS bucket,
           p.series_id,
           JSON_VALUE(s.labels, '$["{{label}}"]') AS label_value,
           AVG(p.value * 1000 / (MSECS_SINCE_EPOCH(p.time_unix) - MSECS_SINCE_EPOCH(p.start_time_unix))) AS series_rate
    FROM {{schema}}.metric_number_point p
    JOIN (SELECT series_id, labels FROM {{schema}}.metric_series
           WHERE metric_name = '{{metric}}' AND temporality = 1) s
      ON s.series_id = p.series_id
    WHERE p.time_unix BETWEEN '{{from}}' AND '{{to}}'
      AND p.start_time_unix < p.time_unix
    GROUP BY TIME_BUCKET({{bucket_ms}}, p.time_unix), p.series_id, JSON_VALUE(s.labels, '$["{{label}}"]')
) b
GROUP BY bucket, label_value
ORDER BY bucket, label_value;

-- name: histogram_rate_by_label
-- The per-second rate of observations of a cumulative histogram (e.g.
-- requests, from http.server.request.duration), per time bucket for each
-- value of a label.
-- params: schema, metric, label, from, to, bucket_ms
SELECT bucket AS time_unix, label_value, SUM(series_rate) AS per_second
FROM (
    SELECT TIME_BUCKET({{bucket_ms}}, time_unix) AS bucket, series_id, label_value,
           AVG(sample_rate) AS series_rate
    FROM (
        SELECT h.series_id, h.time_unix,
               JSON_VALUE(s.labels, '$["{{label}}"]') AS label_value,
               CAST(CASE WHEN LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix) IS NULL THEN NULL
                     WHEN h.data_count >= LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)
                     THEN h.data_count - LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)
                     ELSE h.data_count END AS DOUBLE) * 1000
               / (MSECS_SINCE_EPOCH(h.time_unix)
                  - LAG(MSECS_SINCE_EPOCH(h.time_unix)) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)) AS sample_rate
        FROM {{schema}}.metric_histogram_point h
        JOIN (SELECT series_id, labels FROM {{schema}}.metric_series
               WHERE metric_name = '{{metric}}' AND temporality = 2) s
          ON s.series_id = h.series_id
        WHERE h.time_unix BETWEEN '{{from}}' AND '{{to}}'
    ) d
    WHERE sample_rate IS NOT NULL
    GROUP BY TIME_BUCKET({{bucket_ms}}, time_unix), series_id, label_value
) b
GROUP BY bucket, label_value
ORDER BY bucket, label_value;

-- name: histogram_mean_by_label
-- The mean observed value of a cumulative histogram (e.g. the mean request
-- duration): the increase of its sum over the increase of its count, per
-- time bucket for each value of a label.
-- params: schema, metric, label, from, to, bucket_ms
SELECT bucket AS time_unix, label_value, SUM(sum_increase) / SUM(count_increase) AS mean
FROM (
    SELECT TIME_BUCKET({{bucket_ms}}, h.time_unix) AS bucket,
           JSON_VALUE(s.labels, '$["{{label}}"]') AS label_value,
           CASE WHEN LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix) IS NULL THEN NULL
                WHEN h.data_count >= LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)
                THEN h.data_sum - LAG(h.data_sum) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)
                ELSE h.data_sum END AS sum_increase,
           CASE WHEN LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix) IS NULL THEN NULL
                WHEN h.data_count >= LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)
                THEN h.data_count - LAG(h.data_count) OVER (PARTITION BY h.series_id ORDER BY h.time_unix)
                ELSE h.data_count END AS count_increase
    FROM {{schema}}.metric_histogram_point h
    JOIN (SELECT series_id, labels FROM {{schema}}.metric_series
           WHERE metric_name = '{{metric}}' AND temporality = 2) s
      ON s.series_id = h.series_id
    WHERE h.time_unix BETWEEN '{{from}}' AND '{{to}}'
) d
WHERE count_increase > 0
GROUP BY bucket, label_value
ORDER BY bucket, label_value;

-- name: histogram_buckets_delta
-- EXPERIMENTAL: the observations of a delta histogram over the time range,
-- per bucket. Each bucket's upper bound is null for the last (+Inf) bucket.
-- params: schema, metric, from, to
SELECT b.idx AS bucket_index,
       MAX(CAST(STRING(b.upper_bound) AS DOUBLE)) AS upper_bound,
       SUM(CAST(STRING(b.bucket_count) AS BIGINT)) AS observations
FROM (SELECT h.explicit_bounds, h.bucket_counts
        FROM {{schema}}.metric_histogram_point h
        JOIN (SELECT series_id FROM {{schema}}.metric_series
               WHERE metric_name = '{{metric}}' AND temporality = 1) s
          ON s.series_id = h.series_id
       WHERE h.time_unix BETWEEN '{{from}}' AND '{{to}}') hp,
     UNNEST_JSON_ARRAY(hp.explicit_bounds, hp.bucket_counts) WITH ORDINALITY b(upper_bound, bucket_count, idx)
GROUP BY b.idx
ORDER BY b.idx;

-- name: histogram_quantile_delta
-- EXPERIMENTAL: a quantile of a delta histogram over the time range, by
-- linear interpolation within the bucket holding it, as PromQL's
-- histogram_quantile does. A quantile in the +Inf bucket is that bucket's
-- lower bound.
-- params: schema, metric, from, to, quantile
SELECT CASE WHEN upper_bound IS NULL THEN lower_bound
            ELSE COALESCE(lower_bound, 0)
                 + (upper_bound - COALESCE(lower_bound, 0))
                   * (CAST({{quantile}} AS DOUBLE) * total - (cumulative - observations)) / observations
       END AS quantile_value
FROM (
    SELECT bucket_index, upper_bound, observations,
           LAG(upper_bound) OVER (ORDER BY bucket_index) AS lower_bound,
           SUM(observations) OVER (ORDER BY bucket_index ROWS UNBOUNDED PRECEDING) AS cumulative,
           SUM(observations) OVER () AS total
    FROM (
        SELECT b.idx AS bucket_index,
               MAX(CAST(STRING(b.upper_bound) AS DOUBLE)) AS upper_bound,
               SUM(CAST(STRING(b.bucket_count) AS BIGINT)) AS observations
        FROM (SELECT h.explicit_bounds, h.bucket_counts
                FROM {{schema}}.metric_histogram_point h
                JOIN (SELECT series_id FROM {{schema}}.metric_series
                       WHERE metric_name = '{{metric}}' AND temporality = 1) s
                  ON s.series_id = h.series_id
               WHERE h.time_unix BETWEEN '{{from}}' AND '{{to}}') hp,
             UNNEST_JSON_ARRAY(hp.explicit_bounds, hp.bucket_counts) WITH ORDINALITY b(upper_bound, bucket_count, idx)
        GROUP BY b.idx
    ) buckets
) c
WHERE observations > 0
  AND cumulative >= CAST({{quantile}} AS DOUBLE) * total
ORDER BY bucket_index
LIMIT 1;

-- name: exemplars_largest
-- The exemplars of a metric with the largest values, with their trace and
-- span IDs, to look up in the trace tables.
-- params: schema, metric, from, to, limit
SELECT e.time_unix, e.value, e.trace_id, e.span_id, s.service_name, s.labels
FROM {{schema}}.metric_exemplar e
JOIN (SELECT series_id, service_name, labels FROM {{schema}}.metric_series
       WHERE metric_name = '{{metric}}') s
  ON s.series_id = e.series_id
WHERE e.time_unix BETWEEN '{{from}}' AND '{{to}}'
ORDER BY e.value DESC
LIMIT {{limit}};

-- name: gauge_by_label_1m
-- As gauge_by_label, from the one-minute rollup (see rollups.sql), for long
-- time ranges; bucket_ms should be a multiple of 60000.
-- params: schema, metric, label, from, to, bucket_ms
SELECT bucket AS time_unix, label_value, AVG(series_value) AS value
FROM (
    SELECT TIME_BUCKET({{bucket_ms}}, r.time_unix) AS bucket,
           r.series_id,
           JSON_VALUE(s.labels, '$["{{label}}"]') AS label_value,
           SUM(r.sum_value) / SUM(r.samples) AS series_value
    FROM {{schema}}.metric_number_point_1m r
    JOIN (SELECT series_id, labels FROM {{schema}}.metric_series
           WHERE metric_name = '{{metric}}') s
      ON s.series_id = r.series_id
    WHERE r.time_unix BETWEEN '{{from}}' AND '{{to}}'
    GROUP BY TIME_BUCKET({{bucket_ms}}, r.time_unix), r.series_id, JSON_VALUE(s.labels, '$["{{label}}"]')
) b
GROUP BY bucket, label_value
ORDER BY bucket, label_value;
