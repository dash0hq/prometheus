// Copyright 2026 Dash0 Inc.
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package promql_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/promqltest"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
)

type reuseCountingQueryable struct {
	storage.Queryable
	selections int
}

func (q *reuseCountingQueryable) Querier(mint, maxt int64) (storage.Querier, error) {
	querier, err := q.Queryable.Querier(mint, maxt)
	if err != nil {
		return nil, err
	}
	return &reuseCountingQuerier{Querier: querier, count: &q.selections}, nil
}

type reuseCountingQuerier struct {
	storage.Querier
	count *int
}

type reuseWarningSet struct {
	storage.SeriesSet
}

func (s reuseWarningSet) Warnings() annotations.Annotations {
	return annotations.Annotations{"storage warning": errors.New("storage warning")}
}

type reuseFaultQueryable struct {
	storage.Queryable
	warning bool
	fail    bool
}

func (q reuseFaultQueryable) Querier(mint, maxt int64) (storage.Querier, error) {
	querier, err := q.Queryable.Querier(mint, maxt)
	if err != nil {
		return nil, err
	}
	return reuseFaultQuerier{Querier: querier, warning: q.warning, fail: q.fail}, nil
}

type reuseFaultQuerier struct {
	storage.Querier
	warning bool
	fail    bool
}

func (q reuseFaultQuerier) Select(ctx context.Context, sorted bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	if q.fail {
		return storage.ErrSeriesSet(errors.New("storage error"))
	}
	set := q.Querier.Select(ctx, sorted, hints, matchers...)
	if q.warning {
		return reuseWarningSet{SeriesSet: set}
	}
	return set
}

func (q *reuseCountingQuerier) Select(ctx context.Context, sorted bool, hints *storage.SelectHints, matchers ...*labels.Matcher) storage.SeriesSet {
	*q.count++
	return q.Querier.Select(ctx, sorted, hints, matchers...)
}

func TestSubqueryReuse(t *testing.T) {
	store := promqltest.LoadedStorage(t, `
load 30s
  metric{job="a"} 1+1x30
  metric{job="b"} 2+1x30
  histogram{job="a"} {{schema:1 count:5 sum:20 buckets:[1 2 1 1]}}+{{schema:1 count:10 sum:5 buckets:[1 2 3 4]}}x30
`)
	subquery := `count by (job) (metric)[2m:30s]`
	histogramSubquery := `histogram_count(sum by (job) (rate(histogram[1m])))[2m:30s]`
	cases := []struct {
		name       string
		expression string
		equivalent func(a, b *storage.SelectHints) bool
		hits       int
	}{
		{"identical_hints", `avg_over_time(` + subquery + `) + avg_over_time(` + subquery + `)`, nil, 1},
		{"three_consumers", `avg_over_time(` + subquery + `) + avg_over_time(` + subquery + `) + avg_over_time(` + subquery + `)`, nil, 2},
		{"different_outer_function_with_exact_hints", `avg_over_time(` + subquery + `) + stddev_over_time(` + subquery + `)`, nil, 0},
		{"proven_equivalent_outer_function", `avg_over_time(` + subquery + `) + stddev_over_time(` + subquery + `)`, func(a, b *storage.SelectHints) bool {
			aCopy, bCopy := *a, *b
			aCopy.AllFuncs, bCopy.AllFuncs = nil, nil
			return reflect.DeepEqual(aCopy, bCopy)
		}, 1},
		{"different_offsets", `avg_over_time(` + subquery + `) + avg_over_time(count by (job) (metric)[2m:30s] offset 30s)`, nil, 0},
		{"different_grids", `avg_over_time(` + subquery + `) + avg_over_time(count by (job) (metric)[2m:1m])`, nil, 0},
		{"different_at", `avg_over_time(` + subquery + `) + avg_over_time(count by (job) (metric)[2m:30s] @ 300)`, nil, 0},
		{"histogram_capable_expression", `avg_over_time(sum by (job) (metric)[2m:30s]) + avg_over_time(sum by (job) (metric)[2m:30s])`, nil, 0},
		{"histogram_count_float_output", `avg_over_time(` + histogramSubquery + `) + stddev_over_time(` + histogramSubquery + `)`, func(a, b *storage.SelectHints) bool {
			aCopy, bCopy := *a, *b
			aCopy.AllFuncs, bCopy.AllFuncs = nil, nil
			return reflect.DeepEqual(aCopy, bCopy)
		}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := func(enabled bool) (*promql.Result, int, promql.SubqueryReuseStats) {
				engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
					MaxSamples: 10000, Timeout: 5 * time.Second, LookbackDelta: 5 * time.Minute,
					EnableAtModifier: true, EnableSubqueryReuse: enabled,
					NoStepSubqueryIntervalFn:         func(int64) int64 { return 30000 },
					SubquerySelectionHintsEquivalent: tc.equivalent,
				})
				queryable := &reuseCountingQueryable{Queryable: store}
				query, err := engine.NewRangeQuery(t.Context(), queryable, nil, tc.expression, time.Unix(240, 0), time.Unix(300, 0), time.Minute)
				require.NoError(t, err)
				t.Cleanup(query.Close)
				result := query.Exec(t.Context())
				return result, queryable.selections, promql.QuerySubqueryReuseStats(query)
			}
			original, originalSelections, _ := run(false)
			optimized, optimizedSelections, stats := run(true)
			require.Equal(t, original.Err, optimized.Err)
			require.Equal(t, original.Warnings, optimized.Warnings)
			require.Equal(t, original.Value, optimized.Value)
			require.Equal(t, tc.hits, stats.Hits)
			require.Equal(t, tc.hits, stats.AvoidedSelections)
			require.Equal(t, originalSelections-tc.hits, optimizedSelections)
		})
	}
}

func TestSubqueryReusePreservesErrorsAndWarnings(t *testing.T) {
	store := promqltest.LoadedStorage(t, `
load 30s
  metric{job="a"} 1+1x30
`)
	expression := `avg_over_time(count by (job) (metric)[2m:30s]) + avg_over_time(count by (job) (metric)[2m:30s])`
	for _, tc := range []struct {
		name    string
		warning bool
		fail    bool
		limit   int
	}{
		{"warning", true, false, 10000},
		{"storage_error", false, true, 10000},
		{"sample_limit", false, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := func(enabled bool) (*promql.Result, promql.SubqueryReuseStats) {
				engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
					MaxSamples: tc.limit, Timeout: 5 * time.Second, LookbackDelta: 5 * time.Minute,
					EnableSubqueryReuse: enabled,
				})
				query, err := engine.NewInstantQuery(t.Context(), reuseFaultQueryable{Queryable: store, warning: tc.warning, fail: tc.fail}, nil, expression, time.Unix(300, 0))
				require.NoError(t, err)
				t.Cleanup(query.Close)
				result := query.Exec(t.Context())
				return result, promql.QuerySubqueryReuseStats(query)
			}
			original, _ := run(false)
			optimized, stats := run(true)
			require.Equal(t, original.Err, optimized.Err)
			require.Equal(t, original.Warnings, optimized.Warnings)
			require.Equal(t, original.Value, optimized.Value)
			require.Zero(t, stats.Hits)
			if tc.warning {
				require.Zero(t, stats.AvoidedSelections)
			}
		})
	}
}

func TestSubqueryReuseAtSampleLimit(t *testing.T) {
	store := promqltest.LoadedStorage(t, `
load 30s
  metric{job="a"} 1+1x30
  metric{job="b"} 2+1x30
`)
	expression := `avg_over_time(count by (job) (metric)[2m:30s]) + avg_over_time(count by (job) (metric)[2m:30s])`
	run := func(maxSamples int, enabled bool) (*promql.Result, int) {
		engine := promqltest.NewTestEngineWithOpts(t, promql.EngineOpts{
			MaxSamples: maxSamples, Timeout: 5 * time.Second, LookbackDelta: 5 * time.Minute,
			EnableSubqueryReuse: enabled,
		})
		query, err := engine.NewInstantQuery(t.Context(), store, nil, expression, time.Unix(300, 0))
		require.NoError(t, err)
		t.Cleanup(query.Close)
		result := query.Exec(t.Context())
		return result, query.Stats().Samples.PeakSamples
	}
	unlimited, peak := run(10000, false)
	require.NoError(t, unlimited.Err)
	for _, limit := range []int{peak - 1, peak} {
		original, _ := run(limit, false)
		optimized, _ := run(limit, true)
		require.Equal(t, original.Err, optimized.Err)
		require.Equal(t, original.Warnings, optimized.Warnings)
		require.Equal(t, original.Value, optimized.Value)
	}
}

func BenchmarkSubqueryReuse(b *testing.B) {
	store := promqltest.LoadedStorage(b, `
load 30s
  metric{job="a"} 1+1x200
  metric{job="b"} 2+1x200
`)
	expression := `avg_over_time(count by (job) (metric)[10m:30s]) + stddev_over_time(count by (job) (metric)[10m:30s])`
	for _, enabled := range []bool{false, true} {
		name := "without_reuse"
		if enabled {
			name = "with_reuse"
		}
		b.Run(name, func(b *testing.B) {
			engine := promqltest.NewTestEngineWithOpts(b, promql.EngineOpts{
				MaxSamples: 50_000_000, Timeout: 5 * time.Second, LookbackDelta: 5 * time.Minute,
				EnableSubqueryReuse: enabled,
				SubquerySelectionHintsEquivalent: func(a, c *storage.SelectHints) bool {
					aCopy, cCopy := *a, *c
					aCopy.AllFuncs, cCopy.AllFuncs = nil, nil
					return reflect.DeepEqual(aCopy, cCopy)
				},
			})
			b.ReportAllocs()
			for b.Loop() {
				query, err := engine.NewRangeQuery(context.Background(), store, nil, expression, time.Unix(3600, 0), time.Unix(4200, 0), time.Minute)
				if err != nil {
					b.Fatal(err)
				}
				result := query.Exec(context.Background())
				if result.Err != nil {
					b.Fatal(result.Err)
				}
				query.Close()
			}
		})
	}
}
