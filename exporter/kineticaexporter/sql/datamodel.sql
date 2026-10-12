-- The tables the Kinetica exporter creates, in the schema otel: the same
-- statements it runs when it starts, with its configured schema in place of
-- otel. TestDatamodelSQL checks that this file matches them.

----- Logs

CREATE TABLE otel.log
(
        log_id                   VARCHAR (uuid),            -- generated
        trace_id                 VARCHAR(32),
        span_id                  VARCHAR(16),
        time_unix_nano           TIMESTAMP,
        observed_time_unix_nano  TIMESTAMP,
        severity_id              TINYINT,
        severity_text            VARCHAR(8),
        body                     VARCHAR,
        flags                    INT,
        PRIMARY KEY (log_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE otel.log_attribute
(
        log_id       VARCHAR (uuid),
        key          VARCHAR(256, dict),
        string_value VARCHAR(256),
        bool_value   BOOLEAN,
        int_value    INT,
        double_value DOUBLE,
        bytes_value  BYTES,
        PRIMARY KEY (log_id, key),
        SHARD KEY (log_id),
        FOREIGN KEY (log_id) REFERENCES otel.log(log_id) AS fk_log
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE otel.log_resource_attribute
(
        log_id  VARCHAR (uuid),              -- generated
        key          VARCHAR(256, dict),
        string_value VARCHAR,
        bool_value   BOOLEAN,
        int_value    INT,
        double_value DOUBLE,
        bytes_value  BYTES,
        PRIMARY KEY (log_id, key),
        SHARD KEY (log_id),
        FOREIGN KEY (log_id) REFERENCES otel.log(log_id) AS fk_log_resource
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE otel.log_scope_attribute
(
        log_id     VARCHAR (uuid),              -- generated
        scope_name   VARCHAR(64, dict),
        scope_ver    VARCHAR(16, dict),
        key          VARCHAR(256, dict),
        string_value VARCHAR,
        bool_value   BOOLEAN,
        int_value    INT,
        double_value DOUBLE,
        bytes_value  BYTES,
        PRIMARY KEY (log_id, key),
        SHARD KEY (log_id),
        FOREIGN KEY (log_id) REFERENCES otel.log(log_id) AS fk_log_scope
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

----- Traces

----- NEW

CREATE TABLE "otel"."trace_span"
(
    "id" UUID (primary_key) NOT NULL,
    "trace_id" VARCHAR (32) NOT NULL,
    "span_id" VARCHAR (16) NOT NULL,
    "parent_span_id" VARCHAR (16),
    "trace_state" VARCHAR (256),
    "name" VARCHAR (256, dict) NOT NULL,
    "span_kind" TINYINT (dict),
    "start_time_unix_nano" TIMESTAMP NOT NULL,
    "end_time_unix_nano" TIMESTAMP NOT NULL,
    "dropped_attributes_count" INTEGER,
    "dropped_events_count" INTEGER,
    "dropped_links_count" INTEGER,
    "message" VARCHAR(256),
    "status_code" TINYINT (dict)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE "otel"."trace_span_attribute"
(
    "span_id" UUID (primary_key, shard_key) NOT NULL,
    "key" VARCHAR (primary_key, 256, dict) NOT NULL,
    "string_value" VARCHAR (256),
    "bool_value" BOOLEAN,
    "int_value" INTEGER,
    "double_value" DOUBLE,
    "bytes_value" BLOB (store_only),
    FOREIGN KEY (span_id) references otel.trace_span(id) as fk_span
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE "otel"."trace_resource_attribute"
(
    span_id VARCHAR (UUID) NOT NULL,
    "key" VARCHAR (256, dict) NOT NULL,
    "string_value" VARCHAR (256),
    "bool_value" BOOLEAN,
    "int_value" INTEGER,
    "double_value" DOUBLE,
    "bytes_value" BLOB (store_only),
    PRIMARY KEY (span_id, key),
    SHARD KEY (span_id),
    FOREIGN KEY (span_id) references otel.trace_span(id) as fk_span_resource
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE "otel"."trace_scope_attribute"
(
    "span_id" UUID (primary_key) NOT NULL,
    "name" VARCHAR (256, dict),
    "version" VARCHAR (256, dict),
    "key" VARCHAR (primary_key, 128, dict) NOT NULL,
    "string_value" VARCHAR (256),
    "bool_value" BOOLEAN,
    "int_value" INTEGER,
    "double_value" DOUBLE,
    "bytes_value" BLOB (store_only),
    SHARD KEY (span_id),
    FOREIGN KEY (span_id) references otel.trace_span(id) as fk_span_scope

) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE "otel"."trace_event_attribute"
(
    "span_id" UUID (primary_key) NOT NULL,
    "event_name" VARCHAR (128, dict) NOT NULL,
    "time_unix_nano" TIMESTAMP,
    "key" VARCHAR (primary_key, 128) NOT NULL,
    "string_value" VARCHAR (256),
    "bool_value" BOOLEAN,
    "int_value" INTEGER,
    "double_value" DOUBLE,
    "bytes_value" BLOB (store_only),
    SHARD KEY (span_id),
    FOREIGN KEY (span_id) references otel.trace_span(id) as fk_span_event

) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

CREATE TABLE "otel"."trace_link_attribute"
(
    "link_span_id" UUID (primary_key) NOT NULL,
    "trace_id" VARCHAR (32),
    "span_id" VARCHAR (16),
    "key" VARCHAR (primary_key, 128, dict) NOT NULL,
    "string_value" VARCHAR (256),
    "bool_value" TINYINT,
    "int_value" INTEGER,
    "double_value" DOUBLE,
    "bytes_value" BLOB (store_only),
    SHARD KEY (link_span_id),
    FOREIGN KEY (link_span_id) references otel.trace_span(id) as fk_span_link

) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

------ METRICS
--
-- One row per time series in metric_series, keyed by series_id, a hash of
-- the series' name, type, unit, temporality and attributes; labels holds its
-- resource, scope and data point attributes as flat JSON (dotted keys kept,
-- values as strings), searchable with text_match. The data points of each metric type go to a narrow table
-- of their own. Every table is sharded on series_id, so the join of a series
-- to its points stays on each rank.
--
-- Needs a server with enable_text_search = true. The TEXT INDEX clause is the
-- exporter's default (metrics.labelsanalyzer: keyword); with an empty
-- analyzer it is left out. Rollups and retention: sql/metrics/rollups.sql.

CREATE TABLE otel.metric_series
(
    series_id LONG NOT NULL,
    metric_name VARCHAR (256, dict) NOT NULL,
    metric_type VARCHAR (16, dict) NOT NULL,
    unit VARCHAR (64, dict),
    description VARCHAR (512),
    temporality INT8,
    is_monotonic BOOLEAN,
    service_name VARCHAR (128, dict),
    scope_name VARCHAR (256, dict),
    scope_version VARCHAR (64, dict),
    labels JSON (text_search),
    resource_attributes JSON,
    scope_attributes JSON,
    attributes JSON,
    first_seen TIMESTAMP NOT NULL,
    PRIMARY KEY (series_id),
    SHARD KEY (series_id)
)
TEXT INDEX (labels) WITH OPTIONS (analyzer = 'keyword')
USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- Gauge and sum data points
CREATE TABLE otel.metric_number_point
(
    series_id LONG NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    start_time_unix TIMESTAMP,
    value DOUBLE NOT NULL,
    flags INT,
    PRIMARY KEY (series_id, time_unix),
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- Explicit bucket histogram data points; bucket_counts has one more value
-- than explicit_bounds, the last being the +Inf bucket
CREATE TABLE otel.metric_histogram_point
(
    series_id LONG NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    start_time_unix TIMESTAMP,
    data_count LONG NOT NULL,
    data_sum DOUBLE,
    data_min DOUBLE,
    data_max DOUBLE,
    explicit_bounds JSON,
    bucket_counts JSON,
    flags INT,
    PRIMARY KEY (series_id, time_unix),
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- Exponential histogram data points
CREATE TABLE otel.metric_exp_histogram_point
(
    series_id LONG NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    start_time_unix TIMESTAMP,
    data_count LONG NOT NULL,
    data_sum DOUBLE,
    data_min DOUBLE,
    data_max DOUBLE,
    scale INT NOT NULL,
    zero_count LONG,
    zero_threshold DOUBLE,
    positive_offset INT,
    positive_bucket_counts JSON,
    negative_offset INT,
    negative_bucket_counts JSON,
    flags INT,
    PRIMARY KEY (series_id, time_unix),
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- Summary data points; quantiles and quantile_values are parallel arrays
CREATE TABLE otel.metric_summary_point
(
    series_id LONG NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    start_time_unix TIMESTAMP,
    data_count LONG NOT NULL,
    data_sum DOUBLE,
    quantiles JSON,
    quantile_values JSON,
    flags INT,
    PRIMARY KEY (series_id, time_unix),
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);

-- The exemplars of every metric type; a data point may have several, so
-- there is no primary key
CREATE TABLE otel.metric_exemplar
(
    series_id LONG NOT NULL,
    point_time_unix TIMESTAMP NOT NULL,
    time_unix TIMESTAMP NOT NULL,
    value DOUBLE,
    trace_id VARCHAR (32),
    span_id VARCHAR (16),
    filtered_attributes JSON,
    SHARD KEY (series_id)
) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);
