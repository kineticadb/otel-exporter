// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kineticadb/kinetica-api-go/kinetica"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// spikeData - metrics whose query results are known: three samples a minute
// apart, from base, for the series the templates and dashboards read
type spikeData struct {
	base time.Time
}

func (d spikeData) at(minutes int) time.Time {
	return d.base.Add(time.Duration(minutes) * time.Minute)
}

func (d spikeData) ts(minutes int) pcommon.Timestamp {
	return pcommon.NewTimestampFromTime(d.at(minutes))
}

// templateTime - a time as the templates return a time bucket
func (d spikeData) templateTime(minutes int) string {
	return d.at(minutes).Format("2006-01-02 15:04:05.000")
}

// metrics - the spike's metrics:
//
//   - checkout (host h1): http.server.requests, a cumulative counter, on
//     /api/v1/cart (0, 60, 180) and on /api/v1/pay (100, then 10: a reset);
//     http.server.request.duration, a cumulative histogram, with status 200
//     (count 10 then 30, sum 1 then 5) and 500 (count 0 then 10, sum 0
//     then 3, an exemplar of 0.9); rpc.server.duration, a delta histogram
//     with buckets 3, 4, 2, 1 under 0.1, 0.5, 1 and +Inf;
//     messaging.client.sent.messages, a delta sum of 120 over a minute;
//     http.server.active_requests, 4 then 6; system.cpu.time, idle 0 then
//     45 and user 0 then 15; system.cpu.load_average.1m, 1.5 then 2.5;
//     system.memory.usage, used 1000 and free 3000; system.filesystem.usage
//     on /, used 30 and free 70; system.network.io received 0 then 6000;
//     system.disk.io written 0 then 12000; and spike.labels, a gauge whose
//     labels have awkward values
//   - checkout-v2 and Checkout: http.server.active_requests, 1
func (d spikeData) metrics() pmetric.Metrics {
	md := pmetric.NewMetrics()

	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "checkout")
	rm.Resource().Attributes().PutStr("host.name", "h1")
	metrics := rm.ScopeMetrics().AppendEmpty().Metrics()
	start := d.ts(-60)

	cumulativeSum := func(name string, attrs map[string]string, values ...float64) {
		m := metrics.AppendEmpty()
		m.SetName(name)
		sum := m.SetEmptySum()
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		sum.SetIsMonotonic(true)
		for i, v := range values {
			dp := sum.DataPoints().AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(d.ts(i))
			dp.SetDoubleValue(v)
			for k, a := range attrs {
				dp.Attributes().PutStr(k, a)
			}
		}
	}
	gauge := func(name string, attrs map[string]string, values ...float64) {
		m := metrics.AppendEmpty()
		m.SetName(name)
		g := m.SetEmptyGauge()
		for i, v := range values {
			dp := g.DataPoints().AppendEmpty()
			dp.SetTimestamp(d.ts(i))
			dp.SetDoubleValue(v)
			for k, a := range attrs {
				dp.Attributes().PutStr(k, a)
			}
		}
	}
	upDownCounter := func(name string, attrs map[string]string, values ...float64) {
		m := metrics.AppendEmpty()
		m.SetName(name)
		sum := m.SetEmptySum()
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		for i, v := range values {
			dp := sum.DataPoints().AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(d.ts(i))
			dp.SetDoubleValue(v)
			for k, a := range attrs {
				dp.Attributes().PutStr(k, a)
			}
		}
	}

	cumulativeSum("http.server.requests", map[string]string{"http.route": "/api/v1/cart"}, 0, 60, 180)
	cumulativeSum("http.server.requests", map[string]string{"http.route": "/api/v1/pay"}, 100, 10)

	duration := metrics.AppendEmpty()
	duration.SetName("http.server.request.duration")
	duration.SetUnit("s")
	histogram := duration.SetEmptyHistogram()
	histogram.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	for _, series := range []struct {
		status  int64
		counts  [2]uint64
		sums    [2]float64
		buckets [2][]uint64
	}{
		{200, [2]uint64{10, 30}, [2]float64{1, 5}, [2][]uint64{{5, 3, 1, 1}, {15, 9, 4, 2}}},
		{500, [2]uint64{0, 10}, [2]float64{0, 3}, [2][]uint64{{0, 0, 0, 0}, {0, 5, 4, 1}}},
	} {
		for i := range series.counts {
			dp := histogram.DataPoints().AppendEmpty()
			dp.SetStartTimestamp(start)
			dp.SetTimestamp(d.ts(i))
			dp.SetCount(series.counts[i])
			dp.SetSum(series.sums[i])
			dp.ExplicitBounds().FromRaw([]float64{0.1, 0.5, 1})
			dp.BucketCounts().FromRaw(series.buckets[i])
			dp.Attributes().PutStr("http.route", "/api/v1/cart")
			dp.Attributes().PutInt("http.response.status_code", series.status)
			if series.status == 500 && i == 1 {
				exemplar := dp.Exemplars().AppendEmpty()
				exemplar.SetTimestamp(d.ts(1))
				exemplar.SetDoubleValue(0.9)
				exemplar.SetTraceID(pcommon.TraceID([16]byte{0xab, 15: 0x01}))
			}
		}
	}

	rpc := metrics.AppendEmpty()
	rpc.SetName("rpc.server.duration")
	delta := rpc.SetEmptyHistogram()
	delta.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	rdp := delta.DataPoints().AppendEmpty()
	rdp.SetStartTimestamp(d.ts(0))
	rdp.SetTimestamp(d.ts(1))
	rdp.SetCount(10)
	rdp.ExplicitBounds().FromRaw([]float64{0.1, 0.5, 1})
	rdp.BucketCounts().FromRaw([]uint64{3, 4, 2, 1})

	sent := metrics.AppendEmpty()
	sent.SetName("messaging.client.sent.messages")
	deltaSum := sent.SetEmptySum()
	deltaSum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
	deltaSum.SetIsMonotonic(true)
	sdp := deltaSum.DataPoints().AppendEmpty()
	sdp.SetStartTimestamp(d.ts(0))
	sdp.SetTimestamp(d.ts(1))
	sdp.SetIntValue(120)
	sdp.Attributes().PutStr("messaging.destination.name", "orders")

	upDownCounter("http.server.active_requests", nil, 4, 6)
	cumulativeSum("system.cpu.time", map[string]string{"cpu": "cpu0", "state": "idle"}, 0, 45)
	cumulativeSum("system.cpu.time", map[string]string{"cpu": "cpu0", "state": "user"}, 0, 15)
	gauge("system.cpu.load_average.1m", nil, 1.5, 2.5)
	upDownCounter("system.memory.usage", map[string]string{"state": "used"}, 1000)
	upDownCounter("system.memory.usage", map[string]string{"state": "free"}, 3000)
	upDownCounter("system.filesystem.usage", map[string]string{"mountpoint": "/", "state": "used"}, 30)
	upDownCounter("system.filesystem.usage", map[string]string{"mountpoint": "/", "state": "free"}, 70)
	cumulativeSum("system.network.io", map[string]string{"device": "eth0", "direction": "receive"}, 0, 6000)
	cumulativeSum("system.disk.io", map[string]string{"device": "sda", "direction": "write"}, 0, 12000)
	gauge("spike.labels", map[string]string{
		"deploy.time":  "2026-10-12T10:00:00Z",
		"http.route":   "/api/v1/cart",
		"k8s.pod.name": "checkout-7d9f-x2",
		"k8s-pod":      "p1",
		"note":         "two words",
		"user.name":    "o'brien",
	}, 1)

	for _, service := range []string{"checkout-v2", "Checkout"} {
		other := md.ResourceMetrics().AppendEmpty()
		other.Resource().Attributes().PutStr("service.name", service)
		m := other.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
		m.SetName("http.server.active_requests")
		sum := m.SetEmptySum()
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
		dp := sum.DataPoints().AppendEmpty()
		dp.SetTimestamp(d.ts(0))
		dp.SetDoubleValue(1)
	}
	return md
}

// liveRows - the records a statement returns, each a map from column name to
// value (int32, int64, float32, float64, string, []byte, bool or nil), and
// the result's type, as binary encoding gives them
func liveRows(t *testing.T, db *kinetica.Kinetica, sql string) ([]map[string]any, *kinetica.Type) {
	t.Helper()
	resp, err := db.ExecuteSqlTyped(context.Background(), sql, 0, kinetica.EndOfSet, "", nil, nil)
	require.NoError(t, err, sql)
	rows := make([]map[string]any, len(resp.Data))
	for i, record := range resp.Data {
		row := map[string]any{}
		for _, column := range resp.DataType.Columns() {
			value, _ := record.GetByName(column.Name())
			row[column.Name()] = value.Interface()
		}
		rows[i] = row
	}
	return rows, resp.DataType
}

// number - a numeric value as a float64
func number(t *testing.T, value any) float64 {
	t.Helper()
	switch v := value.(type) {
	case int32:
		return float64(v)
	case int64:
		return float64(v)
	case float32:
		return float64(v)
	case float64:
		return v
	}
	t.Fatalf("%v (%T) is not a number", value, value)
	return 0
}

// assertRows - checks that rows are exactly the expected ones, in any order:
// each expected row gives the values of some columns, compared as numbers
// (to 1e-9) or as they are
func assertRows(t *testing.T, rows []map[string]any, expected ...map[string]any) {
	t.Helper()
	require.Len(t, rows, len(expected), "rows: %v", rows)
	used := make([]bool, len(rows))
next:
	for _, want := range expected {
		for i, row := range rows {
			if !used[i] && rowMatches(t, row, want) {
				used[i] = true
				continue next
			}
		}
		t.Errorf("no row %v in %v", want, rows)
	}
}

func rowMatches(t *testing.T, row map[string]any, want map[string]any) bool {
	for column, expected := range want {
		actual, ok := row[column]
		if !ok {
			t.Fatalf("no column %s in %v", column, row)
		}
		switch e := expected.(type) {
		case float64:
			if actual == nil || abs(number(t, actual)-e) > 1e-9 {
				return false
			}
		default:
			if actual != expected {
				return false
			}
		}
	}
	return true
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// TestMetricsSpike is the validation spike of the metric tables: on a
// live server, it writes metrics whose results are known and checks the
// values the text search, the query templates, the rollups and the
// dashboard queries return. Skipped unless KINETICA_TEST_URL is set.
func TestMetricsSpike(t *testing.T) {
	ctx := context.Background()
	exp, schema := startLiveMetrics(t, "otel_spike")
	db := exp.writer.GetDb()
	// Recent enough for the rollup procedures, which read the last minutes
	data := spikeData{base: time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)}
	require.NoError(t, exp.pushMetricsData(ctx, data.metrics()))

	series := schema + "." + MetricSeriesTable
	matching := func(t *testing.T, metric string, where string) int {
		rows, _ := liveRows(t, db, fmt.Sprintf("SELECT series_id FROM %s WHERE metric_name = '%s' AND %s", series, metric, where))
		return len(rows)
	}

	t.Run("labels", func(t *testing.T) {
		const active, labels = "http.server.active_requests", "spike.labels"
		for _, c := range []struct {
			name   string
			metric string
			where  string
			want   int
		}{
			{"exact value", active, `text_match(labels, 'labels.service.name:"checkout"')`, 1},
			{"exact value with a hyphen", active, `text_match(labels, 'labels.service.name:"checkout-v2"')`, 1},
			{"case-sensitive", active, `text_match(labels, 'labels.service.name:"Checkout"')`, 1},
			{"regex over all three", active, `text_match(labels, 'labels.service.name:/[cC]heckout.*/')`, 3},
			{"value with slashes", labels, `text_match(labels, 'labels.http.route:"/api/v1/cart"')`, 1},
			{"regex with slashes", labels, `text_match(labels, 'labels.http.route://api/.*/')`, 1},
			{"value with hyphens, unquoted", labels, `text_match(labels, 'labels.k8s.pod.name:checkout-7d9f-x2')`, 1},
			{"value with a space", labels, `text_match(labels, 'labels.note:"two words"')`, 1},
			{"a word of a value does not match", labels, `text_match(labels, 'labels.note:"two"')`, 0},
			{"RFC 3339 value", labels, `text_match(labels, 'labels.deploy.time:"2026-10-12T10:00:00Z"')`, 1},
			{"value with an apostrophe", labels, `text_match(labels, 'labels.user.name:"o''brien"')`, 1},
			{"key with a hyphen", labels, `text_match(labels, 'labels.k8s-pod:"p1"')`, 1},
			{"two regexes, two text_match calls", labels,
				`text_match(labels, 'labels.http.route:/.*cart/') AND text_match(labels, 'labels.k8s.pod.name:/checkout-.*/')`, 1},
			{"OR of terms in one text_match", labels, `text_match(labels, 'labels.note:"two" OR labels.note:"two words"')`, 1},
			{"JSON_VALUE of a dotted key", labels, `JSON_VALUE(labels, '$["user.name"]') = 'o''brien'`, 1},
		} {
			t.Run(c.name, func(t *testing.T) {
				assert.Equal(t, c.want, matching(t, c.metric, c.where))
			})
		}

		t.Run("a regex cannot be part of a compound query", func(t *testing.T) {
			_, err := db.ExecuteSqlTyped(ctx, "SELECT series_id FROM "+series+
				` WHERE text_match(labels, 'labels.http.route:/.*cart/ AND labels.note:"two words"')`,
				0, kinetica.EndOfSet, "", nil, nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "Regex query does not support json paths")
		})
	})

	queries, err := loadSQLTemplates(sqlTemplateFiles[0])
	require.NoError(t, err)
	template := func(t *testing.T, name string, values map[string]string) []map[string]any {
		t.Helper()
		all := sampleTemplateValues(schema)
		all["from"] = data.at(-60).Format(time.DateTime)
		all["to"] = data.at(60).Format(time.DateTime)
		for k, v := range values {
			all[k] = v
		}
		for _, tpl := range queries {
			if tpl.name == name {
				sql, err := tpl.render(all)
				require.NoError(t, err)
				rows, _ := liveRows(t, db, sql)
				return rows
			}
		}
		t.Fatalf("no template %s", name)
		return nil
	}

	t.Run("templates", func(t *testing.T) {
		t.Run("counter_rate_by_label", func(t *testing.T) {
			rows := template(t, "counter_rate_by_label", map[string]string{"metric": "http.server.requests", "label": "http.route"})
			assertRows(t, rows,
				map[string]any{"time_unix": data.templateTime(1), "label_value": "/api/v1/cart", "per_second": 1.0},
				map[string]any{"time_unix": data.templateTime(1), "label_value": "/api/v1/pay", "per_second": 10.0 / 60},
				map[string]any{"time_unix": data.templateTime(2), "label_value": "/api/v1/cart", "per_second": 2.0})
		})
		t.Run("delta_rate_by_label", func(t *testing.T) {
			rows := template(t, "delta_rate_by_label", map[string]string{
				"metric": "messaging.client.sent.messages", "label": "messaging.destination.name"})
			assertRows(t, rows, map[string]any{"time_unix": data.templateTime(1), "label_value": "orders", "per_second": 2.0})
		})
		t.Run("histogram_rate_by_label", func(t *testing.T) {
			rows := template(t, "histogram_rate_by_label", map[string]string{"label": "http.response.status_code"})
			assertRows(t, rows,
				map[string]any{"time_unix": data.templateTime(1), "label_value": "200", "per_second": 20.0 / 60},
				map[string]any{"time_unix": data.templateTime(1), "label_value": "500", "per_second": 10.0 / 60})
		})
		t.Run("histogram_mean_by_label", func(t *testing.T) {
			rows := template(t, "histogram_mean_by_label", map[string]string{"label": "http.response.status_code"})
			assertRows(t, rows,
				map[string]any{"time_unix": data.templateTime(1), "label_value": "200", "mean": 0.2},
				map[string]any{"time_unix": data.templateTime(1), "label_value": "500", "mean": 0.3})
		})
		t.Run("histogram_buckets_delta", func(t *testing.T) {
			rows := template(t, "histogram_buckets_delta", map[string]string{"metric": "rpc.server.duration"})
			assertRows(t, rows,
				map[string]any{"upper_bound": 0.1, "observations": 3.0},
				map[string]any{"upper_bound": 0.5, "observations": 4.0},
				map[string]any{"upper_bound": 1.0, "observations": 2.0},
				map[string]any{"upper_bound": nil, "observations": 1.0})
		})
		for quantile, want := range map[string]float64{"0.3": 0.1, "0.5": 0.3, "0.95": 1.0} {
			t.Run("histogram_quantile_delta "+quantile, func(t *testing.T) {
				rows := template(t, "histogram_quantile_delta", map[string]string{"metric": "rpc.server.duration", "quantile": quantile})
				assertRows(t, rows, map[string]any{"quantile_value": want})
			})
		}
		t.Run("gauge_by_label", func(t *testing.T) {
			rows := template(t, "gauge_by_label", map[string]string{"metric": "system.cpu.load_average.1m", "label": "host.name"})
			assertRows(t, rows,
				map[string]any{"time_unix": data.templateTime(0), "label_value": "h1", "value": 1.5},
				map[string]any{"time_unix": data.templateTime(1), "label_value": "h1", "value": 2.5})
		})
		t.Run("label_values", func(t *testing.T) {
			rows := template(t, "label_values", map[string]string{"metric": "http.server.requests", "label": "http.route"})
			assertRows(t, rows,
				map[string]any{"label_value": "/api/v1/cart", "series": 1.0},
				map[string]any{"label_value": "/api/v1/pay", "series": 1.0})
		})
		t.Run("label_names", func(t *testing.T) {
			rows := template(t, "label_names", map[string]string{"metric": "spike.labels"})
			var names []any
			for _, row := range rows {
				names = append(names, row["label"])
			}
			assert.ElementsMatch(t, []any{"deploy.time", "host.name", "http.route", "k8s-pod", "k8s.pod.name", "note",
				"service.name", "user.name"}, names)
		})
		t.Run("series_search_negated", func(t *testing.T) {
			rows := template(t, "series_search_negated", map[string]string{
				"metric": "http.server.active_requests", "label": "service.name", "value": "checkout"})
			assert.Len(t, rows, 2)
		})
		t.Run("exemplars_largest", func(t *testing.T) {
			rows := template(t, "exemplars_largest", nil)
			assertRows(t, rows, map[string]any{"value": 0.9, "trace_id": "ab000000000000000000000000000001"})
		})
	})

	t.Run("rollups", func(t *testing.T) {
		rollups, err := loadSQLTemplates(sqlTemplateFiles[1])
		require.NoError(t, err)
		for _, tpl := range rollups {
			sql, err := tpl.render(sampleTemplateValues(schema))
			require.NoError(t, err)
			_, err = db.Execute(ctx, sql)
			require.NoError(t, err, tpl.name)
		}
		for _, procedure := range []string{"rollup_metric_number_point_1m", "rollup_metric_number_point_1h"} {
			_, err := db.Execute(ctx, "EXECUTE PROCEDURE "+schema+"."+procedure)
			require.NoError(t, err, procedure)
		}

		rows := template(t, "gauge_by_label_1m", map[string]string{"metric": "system.cpu.load_average.1m", "label": "host.name"})
		assertRows(t, rows,
			map[string]any{"time_unix": data.templateTime(0), "label_value": "h1", "value": 1.5},
			map[string]any{"time_unix": data.templateTime(1), "label_value": "h1", "value": 2.5})

		hourly, _ := liveRows(t, db, fmt.Sprintf(`SELECT SUM(r.sum_value) AS sum_value, SUM(r.samples) AS samples
FROM %s.metric_number_point_1h r
JOIN (SELECT series_id FROM %s WHERE metric_name = 'system.cpu.load_average.1m') s ON s.series_id = r.series_id`, schema, series))
		assertRows(t, hourly, map[string]any{"sum_value": 4.0, "samples": 2.0})
	})

	t.Run("dashboards", func(t *testing.T) {
		ms := func(minutes int) int64 { return data.at(minutes).UnixMilli() }
		expected := map[string][]map[string]any{
			"Requests per second":      {{"time_unix": ms(1), "service": "checkout", "requests_per_second": 0.5}},
			"5xx responses per second": {{"time_unix": ms(1), "service": "checkout", "errors_per_second": 10.0 / 60}},
			"Error ratio":              {{"time_unix": ms(1), "service": "checkout", "error_ratio": 1.0 / 3}},
			"Mean request duration":    {{"time_unix": ms(1), "service": "checkout", "mean_duration": 7.0 / 30}},
			"Active requests": {
				{"time_unix": ms(0), "service": "checkout", "active_requests": 4.0},
				{"time_unix": ms(0), "service": "checkout-v2", "active_requests": 1.0},
				{"time_unix": ms(0), "service": "Checkout", "active_requests": 1.0},
				{"time_unix": ms(1), "service": "checkout", "active_requests": 6.0},
			},
			"Busiest routes": {{"service": "checkout", "route": "/api/v1/cart", "requests": 30.0,
				"error_ratio": 1.0 / 3, "mean_duration": 7.0 / 30}},
			"Slowest exemplars": {{"duration": 0.9, "service": "checkout", "route": "/api/v1/cart"}},
			"CPU utilization":   {{"time_unix": ms(1), "host": "h1", "cpu_utilization": 0.25}},
			"Load average (1m)": {
				{"time_unix": ms(0), "host": "h1", "load_average": 1.5},
				{"time_unix": ms(1), "host": "h1", "load_average": 2.5},
			},
			"Memory used":            {{"time_unix": ms(0), "host": "h1", "used_bytes": 1000.0}},
			"Filesystem utilization": {{"time_unix": ms(0), "host": "h1", "mountpoint": "/", "utilization": 0.3}},
			"Network throughput":     {{"time_unix": ms(1), "host": "h1", "direction": "receive", "bytes_per_second": 100.0}},
			"Disk throughput":        {{"time_unix": ms(1), "host": "h1", "direction": "write", "bytes_per_second": 200.0}},
		}

		for file, dashboard := range loadDashboards(t) {
			for _, panel := range dashboard.Panels {
				if panel.Type == "text" {
					continue
				}
				t.Run(file+"/"+panel.Title, func(t *testing.T) {
					sql := withSchema(expandPluginMacros(panel.Targets[0].RawSQL, data.at(-60), data.at(60)), schema)
					rows, resultType := liveRows(t, db, sql)

					if panel.Type == "timeseries" {
						// The plugin turns a column into time only from a
						// 64-bit integer: milliseconds since the epoch
						timeColumn := resultType.ColumnByName(panel.Transformations[0].Options.Conversions[0].TargetField)
						require.NotNil(t, timeColumn)
						assert.Equal(t, kinetica.BaseTypeLong, timeColumn.BaseType())
					}
					if want, ok := expected[panel.Title]; ok {
						// A DECIMAL reaches the plugin as a string, which a
						// panel cannot plot or format as a number
						for column, value := range want[0] {
							if _, isNumber := value.(float64); isNumber {
								assert.Contains(t, []kinetica.ColumnBaseType{kinetica.BaseTypeDouble, kinetica.BaseTypeLong},
									resultType.ColumnByName(column).BaseType(), "type of %s", column)
							}
						}
						assertRows(t, rows, want...)
					} else {
						assert.NotEmpty(t, rows)
					}
				})
			}
		}
	})
}
