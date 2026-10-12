// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"container/list"
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/kineticadb/kinetica-api-go/kinetica"
	"go.opentelemetry.io/collector/consumer/consumererror"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

// rowSink - where the rows of one table go; a [kinetica.BulkInserter] is one
type rowSink interface {
	Insert(record any) error
	Flush() error
	Close() error
}

// errorDrainer - a sink that queues the records the server refused one by
// one, which Flush does not return
type errorDrainer interface {
	DrainErrors() []kinetica.InsertError
}

// metricsWriter - writes metrics to the metric tables
type metricsWriter struct {
	// mu makes the writes of one export one unit: a Flush returns the
	// failures of the records the same export inserted, and the series
	// cache learns of a series only once its row is written
	mu      sync.Mutex
	logger  *zap.Logger
	builder *metricsBatchBuilder
	cache   *seriesCache

	series             rowSink
	numberPoints       rowSink
	histogramPoints    rowSink
	expHistogramPoints rowSink
	summaryPoints      rowSink
	exemplars          rowSink
}

// newMetricsWriter - a writer with a BulkInserter per metric table; the
// tables must exist
//
//	@param ctx - bounds the requests the BulkInserters send while starting
//	@param kiWriter - a connected [KiWriter]
//	@return *metricsWriter
//	@return error
func newMetricsWriter(ctx context.Context, kiWriter *KiWriter) (*metricsWriter, error) {
	// Retried exports must not duplicate points or series: skip existing keys
	keyedOptions := func() *kinetica.BulkInserterOptions {
		return kinetica.NewBulkInserterOptions().
			SetIgnoreExistingPK(true).
			SetReturnIndividualErrors(true)
	}
	tables := []struct {
		name    string
		options *kinetica.BulkInserterOptions
	}{
		{MetricSeriesTable, keyedOptions()},
		{MetricNumberPointTable, keyedOptions()},
		{MetricHistogramPointTable, keyedOptions()},
		{MetricExpHistogramPointTable, keyedOptions()},
		{MetricSummaryPointTable, keyedOptions()},
		{MetricExemplarTable, kinetica.NewBulkInserterOptions().SetReturnIndividualErrors(true)},
	}

	sinks := make([]rowSink, 0, len(tables))
	for _, table := range tables {
		inserter, err := kinetica.NewBulkInserterContext(ctx, kiWriter.Db, qualifiedTableName(kiWriter.cfg.Schema, table.name),
			ChunkSize, 0, table.options)
		if err != nil {
			for _, sink := range sinks {
				_ = sink.Close()
			}
			return nil, fmt.Errorf("cannot create the BulkInserter of %s: %w", table.name, err)
		}
		sinks = append(sinks, inserter)
	}

	return newMetricsWriterWithSinks(kiWriter.logger, kiWriter.cfg.Metrics, sinks), nil
}

// newMetricsWriterWithSinks - a writer sending each table's rows to the
// given sinks, in the order of [newMetricsWriter]'s tables
func newMetricsWriterWithSinks(logger *zap.Logger, cfg MetricsConfig, sinks []rowSink) *metricsWriter {
	w := &metricsWriter{
		logger:             logger,
		cache:              newSeriesCache(cfg.SeriesCacheSize),
		series:             sinks[0],
		numberPoints:       sinks[1],
		histogramPoints:    sinks[2],
		expHistogramPoints: sinks[3],
		summaryPoints:      sinks[4],
		exemplars:          sinks[5],
	}
	w.builder = &metricsBatchBuilder{
		enc:   newAttributeEncoder(cfg.ExcludeLabelKeys),
		known: w.cache.contains,
	}
	return w
}

// write - writes an export. The series go first, so that no point is
// written without its series; a failed flush is returned for the collector
// to retry the export, which the primary keys make safe. Data points that
// cannot be encoded, and records the server refuses one by one, fail for
// good, and are not retried.
//
//	@receiver w
//	@param md
//	@return error
func (w *metricsWriter) write(md pmetric.Metrics) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	batch, buildErr := w.builder.build(md)
	var permanent error
	if buildErr != nil {
		permanent = multierr.Append(permanent, buildErr)
	}

	w.logger.Debug("Writing metrics",
		zap.Int("series", len(batch.series)),
		zap.Int("number points", len(batch.numberPoints)),
		zap.Int("histogram points", len(batch.histogramPoints)),
		zap.Int("exp histogram points", len(batch.expHistogramPoints)),
		zap.Int("summary points", len(batch.summaryPoints)),
		zap.Int("exemplars", len(batch.exemplars)))

	seriesRefused, err := writeRows(MetricSeriesTable, w.series, batch.series)
	if err != nil {
		return err
	}
	permanent = multierr.Append(permanent, seriesRefused)

	var retryable error
	for _, writeTable := range []func() (refused error, err error){
		func() (error, error) { return writeRows(MetricNumberPointTable, w.numberPoints, batch.numberPoints) },
		func() (error, error) {
			return writeRows(MetricHistogramPointTable, w.histogramPoints, batch.histogramPoints)
		},
		func() (error, error) {
			return writeRows(MetricExpHistogramPointTable, w.expHistogramPoints, batch.expHistogramPoints)
		},
		func() (error, error) { return writeRows(MetricSummaryPointTable, w.summaryPoints, batch.summaryPoints) },
		func() (error, error) { return writeRows(MetricExemplarTable, w.exemplars, batch.exemplars) },
	} {
		refused, err := writeTable()
		permanent = multierr.Append(permanent, refused)
		retryable = multierr.Append(retryable, err)
	}

	// The series rows are written: later exports need not send them again.
	// When some were refused, which ones is not known, so none is
	// remembered and the next export sends them all again.
	if seriesRefused == nil {
		for i := range batch.series {
			w.cache.add(batch.series[i].SeriesID)
		}
	}

	if retryable != nil {
		w.logger.Error("Writing metrics failed", zap.Error(retryable))
		return retryable
	}
	if permanent != nil {
		w.logger.Error("Some metrics were not written", zap.Error(permanent))
		return consumererror.NewPermanent(permanent)
	}
	return nil
}

// writeRows - inserts a table's rows and flushes them
//
//	@return refused - the records the server refused one by one
//	@return err - the batches that failed, worth retrying
func writeRows[T any](table string, sink rowSink, rows []T) (refused error, err error) {
	if len(rows) == 0 {
		return nil, nil
	}
	var failed error
	for i := range rows {
		if err := sink.Insert(rows[i]); err != nil {
			failed = multierr.Append(failed, err)
			break
		}
	}
	failed = multierr.Append(failed, sink.Flush())

	drainer, ok := sink.(errorDrainer)
	if !ok {
		return nil, wrapTableError(table, failed)
	}
	drained := drainer.DrainErrors()
	if failed != nil {
		// The failed batches are among the drained errors: Flush reported them
		return nil, wrapTableError(table, failed)
	}
	for _, e := range drained {
		refused = multierr.Append(refused, fmt.Errorf("%d record(s) refused: %s", e.RecordCount, e.Message))
	}
	return wrapTableError(table, refused), nil
}

// wrapTableError - an error naming its table, or nil
func wrapTableError(table string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", table, err)
}

// close - flushes and closes every sink
//
//	@receiver w
//	@return error
func (w *metricsWriter) close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	var errs error
	for _, sink := range []rowSink{w.series, w.numberPoints, w.histogramPoints, w.expHistogramPoints, w.summaryPoints, w.exemplars} {
		errs = multierr.Append(errs, sink.Close())
	}
	return errs
}

// qualifiedTableName - a table's name with the schema before it, if one is
// configured
//
//	@param schema
//	@param table
//	@return string
func qualifiedTableName(schema string, table string) string {
	if schema = strings.TrimSpace(schema); schema != "" {
		return schema + "." + table
	}
	return table
}

// seriesCache - the IDs of the series most recently written, up to a
// capacity; the least recently used is forgotten first. A capacity of 0
// remembers none. It is not safe for concurrent use.
type seriesCache struct {
	capacity int
	order    *list.List
	elements map[int64]*list.Element
}

// newSeriesCache - constructor for [seriesCache]
func newSeriesCache(capacity int) *seriesCache {
	return &seriesCache{capacity: capacity, order: list.New(), elements: map[int64]*list.Element{}}
}

// contains - whether a series is remembered, marking it as used
func (c *seriesCache) contains(seriesID int64) bool {
	element, ok := c.elements[seriesID]
	if ok {
		c.order.MoveToFront(element)
	}
	return ok
}

// add - remembers a series, forgetting the least recently used one when full
func (c *seriesCache) add(seriesID int64) {
	if c.capacity <= 0 {
		return
	}
	if element, ok := c.elements[seriesID]; ok {
		c.order.MoveToFront(element)
		return
	}
	if c.order.Len() >= c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.elements, oldest.Value.(int64))
	}
	c.elements[seriesID] = c.order.PushFront(seriesID)
}

// len - the number of series remembered
func (c *seriesCache) len() int {
	return c.order.Len()
}
