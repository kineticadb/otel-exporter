// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

package kineticaexporter

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// loadSeries - the series of each load batch
const loadSeries = 2000

// loadBatch - a gauge with a point at each of the given times for each of
// count series, the series named from first on; one export's worth
func loadBatch(first int, count int, times []time.Time) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	rm.Resource().Attributes().PutStr("service.name", "load")
	rm.Resource().Attributes().PutStr("host.name", "load-host")
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("load.gauge")
	points := m.SetEmptyGauge().DataPoints()
	points.EnsureCapacity(count * len(times))
	for _, ts := range times {
		for i := first; i < first+count; i++ {
			dp := points.AppendEmpty()
			dp.SetTimestamp(pcommon.NewTimestampFromTime(ts))
			dp.SetDoubleValue(float64(i))
			dp.Attributes().PutStr("instance", "i-"+strconv.Itoa(i))
			dp.Attributes().PutStr("zone", "z"+strconv.Itoa(i%4))
		}
	}
	return md
}

// latencies - durations, summarised
type latencies []time.Duration

func (l latencies) String() string {
	if len(l) == 0 {
		return "no queries"
	}
	sorted := slices.Clone(l)
	slices.Sort(sorted)
	at := func(q float64) time.Duration { return sorted[int(q*float64(len(sorted)-1))] }
	return fmt.Sprintf("%d queries, p50 %v, p95 %v, max %v", len(sorted),
		at(0.5).Round(time.Millisecond), at(0.95).Round(time.Millisecond), sorted[len(sorted)-1].Round(time.Millisecond))
}

// TestMetricsLoad measures the metrics write path on a live server: the rate
// of points written through the exporter, alone and while a label search
// joined to the points runs every 100ms, and while new series keep coming,
// which keeps the series table's text index changing. Skipped unless both
// KINETICA_TEST_URL and KINETICA_TEST_LOAD, the number of points of each
// phase (e.g. 200000), are set. It reports and does not judge the numbers.
func TestMetricsLoad(t *testing.T) {
	total, _ := strconv.Atoi(os.Getenv("KINETICA_TEST_LOAD"))
	if total <= 0 {
		t.Skip("set KINETICA_TEST_LOAD to the number of points of each phase, e.g. 200000, to run the load test")
	}
	ctx := context.Background()
	exp, schema := startLiveMetrics(t, "otel_load")
	db := exp.writer.GetDb()

	// Each export holds 20 points of each of 2000 series
	const pointsPerSeries = 20
	exports := total / (loadSeries * pointsPerSeries)
	if exports < 1 {
		exports = 1
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(-24 * time.Hour)
	nextTime := 0
	times := func() []time.Time {
		ts := make([]time.Time, pointsPerSeries)
		for i := range ts {
			ts[i] = base.Add(time.Duration(nextTime) * time.Second)
			nextTime++
		}
		return ts
	}

	query := fmt.Sprintf(`SELECT COUNT(*) AS points
FROM (SELECT series_id FROM %[1]s.metric_series
       WHERE metric_name = 'load.gauge' AND text_match(labels, 'labels.zone:"z1"')) s
JOIN %[1]s.metric_number_point p ON p.series_id = s.series_id`, schema)

	// queryLoop - runs the query every 100ms until stop is closed
	queryLoop := func(stop <-chan struct{}) (*latencies, *sync.WaitGroup, *error) {
		var measured latencies
		var failure error
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-stop:
					return
				case <-ticker.C:
					began := time.Now()
					if _, err := db.ExecuteSqlTyped(ctx, query, 0, 10, "", nil, nil); err != nil {
						failure = err
						return
					}
					measured = append(measured, time.Since(began))
				}
			}
		}()
		return &measured, &wg, &failure
	}

	// run - writes exports, each made by batch, and reports the rates
	run := func(t *testing.T, name string, withQueries bool, batch func(i int) pmetric.Metrics) {
		batches := make([]pmetric.Metrics, exports)
		points := 0
		for i := range batches {
			batches[i] = batch(i)
			points += batches[i].DataPointCount()
		}

		// The client's share: building the rows of one export
		buildStart := time.Now()
		_, err := exp.metrics.builder.build(batches[0])
		require.NoError(t, err)
		build := time.Since(buildStart)

		stop := make(chan struct{})
		var measured *latencies
		var wg *sync.WaitGroup
		var failure *error
		if withQueries {
			measured, wg, failure = queryLoop(stop)
		}
		began := time.Now()
		for _, md := range batches {
			require.NoError(t, exp.pushMetricsData(ctx, md))
		}
		elapsed := time.Since(began)
		close(stop)
		if withQueries {
			wg.Wait()
			require.NoError(t, *failure)
		}

		line := fmt.Sprintf("%-28s %7d points in %6.2fs: %8.0f points/s (building one export of %d points: %v)",
			name, points, elapsed.Seconds(), float64(points)/elapsed.Seconds(),
			batches[0].DataPointCount(), build.Round(time.Millisecond))
		if withQueries {
			line += "; label search + join: " + measured.String()
		}
		t.Log(line)
	}

	t.Run("known series", func(t *testing.T) {
		// Writes the series once, then measures points of known series
		require.NoError(t, exp.pushMetricsData(ctx, loadBatch(0, loadSeries, times()[:1])))
		run(t, "points, no queries", false, func(int) pmetric.Metrics { return loadBatch(0, loadSeries, times()) })
		run(t, "points, with queries", true, func(int) pmetric.Metrics { return loadBatch(0, loadSeries, times()) })
	})

	t.Run("new series", func(t *testing.T) {
		// Every export brings 2000 series never seen, so each one writes
		// series rows and changes the text index the queries read
		next := 1_000_000
		run(t, "new series, with queries", true, func(int) pmetric.Metrics {
			md := loadBatch(next, loadSeries, times())
			next += loadSeries
			return md
		})
	})
}
