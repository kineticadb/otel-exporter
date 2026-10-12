// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

var testTime = time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)

// newTestMetrics - one resource and scope with a metric of every type: a
// gauge with two int points of one series, a cumulative monotonic sum, a
// histogram with an exemplar, an exponential histogram and a summary
func newTestMetrics() pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	rm.Resource().Attributes().PutStr("host.name", "h1")
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("otelhttp")
	sm.Scope().SetVersion("1.0")
	sm.Scope().Attributes().PutStr("scope.attr", "s")

	gauge := sm.Metrics().AppendEmpty()
	gauge.SetName("http.server.active_requests")
	gauge.SetUnit("{request}")
	gaugePoints := gauge.SetEmptyGauge().DataPoints()
	for i, v := range []int64{42, 43} {
		dp := gaugePoints.AppendEmpty()
		dp.SetIntValue(v)
		dp.SetTimestamp(pcommon.NewTimestampFromTime(testTime.Add(time.Duration(i) * time.Minute)))
		dp.Attributes().PutStr("http.request.method", "GET")
	}

	sum := sm.Metrics().AppendEmpty()
	sum.SetName("http.server.requests")
	sum.SetDescription("Requests served")
	s := sum.SetEmptySum()
	s.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	s.SetIsMonotonic(true)
	sdp := s.DataPoints().AppendEmpty()
	sdp.SetDoubleValue(10.5)
	sdp.SetStartTimestamp(pcommon.NewTimestampFromTime(testTime.Add(-time.Hour)))
	sdp.SetTimestamp(pcommon.NewTimestampFromTime(testTime))
	sdp.Attributes().PutInt("http.response.status_code", 200)

	histogram := sm.Metrics().AppendEmpty()
	histogram.SetName("http.server.request.duration")
	histogram.SetUnit("s")
	h := histogram.SetEmptyHistogram()
	h.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	hdp := h.DataPoints().AppendEmpty()
	hdp.SetTimestamp(pcommon.NewTimestampFromTime(testTime))
	hdp.SetCount(10)
	hdp.SetSum(4.5)
	hdp.SetMin(0.01)
	hdp.ExplicitBounds().FromRaw([]float64{0.1, 0.5, 1})
	hdp.BucketCounts().FromRaw([]uint64{3, 4, 2, 1})
	exemplar := hdp.Exemplars().AppendEmpty()
	exemplar.SetIntValue(2)
	exemplar.SetTimestamp(pcommon.NewTimestampFromTime(testTime.Add(-time.Second)))
	exemplar.SetTraceID(pcommon.TraceID([16]byte{0x0a, 0x0b, 15: 0x01}))
	exemplar.SetSpanID(pcommon.SpanID([8]byte{0x01, 7: 0x02}))
	exemplar.FilteredAttributes().PutStr("user.id", "u-1")

	expHistogram := sm.Metrics().AppendEmpty()
	expHistogram.SetName("rpc.duration")
	e := expHistogram.SetEmptyExponentialHistogram()
	e.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	edp := e.DataPoints().AppendEmpty()
	edp.SetTimestamp(pcommon.NewTimestampFromTime(testTime))
	edp.SetCount(6)
	edp.SetScale(2)
	edp.SetZeroCount(1)
	edp.Positive().SetOffset(3)
	edp.Positive().BucketCounts().FromRaw([]uint64{2, 3})
	edp.Negative().SetOffset(-1)

	summary := sm.Metrics().AppendEmpty()
	summary.SetName("gc.pause")
	sumdp := summary.SetEmptySummary().DataPoints().AppendEmpty()
	sumdp.SetTimestamp(pcommon.NewTimestampFromTime(testTime))
	sumdp.SetCount(4)
	sumdp.SetSum(8)
	for _, q := range [][2]float64{{0.5, 1.5}, {0.99, 3.25}} {
		qv := sumdp.QuantileValues().AppendEmpty()
		qv.SetQuantile(q[0])
		qv.SetValue(q[1])
	}

	return md
}

func newTestBuilder(known func(int64) bool) *metricsV2BatchBuilder {
	return &metricsV2BatchBuilder{enc: newAttributeEncoder(nil), known: known}
}

func TestBuildWritesOneSeriesRowPerSeries(t *testing.T) {
	batch, err := newTestBuilder(nil).build(newTestMetrics())
	require.NoError(t, err)

	require.Len(t, batch.series, 5)
	types := map[string]string{}
	for _, s := range batch.series {
		types[s.MetricName] = s.MetricType
	}
	assert.Equal(t, map[string]string{
		"http.server.active_requests":  metricTypeGauge,
		"http.server.requests":         metricTypeSum,
		"http.server.request.duration": metricTypeHistogram,
		"rpc.duration":                 metricTypeExpHistogram,
		"gc.pause":                     metricTypeSummary,
	}, types)
}

func TestBuildSeriesRow(t *testing.T) {
	batch, err := newTestBuilder(nil).build(newTestMetrics())
	require.NoError(t, err)

	sum := batch.series[1]
	assert.Equal(t, "http.server.requests", sum.MetricName)
	assert.Equal(t, int8(pmetric.AggregationTemporalityCumulative), sum.Temporality)
	assert.True(t, sum.IsMonotonic)
	require.NotNil(t, sum.ServiceName)
	assert.Equal(t, "checkout", *sum.ServiceName)
	require.NotNil(t, sum.Description)
	assert.Equal(t, "Requests served", *sum.Description)
	assert.Nil(t, sum.Unit)
	assert.Equal(t, `{"host.name":"h1","service.name":"checkout"}`, sum.ResourceAttributes)
	assert.Equal(t, `{"scope.attr":"s"}`, sum.ScopeAttributes)
	assert.Equal(t, `{"http.response.status_code":200}`, sum.Attributes)
	assert.Equal(t, testTime.UnixMilli(), sum.FirstSeen)

	var labels map[string]string
	require.NoError(t, json.Unmarshal([]byte(sum.Labels), &labels))
	assert.Equal(t, map[string]string{
		"service.name":              "checkout",
		"host.name":                 "h1",
		"scope.attr":                "s",
		"otel.scope.name":           "otelhttp",
		"otel.scope.version":        "1.0",
		"http.response.status_code": "200",
	}, labels)
}

func TestBuildNumberPointsKeepIntValues(t *testing.T) {
	batch, err := newTestBuilder(nil).build(newTestMetrics())
	require.NoError(t, err)

	require.Len(t, batch.numberPoints, 3)
	gaugeSeries := batch.series[0].SeriesID
	assert.Equal(t, numberPointRow{SeriesID: gaugeSeries, TimeUnix: testTime.UnixMilli(), Value: 42}, batch.numberPoints[0])
	assert.Equal(t, gaugeSeries, batch.numberPoints[1].SeriesID)
	assert.Equal(t, float64(43), batch.numberPoints[1].Value)

	sum := batch.numberPoints[2]
	assert.Equal(t, 10.5, sum.Value)
	require.NotNil(t, sum.StartTimeUnix)
	assert.Equal(t, testTime.Add(-time.Hour).UnixMilli(), *sum.StartTimeUnix)
}

func TestBuildHistogramPoint(t *testing.T) {
	batch, err := newTestBuilder(nil).build(newTestMetrics())
	require.NoError(t, err)

	require.Len(t, batch.histogramPoints, 1)
	point := batch.histogramPoints[0]
	assert.Equal(t, int64(10), point.DataCount)
	assert.Equal(t, 4.5, *point.DataSum)
	assert.Equal(t, 0.01, *point.DataMin)
	assert.Nil(t, point.DataMax)
	assert.Nil(t, point.StartTimeUnix)
	assert.Equal(t, "[0.1,0.5,1]", point.ExplicitBounds)
	assert.Equal(t, "[3,4,2,1]", point.BucketCounts)
}

func TestBuildExemplar(t *testing.T) {
	batch, err := newTestBuilder(nil).build(newTestMetrics())
	require.NoError(t, err)

	require.Len(t, batch.exemplars, 1)
	exemplar := batch.exemplars[0]
	assert.Equal(t, batch.series[2].SeriesID, exemplar.SeriesID)
	assert.Equal(t, testTime.UnixMilli(), exemplar.PointTimeUnix)
	assert.Equal(t, testTime.Add(-time.Second).UnixMilli(), exemplar.TimeUnix)
	assert.Equal(t, float64(2), exemplar.Value)
	assert.Equal(t, "0a0b0000000000000000000000000001", *exemplar.TraceID)
	assert.Equal(t, "0100000000000002", *exemplar.SpanID)
	assert.Equal(t, `{"user.id":"u-1"}`, exemplar.FilteredAttributes)
}

func TestBuildExpHistogramAndSummaryPoints(t *testing.T) {
	batch, err := newTestBuilder(nil).build(newTestMetrics())
	require.NoError(t, err)

	require.Len(t, batch.expHistogramPoints, 1)
	exp := batch.expHistogramPoints[0]
	assert.Equal(t, int32(2), exp.Scale)
	assert.Equal(t, int64(1), exp.ZeroCount)
	assert.Equal(t, int32(3), exp.PositiveOffset)
	assert.Equal(t, "[2,3]", exp.PositiveBucketCounts)
	assert.Equal(t, int32(-1), exp.NegativeOffset)
	assert.Equal(t, "[]", exp.NegativeBucketCounts)
	assert.Nil(t, exp.DataSum)

	require.Len(t, batch.summaryPoints, 1)
	summary := batch.summaryPoints[0]
	assert.Equal(t, int64(4), summary.DataCount)
	assert.Equal(t, float64(8), summary.DataSum)
	assert.Equal(t, "[0.5,0.99]", summary.Quantiles)
	assert.Equal(t, "[1.5,3.25]", summary.QuantileValues)
}

func TestBuildLeavesOutKnownSeries(t *testing.T) {
	batch, err := newTestBuilder(func(int64) bool { return true }).build(newTestMetrics())
	require.NoError(t, err)

	assert.Empty(t, batch.series)
	assert.Len(t, batch.numberPoints, 3)
	assert.Len(t, batch.histogramPoints, 1)
}

func TestBuildDistinguishesSeriesByAttributes(t *testing.T) {
	md := pmetric.NewMetrics()
	gauge := md.ResourceMetrics().AppendEmpty().ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	gauge.SetName("queue.size")
	points := gauge.SetEmptyGauge().DataPoints()
	for _, queue := range []string{"a", "b", "a"} {
		dp := points.AppendEmpty()
		dp.SetIntValue(1)
		dp.Attributes().PutStr("queue", queue)
	}

	batch, err := newTestBuilder(nil).build(md)
	require.NoError(t, err)

	require.Len(t, batch.series, 2)
	require.Len(t, batch.numberPoints, 3)
	assert.Equal(t, batch.numberPoints[0].SeriesID, batch.numberPoints[2].SeriesID)
	assert.NotEqual(t, batch.numberPoints[0].SeriesID, batch.numberPoints[1].SeriesID)
	assert.Nil(t, batch.series[0].ServiceName)
}

func TestFloatArrayJSONNamesNonFiniteValues(t *testing.T) {
	assert.Equal(t, `[1,"+Inf","NaN"]`, floatArrayJSON([]float64{1, math.Inf(1), math.NaN()}))
}
