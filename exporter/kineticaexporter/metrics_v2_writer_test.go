// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"errors"
	"testing"

	"github.com/kineticadb/kinetica-api-go/kinetica"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.uber.org/zap"
)

// fakeSink - a rowSink that keeps what it is given, standing in for a
// BulkInserter
type fakeSink struct {
	inserted []any
	flushes  int
	closed   bool
	flushErr error
	refused  []kinetica.InsertError
}

func (f *fakeSink) Insert(record any) error {
	f.inserted = append(f.inserted, record)
	return nil
}

func (f *fakeSink) Flush() error {
	f.flushes++
	return f.flushErr
}

func (f *fakeSink) Close() error {
	f.closed = true
	return nil
}

func (f *fakeSink) DrainErrors() []kinetica.InsertError {
	refused := f.refused
	f.refused = nil
	return refused
}

// newTestWriter - a writer over fake sinks: series, number points,
// histogram points, exp histogram points, summary points and exemplars
func newTestWriter(cfg MetricsV2Config) (*metricsV2Writer, []*fakeSink) {
	sinks := make([]*fakeSink, 6)
	rowSinks := make([]rowSink, 6)
	for i := range sinks {
		sinks[i] = &fakeSink{}
		rowSinks[i] = sinks[i]
	}
	return newMetricsV2WriterWithSinks(zap.NewNop(), cfg, rowSinks), sinks
}

func TestWriterWritesEveryTable(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 100})

	require.NoError(t, w.write(newTestMetrics()))

	counts := make([]int, len(sinks))
	for i, sink := range sinks {
		counts[i] = len(sink.inserted)
		assert.Equal(t, 1, sink.flushes)
	}
	assert.Equal(t, []int{5, 3, 1, 1, 1, 1}, counts)
	assert.IsType(t, seriesRow{}, sinks[0].inserted[0])
	assert.IsType(t, numberPointRow{}, sinks[1].inserted[0])
}

func TestWriterSendsASeriesOnlyOnce(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 100})

	require.NoError(t, w.write(newTestMetrics()))
	require.NoError(t, w.write(newTestMetrics()))

	assert.Len(t, sinks[0].inserted, 5)
	assert.Equal(t, 1, sinks[0].flushes, "no series flush when there are no new series")
	assert.Len(t, sinks[1].inserted, 6)
}

func TestWriterWithoutCacheSendsSeriesEveryTime(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 0})

	require.NoError(t, w.write(newTestMetrics()))
	require.NoError(t, w.write(newTestMetrics()))

	assert.Len(t, sinks[0].inserted, 10)
}

func TestWriterStopsWhenTheSeriesFail(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 100})
	sinks[0].flushErr = errors.New("connection refused")

	err := w.write(newTestMetrics())

	require.Error(t, err)
	assert.False(t, consumererror.IsPermanent(err), "a failed flush is retried")
	assert.ErrorContains(t, err, MetricSeriesTable)
	for _, sink := range sinks[1:] {
		assert.Empty(t, sink.inserted, "no point is written without its series")
	}

	// The retry sends the series again
	sinks[0].flushErr = nil
	require.NoError(t, w.write(newTestMetrics()))
	assert.Len(t, sinks[0].inserted, 10)
}

func TestWriterRetriesFailedPoints(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 100})
	sinks[1].flushErr = errors.New("server exited")

	err := w.write(newTestMetrics())

	require.Error(t, err)
	assert.False(t, consumererror.IsPermanent(err))
	assert.ErrorContains(t, err, MetricNumberPointTable)
	assert.Len(t, sinks[2].inserted, 1, "the other tables are still written")
	assert.Equal(t, 5, w.cache.len(), "the series were written")
}

func TestWriterReportsRefusedRecordsAsPermanent(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 100})
	sinks[2].refused = []kinetica.InsertError{{RecordCount: 1, Message: "invalid value"}}

	err := w.write(newTestMetrics())

	require.Error(t, err)
	assert.True(t, consumererror.IsPermanent(err))
	assert.ErrorContains(t, err, MetricHistogramPointTable)
	assert.ErrorContains(t, err, "invalid value")
}

func TestWriterDoesNotRememberRefusedSeries(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{SeriesCacheSize: 100})
	sinks[0].refused = []kinetica.InsertError{{RecordCount: 1, Message: "invalid labels"}}

	err := w.write(newTestMetrics())

	require.Error(t, err)
	assert.True(t, consumererror.IsPermanent(err))
	assert.Equal(t, 0, w.cache.len())
}

func TestWriterCloseClosesEverySink(t *testing.T) {
	w, sinks := newTestWriter(MetricsV2Config{})

	require.NoError(t, w.close())

	for _, sink := range sinks {
		assert.True(t, sink.closed)
	}
}

func TestSeriesCacheForgetsTheLeastRecentlyUsed(t *testing.T) {
	cache := newSeriesCache(2)
	cache.add(1)
	cache.add(2)
	assert.True(t, cache.contains(1)) // 1 is now the most recently used
	cache.add(3)

	assert.True(t, cache.contains(1))
	assert.False(t, cache.contains(2))
	assert.True(t, cache.contains(3))
	assert.Equal(t, 2, cache.len())
}

func TestSeriesCacheOfNoCapacityRemembersNothing(t *testing.T) {
	cache := newSeriesCache(0)
	cache.add(1)

	assert.False(t, cache.contains(1))
}

func TestQualifiedTableName(t *testing.T) {
	assert.Equal(t, "otel.metric_series", qualifiedTableName(" otel ", MetricSeriesTable))
	assert.Equal(t, "metric_series", qualifiedTableName("", MetricSeriesTable))
}
