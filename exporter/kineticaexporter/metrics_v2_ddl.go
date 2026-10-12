// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"fmt"
	"strings"
)

// v2 metrics tables: one row per series in metric_series, with its labels as
// flat, text-searchable JSON, and the data points of each metric type in a
// narrow table of its own. Every table is sharded on series_id, so the join
// of a series to its points stays on each rank.
const (
	MetricSeriesTable            = "metric_series"
	MetricNumberPointTable       = "metric_number_point"
	MetricHistogramPointTable    = "metric_histogram_point"
	MetricExpHistogramPointTable = "metric_exp_histogram_point"
	MetricSummaryPointTable      = "metric_summary_point"
	MetricExemplarTable          = "metric_exemplar"
)

// textIndexPlaceholder - replaced in CreateMetricSeries by the TEXT INDEX
// clause of the labels column, or by nothing
const textIndexPlaceholder = "$TEXT_INDEX$"

const (
	// CreateMetricSeries - one row per time series. labels is the union of the
	// resource, scope and data point attributes, flat, with dotted keys kept;
	// the other JSON columns keep each level's attributes as they were.
	CreateMetricSeries string = `CREATE TABLE %smetric_series
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
	$TEXT_INDEX$USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);
	`

	// CreateMetricNumberPoint - the data points of gauges and sums
	CreateMetricNumberPoint string = `CREATE TABLE %smetric_number_point
	(
		series_id LONG NOT NULL,
		time_unix TIMESTAMP NOT NULL,
		start_time_unix TIMESTAMP,
		value DOUBLE NOT NULL,
		flags INT,
		PRIMARY KEY (series_id, time_unix),
		SHARD KEY (series_id)
	) USING TABLE PROPERTIES (NO_ERROR_IF_EXISTS = TRUE);
	`

	// CreateMetricHistogramPoint - explicit bucket histogram data points;
	// bucket_counts has one more value than explicit_bounds, the last being
	// the +Inf bucket
	CreateMetricHistogramPoint string = `CREATE TABLE %smetric_histogram_point
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
	`

	// CreateMetricExpHistogramPoint - exponential histogram data points
	CreateMetricExpHistogramPoint string = `CREATE TABLE %smetric_exp_histogram_point
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
	`

	// CreateMetricSummaryPoint - summary data points; quantiles and
	// quantile_values are parallel arrays
	CreateMetricSummaryPoint string = `CREATE TABLE %smetric_summary_point
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
	`

	// CreateMetricExemplar - the exemplars of every metric type; a data point
	// may have several, so there is no primary key
	CreateMetricExemplar string = `CREATE TABLE %smetric_exemplar
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
	`
)

// metricsV2TableDDLs - the statements creating the v2 metrics tables, the
// schema placeholder left in place for createTablesForMetricType
//
//	@param labelsAnalyzer - the analyzer of the labels column's text index;
//	empty gives the column the server's default analyzer, with no TEXT INDEX
//	clause
//	@return []string
func metricsV2TableDDLs(labelsAnalyzer string) []string {
	textIndex := ""
	if labelsAnalyzer != "" {
		textIndex = fmt.Sprintf("TEXT INDEX (labels) WITH OPTIONS (analyzer = '%s')\n\t", labelsAnalyzer)
	}
	return []string{
		strings.Replace(CreateMetricSeries, textIndexPlaceholder, textIndex, 1),
		CreateMetricNumberPoint,
		CreateMetricHistogramPoint,
		CreateMetricExpHistogramPoint,
		CreateMetricSummaryPoint,
		CreateMetricExemplar,
	}
}
