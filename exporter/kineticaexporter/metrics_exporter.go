// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"context"
	"fmt"
	"strings"

	"github.com/samber/lo"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/multierr"
	"go.uber.org/zap"
)

type kineticaMetricsExporter struct {
	logger *zap.Logger

	writer *KiWriter
	// metrics writes the metric tables; nil until the exporter starts
	metrics *metricsWriter
}

func newMetricsExporter(_ *zap.Logger, cfg *Config) (*kineticaMetricsExporter, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	kineticaLogger := cfg.createLogger()
	writer := NewKiWriter(*cfg, kineticaLogger)
	metricsExp := &kineticaMetricsExporter{
		logger: kineticaLogger,
		writer: writer,
	}
	return metricsExp, nil
}

func (e *kineticaMetricsExporter) start(ctx context.Context, _ component.Host) error {

	if err := e.writer.Connect(ctx); err != nil {
		return err
	}

	fmt.Println("SCHEMA NAME - ", e.writer.cfg.Schema)

	if e.writer.cfg.Schema != "" && len(e.writer.cfg.Schema) != 0 {
		// Config has a schema name
		if err := createSchema(ctx, e.writer, e.writer.cfg); err != nil {
			return err
		}
	}

	if err := createMetricTables(ctx, e.writer); err != nil {
		return err
	}
	metrics, err := newMetricsWriter(ctx, e.writer)
	if err != nil {
		return err
	}
	e.metrics = metrics
	return nil
}

// createMetricTables - creates the metric tables that do not exist
//
//	@param ctx
//	@param kiWriter
//	@return error
func createMetricTables(ctx context.Context, kiWriter *KiWriter) error {
	var errs []error

	var schema string
	schema = strings.Trim(kiWriter.cfg.Schema, " ")
	if len(schema) > 0 {
		schema += "."
	} else {
		schema = ""
	}

	lo.ForEach(metricTableDDLs(kiWriter.cfg.Metrics.LabelsAnalyzer), func(ddl string, index int) {

		stmt := strings.ReplaceAll(ddl, "%s", schema)
		kiWriter.logger.Debug("Creating Table - ", zap.String("DDL", stmt))

		_, err := kiWriter.Db.ExecuteSqlRaw(ctx, stmt, 0, 0, "", nil)
		if err != nil {
			kiWriter.logger.Error(err.Error())
			errs = append(errs, err)
		}
	})

	return multierr.Combine(errs...)
}

// createSchema - Create a schema
//
//	@param ctx
//	@param kiWriter
//	@param config
//	@return bool - True if schema creation successful
func createSchema(ctx context.Context, kiWriter *KiWriter, config Config) error {
	stmt := fmt.Sprintf(CreateSchema, config.Schema)
	kiWriter.logger.Debug(stmt)
	_, err := kiWriter.Db.ExecuteSqlRaw(ctx, stmt, 0, 0, "", nil)
	if err != nil {
		kiWriter.logger.Error(err.Error())
	}
	return err
}

// shutdown will shut down the exporter.
func (e *kineticaMetricsExporter) shutdown(_ context.Context) error {
	var errs error
	if e.metrics != nil {
		errs = multierr.Append(errs, e.metrics.close())
		e.metrics = nil
	}
	return multierr.Append(errs, e.writer.Close())
}

// pushMetricsData - this method is called by the collector to feed the metrics data to the exporter
//
//	@receiver e
//	@param _ ctx unused
//	@param md
//	@return error
func (e *kineticaMetricsExporter) pushMetricsData(_ context.Context, md pmetric.Metrics) error {
	return e.metrics.write(md)
}
