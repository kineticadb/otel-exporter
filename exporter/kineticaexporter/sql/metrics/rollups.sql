-- Rollups and retention for the metric tables
--
-- Run these once, in order, after the exporter has created its tables. They
-- use the same template format as queries.sql: a "-- name:" line, comment
-- lines, a "-- params:" line, and {{name}} placeholders:
--
--   schema          the schema the exporter writes to, e.g. otel
--   retention_days  the number of days of points to keep
--
-- The rollups aggregate metric_number_point (gauges and sums) per series:
-- one row per minute in metric_number_point_1m, and one per hour in
-- metric_number_point_1h. Each keeps the minimum, maximum, sum and number of
-- samples, so averages compose across buckets (sum_value / samples), and the
-- maximum of a monotonic counter is its last value in the bucket unless it
-- was reset.
--
-- A SQL procedure refreshes each rollup on a schedule, re-aggregating a few
-- recent buckets so that late points are included; its upsert replaces the
-- rows it recomputes. The lower bound of the re-aggregated range is aligned
-- to a bucket, so a bucket is never replaced by a partial one.

-- name: create_metric_number_point_1m
-- One row per series and minute.
-- params: schema
CREATE TABLE {{schema}}.metric_number_point_1m
(
    series_id LONG NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    min_value DOUBLE,
    max_value DOUBLE,
    sum_value DOUBLE,
    samples LONG,
    PRIMARY KEY (series_id, time_unix),
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- name: create_rollup_metric_number_point_1m
-- Every minute, re-aggregates the last ten minutes into the one-minute
-- rollup.
-- params: schema
CREATE OR REPLACE PROCEDURE {{schema}}.rollup_metric_number_point_1m
BEGIN
    INSERT INTO {{schema}}.metric_number_point_1m /* KI_HINT_UPDATE_ON_EXISTING_PK */
    SELECT series_id,
           TIME_BUCKET(60000, time_unix) AS time_unix,
           MIN(value) AS min_value,
           MAX(value) AS max_value,
           SUM(value) AS sum_value,
           COUNT(*) AS samples
    FROM {{schema}}.metric_number_point
    WHERE time_unix >= TIME_BUCKET(60000, DATEADD(MINUTE, -10, NOW()))
    GROUP BY series_id, TIME_BUCKET(60000, time_unix);
END
EXECUTE FOR EVERY 1 MINUTE;

-- name: create_metric_number_point_1h
-- One row per series and hour.
-- params: schema
CREATE TABLE {{schema}}.metric_number_point_1h
(
    series_id LONG NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    min_value DOUBLE,
    max_value DOUBLE,
    sum_value DOUBLE,
    samples LONG,
    PRIMARY KEY (series_id, time_unix),
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- name: create_rollup_metric_number_point_1h
-- Every ten minutes, re-aggregates the last three hours of the one-minute
-- rollup into the hourly rollup.
-- params: schema
CREATE OR REPLACE PROCEDURE {{schema}}.rollup_metric_number_point_1h
BEGIN
    INSERT INTO {{schema}}.metric_number_point_1h /* KI_HINT_UPDATE_ON_EXISTING_PK */
    SELECT series_id,
           TIME_BUCKET(3600000, time_unix) AS time_unix,
           MIN(min_value) AS min_value,
           MAX(max_value) AS max_value,
           SUM(sum_value) AS sum_value,
           SUM(samples) AS samples
    FROM {{schema}}.metric_number_point_1m
    WHERE time_unix >= TIME_BUCKET(3600000, DATEADD(HOUR, -3, NOW()))
    GROUP BY series_id, TIME_BUCKET(3600000, time_unix);
END
EXECUTE FOR EVERY 10 MINUTES;

-- name: create_retention
-- OPTIONAL: every hour, deletes the points, exemplars and one-minute rollups
-- older than the retention period. The hourly rollup is kept. Series rows
-- are kept too: a series that stops reporting keeps its labels.
-- params: schema, retention_days
CREATE OR REPLACE PROCEDURE {{schema}}.metrics_retention
BEGIN
    DELETE FROM {{schema}}.metric_number_point WHERE time_unix < DATEADD(DAY, -{{retention_days}}, NOW());
    DELETE FROM {{schema}}.metric_histogram_point WHERE time_unix < DATEADD(DAY, -{{retention_days}}, NOW());
    DELETE FROM {{schema}}.metric_exp_histogram_point WHERE time_unix < DATEADD(DAY, -{{retention_days}}, NOW());
    DELETE FROM {{schema}}.metric_summary_point WHERE time_unix < DATEADD(DAY, -{{retention_days}}, NOW());
    DELETE FROM {{schema}}.metric_exemplar WHERE time_unix < DATEADD(DAY, -{{retention_days}}, NOW());
    DELETE FROM {{schema}}.metric_number_point_1m WHERE time_unix < DATEADD(DAY, -{{retention_days}}, NOW());
END
EXECUTE FOR EVERY 1 HOUR;
