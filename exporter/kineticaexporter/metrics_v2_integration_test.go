// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kineticadb/kinetica-api-go/kinetica"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/config/configopaque"
	"go.uber.org/zap"
)

// TestMetricsV2Integration writes metrics in the v2 layout to a live server
// and runs the text-search queries the layout is designed for. It needs a
// server with enable_text_search = true, and is skipped unless
// KINETICA_TEST_URL is set (with KINETICA_TEST_USER and
// KINETICA_TEST_PASSWORD). It works in a schema of its own, dropped at the
// end.
func TestMetricsV2Integration(t *testing.T) {
	ctx := context.Background()
	exp, schema := startLiveMetricsV2(t, "otel_it")

	client, err := kinetica.NewSQLClient(exp.writer.GetDb())
	require.NoError(t, err)
	table := func(name string) string { return schema + "." + name }
	count := func(t *testing.T, sql string) int64 {
		result, err := client.Execute(ctx, sql)
		require.NoError(t, err, sql)
		n, err := kinetica.SQLResultScalar[int64](result)
		require.NoError(t, err, sql)
		return n
	}

	require.NoError(t, exp.pushMetricsDataV2(ctx, newTestMetrics()))

	t.Run("rows", func(t *testing.T) {
		assert.EqualValues(t, 5, count(t, "SELECT COUNT(*) FROM "+table(MetricSeriesTable)))
		assert.EqualValues(t, 3, count(t, "SELECT COUNT(*) FROM "+table(MetricNumberPointTable)))
		assert.EqualValues(t, 1, count(t, "SELECT COUNT(*) FROM "+table(MetricHistogramPointTable)))
		assert.EqualValues(t, 1, count(t, "SELECT COUNT(*) FROM "+table(MetricExpHistogramPointTable)))
		assert.EqualValues(t, 1, count(t, "SELECT COUNT(*) FROM "+table(MetricSummaryPointTable)))
		assert.EqualValues(t, 1, count(t, "SELECT COUNT(*) FROM "+table(MetricExemplarTable)))
	})

	series := table(MetricSeriesTable)
	queries := []struct {
		name  string
		where string
		want  int64
	}{
		{"flat dotted key, column prefix", `text_match(labels, 'labels.service.name:"checkout"')`, 5},
		{"flat dotted key, no prefix", `text_match(labels, 'service.name:"checkout"')`, 5},
		{"keyword analyzer is case-sensitive", `text_match(labels, 'labels.service.name:"Checkout"')`, 0},
		{"anchored regex on a dotted key", `text_match(labels, 'labels.http.response.status_code:/2../')`, 1},
		{"negation includes series without the label", `NOT text_match(labels, 'labels.http.request.method:"GET"')`, 4},
		{"scope labels", `text_match(labels, 'labels.otel.scope.name:"otelhttp"')`, 5},
		{"JSON_VALUE with bracket notation", `JSON_VALUE(labels, '$["otel.scope.version"]') = '1.0'`, 5},
	}
	for _, q := range queries {
		t.Run(q.name, func(t *testing.T) {
			assert.Equal(t, q.want, count(t, "SELECT COUNT(*) FROM "+series+" WHERE "+q.where))
		})
	}

	t.Run("search series, join points", func(t *testing.T) {
		sql := `SELECT COUNT(*)
FROM (SELECT series_id FROM ` + series + `
       WHERE metric_name = 'http.server.active_requests'
         AND text_match(labels, 'labels.http.request.method:"GET"')) s
JOIN ` + table(MetricNumberPointTable) + ` p ON p.series_id = s.series_id`
		assert.EqualValues(t, 2, count(t, sql))
	})

	t.Run("rollups", func(t *testing.T) {
		templates, err := loadSQLTemplates(sqlTemplateFiles[1])
		require.NoError(t, err)
		for _, tpl := range templates {
			sql, err := tpl.render(sampleTemplateValues(schema))
			require.NoError(t, err)
			_, err = client.Execute(ctx, sql)
			require.NoError(t, err, tpl.name)
		}
		_, err = client.Execute(ctx, "EXECUTE PROCEDURE "+schema+".rollup_metric_number_point_1m")
		assert.NoError(t, err)
	})

	t.Run("query templates", func(t *testing.T) {
		templates, err := loadSQLTemplates(sqlTemplateFiles[0])
		require.NoError(t, err)
		for _, tpl := range templates {
			t.Run(tpl.name, func(t *testing.T) {
				values := sampleTemplateValues(schema)
				values["metric"] = templateTestMetric(tpl.name)
				sql, err := tpl.render(values)
				require.NoError(t, err)
				_, err = client.Execute(ctx, sql)
				assert.NoError(t, err, sql)
			})
		}
	})

	t.Run("dashboard queries", func(t *testing.T) {
		for file, dashboard := range loadDashboards(t) {
			for _, panel := range dashboard.Panels {
				for _, target := range panel.Targets {
					t.Run(file+"/"+panel.Title, func(t *testing.T) {
						sql := withSchema(expandPluginMacros(target.RawSQL, testTime.Add(-time.Hour), testTime.Add(time.Hour)), schema)
						_, err := client.Execute(ctx, sql)
						assert.NoError(t, err, sql)
					})
				}
			}
		}
	})

	t.Run("a retried export adds no series or points", func(t *testing.T) {
		require.NoError(t, exp.pushMetricsDataV2(ctx, newTestMetrics()))
		exp.v2.cache = newSeriesCache(0) // as after a restart
		require.NoError(t, exp.pushMetricsDataV2(ctx, newTestMetrics()))

		assert.EqualValues(t, 5, count(t, "SELECT COUNT(*) FROM "+series))
		assert.EqualValues(t, 3, count(t, "SELECT COUNT(*) FROM "+table(MetricNumberPointTable)))
		assert.EqualValues(t, 1, count(t, "SELECT COUNT(*) FROM "+table(MetricHistogramPointTable)))
	})
}

// templateTestMetric - a metric of newTestMetrics of the type a query
// template reads
func templateTestMetric(template string) string {
	switch {
	case strings.HasPrefix(template, "histogram_"), strings.HasPrefix(template, "exemplars_"):
		return "http.server.request.duration"
	case strings.HasPrefix(template, "counter_"), strings.HasPrefix(template, "delta_"):
		return "http.server.requests"
	default:
		return "http.server.active_requests"
	}
}

// startLiveMetricsV2 - a started v2 metrics exporter writing to a schema of
// its own on the server KINETICA_TEST_URL names, which is dropped when the
// test ends; the test is skipped when KINETICA_TEST_URL is not set
//
//	@param schemaPrefix - the start of the schema's name
//	@return *kineticaMetricsExporter
//	@return string - the schema
func startLiveMetricsV2(t *testing.T, schemaPrefix string) (*kineticaMetricsExporter, string) {
	t.Helper()
	url := os.Getenv("KINETICA_TEST_URL")
	if url == "" {
		t.Skip("set KINETICA_TEST_URL, KINETICA_TEST_USER and KINETICA_TEST_PASSWORD to run against a server")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("%s_%d", schemaPrefix, time.Now().UnixNano())
	cfg := withDefaultConfig(func(cfg *Config) {
		cfg.Host = url
		cfg.Username = os.Getenv("KINETICA_TEST_USER")
		cfg.Password = configopaque.String(os.Getenv("KINETICA_TEST_PASSWORD"))
		cfg.Schema = schema
		cfg.MetricsSchema = MetricsSchemaV2
	})

	exp, err := newMetricsExporter(zap.NewNop(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, exp.shutdown(ctx))
		db, err := GetGpuDbInst(ctx, cfg)
		if assert.NoError(t, err) {
			_, err = db.Execute(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
			assert.NoError(t, err)
			assert.NoError(t, db.Close())
		}
	})
	require.NoError(t, exp.start(ctx, componenttest.NewNopHost()))
	return exp, schema
}
