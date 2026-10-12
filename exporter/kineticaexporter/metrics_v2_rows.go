// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/multierr"
)

// metric_type values of the v2 series table
const (
	metricTypeGauge        = "gauge"
	metricTypeSum          = "sum"
	metricTypeHistogram    = "histogram"
	metricTypeExpHistogram = "exp_histogram"
	metricTypeSummary      = "summary"
)

// Rows of the v2 metrics tables. Each field is written to the column its tag
// names; a nil pointer is written as null.

type seriesRow struct {
	SeriesID           int64   `kinetica:"series_id"`
	MetricName         string  `kinetica:"metric_name"`
	MetricType         string  `kinetica:"metric_type"`
	Unit               *string `kinetica:"unit"`
	Description        *string `kinetica:"description"`
	Temporality        int8    `kinetica:"temporality"`
	IsMonotonic        bool    `kinetica:"is_monotonic"`
	ServiceName        *string `kinetica:"service_name"`
	ScopeName          *string `kinetica:"scope_name"`
	ScopeVersion       *string `kinetica:"scope_version"`
	Labels             string  `kinetica:"labels"`
	ResourceAttributes string  `kinetica:"resource_attributes"`
	ScopeAttributes    string  `kinetica:"scope_attributes"`
	Attributes         string  `kinetica:"attributes"`
	FirstSeen          int64   `kinetica:"first_seen"`
}

type numberPointRow struct {
	SeriesID      int64   `kinetica:"series_id"`
	TimeUnix      int64   `kinetica:"time_unix"`
	StartTimeUnix *int64  `kinetica:"start_time_unix"`
	Value         float64 `kinetica:"value"`
	Flags         int32   `kinetica:"flags"`
}

type histogramPointRow struct {
	SeriesID       int64    `kinetica:"series_id"`
	TimeUnix       int64    `kinetica:"time_unix"`
	StartTimeUnix  *int64   `kinetica:"start_time_unix"`
	DataCount      int64    `kinetica:"data_count"`
	DataSum        *float64 `kinetica:"data_sum"`
	DataMin        *float64 `kinetica:"data_min"`
	DataMax        *float64 `kinetica:"data_max"`
	ExplicitBounds string   `kinetica:"explicit_bounds"`
	BucketCounts   string   `kinetica:"bucket_counts"`
	Flags          int32    `kinetica:"flags"`
}

type expHistogramPointRow struct {
	SeriesID             int64    `kinetica:"series_id"`
	TimeUnix             int64    `kinetica:"time_unix"`
	StartTimeUnix        *int64   `kinetica:"start_time_unix"`
	DataCount            int64    `kinetica:"data_count"`
	DataSum              *float64 `kinetica:"data_sum"`
	DataMin              *float64 `kinetica:"data_min"`
	DataMax              *float64 `kinetica:"data_max"`
	Scale                int32    `kinetica:"scale"`
	ZeroCount            int64    `kinetica:"zero_count"`
	ZeroThreshold        *float64 `kinetica:"zero_threshold"`
	PositiveOffset       int32    `kinetica:"positive_offset"`
	PositiveBucketCounts string   `kinetica:"positive_bucket_counts"`
	NegativeOffset       int32    `kinetica:"negative_offset"`
	NegativeBucketCounts string   `kinetica:"negative_bucket_counts"`
	Flags                int32    `kinetica:"flags"`
}

type summaryPointRow struct {
	SeriesID       int64   `kinetica:"series_id"`
	TimeUnix       int64   `kinetica:"time_unix"`
	StartTimeUnix  *int64  `kinetica:"start_time_unix"`
	DataCount      int64   `kinetica:"data_count"`
	DataSum        float64 `kinetica:"data_sum"`
	Quantiles      string  `kinetica:"quantiles"`
	QuantileValues string  `kinetica:"quantile_values"`
	Flags          int32   `kinetica:"flags"`
}

type exemplarRow struct {
	SeriesID           int64   `kinetica:"series_id"`
	PointTimeUnix      int64   `kinetica:"point_time_unix"`
	TimeUnix           int64   `kinetica:"time_unix"`
	Value              float64 `kinetica:"value"`
	TraceID            *string `kinetica:"trace_id"`
	SpanID             *string `kinetica:"span_id"`
	FilteredAttributes string  `kinetica:"filtered_attributes"`
}

// metricsV2Batch - the rows of one export, by table
type metricsV2Batch struct {
	series             []seriesRow
	numberPoints       []numberPointRow
	histogramPoints    []histogramPointRow
	expHistogramPoints []expHistogramPointRow
	summaryPoints      []summaryPointRow
	exemplars          []exemplarRow
}

// metricsV2BatchBuilder - turns [pmetric.Metrics] into the rows of the v2
// tables
type metricsV2BatchBuilder struct {
	enc *attributeEncoder
	// known reports a series already written, whose row is not built again;
	// nil knows none
	known func(seriesID int64) bool
}

// scopeContext - what the series of one resource and scope share
type scopeContext struct {
	resourceAttributes string
	scopeAttributes    string
	scopeName          string
	scopeVersion       string
	serviceName        *string
	labels             map[string]any
}

// metricContext - what the series of one metric share
type metricContext struct {
	*scopeContext
	name        string
	metricType  string
	unit        string
	description string
	temporality int8
	isMonotonic bool
}

// batchState - the batch being built, and the series it already has
type batchState struct {
	batch *metricsV2Batch
	seen  map[int64]struct{}
	errs  error
}

// build - the rows of an export. Series are built once each, and not at all
// when already known. A data point whose attributes cannot be encoded is
// left out and reported in the error, the rest of the batch still built.
//
//	@receiver b
//	@param md
//	@return *metricsV2Batch
//	@return error
func (b *metricsV2BatchBuilder) build(md pmetric.Metrics) (*metricsV2Batch, error) {
	state := &batchState{batch: &metricsV2Batch{}, seen: map[int64]struct{}{}}

	for i := 0; i < md.ResourceMetrics().Len(); i++ {
		resourceMetrics := md.ResourceMetrics().At(i)
		resource := resourceMetrics.Resource().Attributes()
		resourceJSON, err := b.enc.canonicalJSON(resource)
		if err != nil {
			state.errs = multierr.Append(state.errs, fmt.Errorf("resource attributes: %w", err))
			continue
		}
		resourceLabels := map[string]any{}
		b.enc.labels(resource, resourceLabels)

		var serviceName *string
		if v, ok := resource.Get(serviceNameKey); ok && v.Type() == pcommon.ValueTypeStr && v.Str() != "" {
			name := v.Str()
			serviceName = &name
		}

		for j := 0; j < resourceMetrics.ScopeMetrics().Len(); j++ {
			scopeMetrics := resourceMetrics.ScopeMetrics().At(j)
			scope := scopeMetrics.Scope()
			scopeJSON, err := b.enc.canonicalJSON(scope.Attributes())
			if err != nil {
				state.errs = multierr.Append(state.errs, fmt.Errorf("scope attributes: %w", err))
				continue
			}
			labels := make(map[string]any, len(resourceLabels)+scope.Attributes().Len()+2)
			for k, v := range resourceLabels {
				labels[k] = v
			}
			b.enc.labels(scope.Attributes(), labels)
			if scope.Name() != "" && !b.enc.excluded(scopeNameLabel) {
				labels[scopeNameLabel] = scope.Name()
			}
			if scope.Version() != "" && !b.enc.excluded(scopeVersionLabel) {
				labels[scopeVersionLabel] = scope.Version()
			}
			sc := &scopeContext{
				resourceAttributes: resourceJSON,
				scopeAttributes:    scopeJSON,
				scopeName:          scope.Name(),
				scopeVersion:       scope.Version(),
				serviceName:        serviceName,
				labels:             labels,
			}

			for k := 0; k < scopeMetrics.Metrics().Len(); k++ {
				b.addMetric(state, sc, scopeMetrics.Metrics().At(k))
			}
		}
	}
	return state.batch, state.errs
}

// addMetric - adds the rows of a metric's data points
func (b *metricsV2BatchBuilder) addMetric(state *batchState, sc *scopeContext, metric pmetric.Metric) {
	mc := &metricContext{
		scopeContext: sc,
		name:         metric.Name(),
		unit:         metric.Unit(),
		description:  metric.Description(),
	}
	batch := state.batch

	switch metric.Type() {
	case pmetric.MetricTypeGauge:
		mc.metricType = metricTypeGauge
		points := metric.Gauge().DataPoints()
		for i := 0; i < points.Len(); i++ {
			b.addNumberPoint(state, mc, points.At(i))
		}
	case pmetric.MetricTypeSum:
		sum := metric.Sum()
		mc.metricType = metricTypeSum
		mc.temporality = int8(sum.AggregationTemporality())
		mc.isMonotonic = sum.IsMonotonic()
		points := sum.DataPoints()
		for i := 0; i < points.Len(); i++ {
			b.addNumberPoint(state, mc, points.At(i))
		}
	case pmetric.MetricTypeHistogram:
		histogram := metric.Histogram()
		mc.metricType = metricTypeHistogram
		mc.temporality = int8(histogram.AggregationTemporality())
		points := histogram.DataPoints()
		for i := 0; i < points.Len(); i++ {
			dp := points.At(i)
			seriesID, ok := b.seriesFor(state, mc, dp.Attributes(), dp.Timestamp())
			if !ok {
				continue
			}
			batch.histogramPoints = append(batch.histogramPoints, histogramPointRow{
				SeriesID:       seriesID,
				TimeUnix:       unixMilli(dp.Timestamp()),
				StartTimeUnix:  optionalUnixMilli(dp.StartTimestamp()),
				DataCount:      int64(dp.Count()),
				DataSum:        optionalFloat(dp.HasSum(), dp.Sum),
				DataMin:        optionalFloat(dp.HasMin(), dp.Min),
				DataMax:        optionalFloat(dp.HasMax(), dp.Max),
				ExplicitBounds: floatArrayJSON(dp.ExplicitBounds().AsRaw()),
				BucketCounts:   uintArrayJSON(dp.BucketCounts().AsRaw()),
				Flags:          int32(dp.Flags()),
			})
			b.addExemplars(state, seriesID, dp.Timestamp(), dp.Exemplars())
		}
	case pmetric.MetricTypeExponentialHistogram:
		histogram := metric.ExponentialHistogram()
		mc.metricType = metricTypeExpHistogram
		mc.temporality = int8(histogram.AggregationTemporality())
		points := histogram.DataPoints()
		for i := 0; i < points.Len(); i++ {
			dp := points.At(i)
			seriesID, ok := b.seriesFor(state, mc, dp.Attributes(), dp.Timestamp())
			if !ok {
				continue
			}
			batch.expHistogramPoints = append(batch.expHistogramPoints, expHistogramPointRow{
				SeriesID:      seriesID,
				TimeUnix:      unixMilli(dp.Timestamp()),
				StartTimeUnix: optionalUnixMilli(dp.StartTimestamp()),
				DataCount:     int64(dp.Count()),
				DataSum:       optionalFloat(dp.HasSum(), dp.Sum),
				DataMin:       optionalFloat(dp.HasMin(), dp.Min),
				DataMax:       optionalFloat(dp.HasMax(), dp.Max),
				Scale:         dp.Scale(),
				ZeroCount:     int64(dp.ZeroCount()),
				// pdata v1.0.0-rcv0014 has no zero threshold; null until it does
				ZeroThreshold:        nil,
				PositiveOffset:       dp.Positive().Offset(),
				PositiveBucketCounts: uintArrayJSON(dp.Positive().BucketCounts().AsRaw()),
				NegativeOffset:       dp.Negative().Offset(),
				NegativeBucketCounts: uintArrayJSON(dp.Negative().BucketCounts().AsRaw()),
				Flags:                int32(dp.Flags()),
			})
			b.addExemplars(state, seriesID, dp.Timestamp(), dp.Exemplars())
		}
	case pmetric.MetricTypeSummary:
		mc.metricType = metricTypeSummary
		points := metric.Summary().DataPoints()
		for i := 0; i < points.Len(); i++ {
			dp := points.At(i)
			seriesID, ok := b.seriesFor(state, mc, dp.Attributes(), dp.Timestamp())
			if !ok {
				continue
			}
			quantiles := make([]float64, dp.QuantileValues().Len())
			values := make([]float64, dp.QuantileValues().Len())
			for n := range quantiles {
				qv := dp.QuantileValues().At(n)
				quantiles[n] = qv.Quantile()
				values[n] = qv.Value()
			}
			batch.summaryPoints = append(batch.summaryPoints, summaryPointRow{
				SeriesID:       seriesID,
				TimeUnix:       unixMilli(dp.Timestamp()),
				StartTimeUnix:  optionalUnixMilli(dp.StartTimestamp()),
				DataCount:      int64(dp.Count()),
				DataSum:        dp.Sum(),
				Quantiles:      floatArrayJSON(quantiles),
				QuantileValues: floatArrayJSON(values),
				Flags:          int32(dp.Flags()),
			})
		}
	}
}

// addNumberPoint - adds a gauge or sum data point
func (b *metricsV2BatchBuilder) addNumberPoint(state *batchState, mc *metricContext, dp pmetric.NumberDataPoint) {
	seriesID, ok := b.seriesFor(state, mc, dp.Attributes(), dp.Timestamp())
	if !ok {
		return
	}
	state.batch.numberPoints = append(state.batch.numberPoints, numberPointRow{
		SeriesID:      seriesID,
		TimeUnix:      unixMilli(dp.Timestamp()),
		StartTimeUnix: optionalUnixMilli(dp.StartTimestamp()),
		Value:         numberDataPointValue(dp),
		Flags:         int32(dp.Flags()),
	})
	b.addExemplars(state, seriesID, dp.Timestamp(), dp.Exemplars())
}

// addExemplars - adds a data point's exemplars
func (b *metricsV2BatchBuilder) addExemplars(state *batchState, seriesID int64, pointTime pcommon.Timestamp, exemplars pmetric.ExemplarSlice) {
	for i := 0; i < exemplars.Len(); i++ {
		exemplar := exemplars.At(i)
		filtered, err := b.enc.canonicalJSON(exemplar.FilteredAttributes())
		if err != nil {
			state.errs = multierr.Append(state.errs, fmt.Errorf("exemplar attributes: %w", err))
			continue
		}
		row := exemplarRow{
			SeriesID:           seriesID,
			PointTimeUnix:      unixMilli(pointTime),
			TimeUnix:           unixMilli(exemplar.Timestamp()),
			Value:              exemplarValue(exemplar),
			FilteredAttributes: filtered,
		}
		if traceID := exemplar.TraceID(); !traceID.IsEmpty() {
			id := hex.EncodeToString(traceID[:])
			row.TraceID = &id
		}
		if spanID := exemplar.SpanID(); !spanID.IsEmpty() {
			id := hex.EncodeToString(spanID[:])
			row.SpanID = &id
		}
		state.batch.exemplars = append(state.batch.exemplars, row)
	}
}

// seriesFor - the ID of a data point's series, adding the series' row the
// first time the batch sees a series not known to be written
//
//	@return int64 - the series ID
//	@return bool - false when the attributes cannot be encoded
func (b *metricsV2BatchBuilder) seriesFor(state *batchState, mc *metricContext, attributes pcommon.Map, timestamp pcommon.Timestamp) (int64, bool) {
	attributesJSON, err := b.enc.canonicalJSON(attributes)
	if err != nil {
		state.errs = multierr.Append(state.errs, fmt.Errorf("data point attributes of %s: %w", mc.name, err))
		return 0, false
	}
	identity := seriesIdentity{
		metricName:         mc.name,
		metricType:         mc.metricType,
		unit:               mc.unit,
		temporality:        mc.temporality,
		isMonotonic:        mc.isMonotonic,
		resourceAttributes: mc.resourceAttributes,
		scopeName:          mc.scopeName,
		scopeVersion:       mc.scopeVersion,
		scopeAttributes:    mc.scopeAttributes,
		attributes:         attributesJSON,
	}
	seriesID := identity.id()

	if _, seen := state.seen[seriesID]; seen {
		return seriesID, true
	}
	state.seen[seriesID] = struct{}{}
	if b.known != nil && b.known(seriesID) {
		return seriesID, true
	}

	labels := make(map[string]any, len(mc.labels)+attributes.Len())
	for k, v := range mc.labels {
		labels[k] = v
	}
	b.enc.labels(attributes, labels)
	labelsJSON, err := marshalJSON(labels)
	if err != nil {
		state.errs = multierr.Append(state.errs, fmt.Errorf("labels of %s: %w", mc.name, err))
		return 0, false
	}

	state.batch.series = append(state.batch.series, seriesRow{
		SeriesID:           seriesID,
		MetricName:         mc.name,
		MetricType:         mc.metricType,
		Unit:               optionalString(mc.unit),
		Description:        optionalString(mc.description),
		Temporality:        mc.temporality,
		IsMonotonic:        mc.isMonotonic,
		ServiceName:        mc.serviceName,
		ScopeName:          optionalString(mc.scopeName),
		ScopeVersion:       optionalString(mc.scopeVersion),
		Labels:             labelsJSON,
		ResourceAttributes: mc.resourceAttributes,
		ScopeAttributes:    mc.scopeAttributes,
		Attributes:         attributesJSON,
		FirstSeen:          unixMilli(timestamp),
	})
	return seriesID, true
}

// unixMilli - a timestamp in the milliseconds a TIMESTAMP column holds
func unixMilli(ts pcommon.Timestamp) int64 {
	return ts.AsTime().UnixMilli()
}

// optionalUnixMilli - a timestamp in milliseconds, or nil when unset
func optionalUnixMilli(ts pcommon.Timestamp) *int64 {
	if ts == 0 {
		return nil
	}
	ms := unixMilli(ts)
	return &ms
}

// optionalFloat - the value of an optional field, or nil when unset
func optionalFloat(has bool, value func() float64) *float64 {
	if !has {
		return nil
	}
	v := value()
	return &v
}

// optionalString - a string, or nil when empty
func optionalString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// floatArrayJSON - a JSON array of floats; NaN and the infinities, which
// JSON numbers cannot hold, as the strings "NaN", "+Inf" and "-Inf"
func floatArrayJSON(values []float64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			b.WriteString(strconv.Quote(strconv.FormatFloat(v, 'g', -1, 64)))
		} else {
			b.WriteString(strconv.FormatFloat(v, 'g', -1, 64))
		}
	}
	b.WriteByte(']')
	return b.String()
}

// uintArrayJSON - a JSON array of counts
func uintArrayJSON(values []uint64) string {
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatUint(v, 10))
	}
	b.WriteByte(']')
	return b.String()
}
