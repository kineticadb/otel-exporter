// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

func TestMetricTableDDLsWithAnalyzer(t *testing.T) {
	ddls := metricTableDDLs("keyword")

	require.Len(t, ddls, 6)
	series := ddls[0]
	assert.Contains(t, series, "labels JSON (text_search)")
	assert.Contains(t, series, "TEXT INDEX (labels) WITH OPTIONS (analyzer = 'keyword')\n\tUSING TABLE PROPERTIES")
	assert.NotContains(t, series, textIndexPlaceholder)
}

func TestMetricTableDDLsWithoutAnalyzer(t *testing.T) {
	series := metricTableDDLs("")[0]

	assert.NotContains(t, series, "TEXT INDEX")
	assert.NotContains(t, series, textIndexPlaceholder)
	assert.Contains(t, series, "labels JSON (text_search)")
}

func TestMetricTableDDLsAreShardedOnTheSeries(t *testing.T) {
	tables := []string{
		MetricSeriesTable, MetricNumberPointTable, MetricHistogramPointTable,
		MetricExpHistogramPointTable, MetricSummaryPointTable, MetricExemplarTable,
	}
	for i, ddl := range metricTableDDLs("keyword") {
		stmt := strings.ReplaceAll(ddl, "%s", "otel.")
		assert.Contains(t, stmt, "CREATE TABLE otel."+tables[i]+"\n")
		assert.Contains(t, stmt, "SHARD KEY (series_id)")
		assert.Contains(t, stmt, "NO_ERROR_IF_EXISTS = TRUE")
	}
}

func TestNumberDataPointValue(t *testing.T) {
	dp := pmetric.NewNumberDataPoint()
	dp.SetIntValue(7)
	assert.Equal(t, float64(7), numberDataPointValue(dp))
	dp.SetDoubleValue(2.5)
	assert.Equal(t, 2.5, numberDataPointValue(dp))

	exemplar := pmetric.NewExemplar()
	exemplar.SetIntValue(9)
	assert.Equal(t, float64(9), exemplarValue(exemplar))
	exemplar.SetDoubleValue(0.5)
	assert.Equal(t, 0.5, exemplarValue(exemplar))
}

func TestUnixMilli(t *testing.T) {
	assert.Equal(t, testTime.UnixMilli(), unixMilli(pcommon.NewTimestampFromTime(testTime)))
	assert.Nil(t, optionalUnixMilli(0))
}
