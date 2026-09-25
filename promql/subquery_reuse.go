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

package promql

import (
	"context"
	"reflect"
	"time"

	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/util/annotations"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// SubqueryReuseStats describes reuse performed within one query evaluation.
type SubqueryReuseStats struct {
	Hits              int
	AvoidedSelections int
	// RetainedSamples is the peak number of samples held for reuse.
	RetainedSamples int
}

type subquerySelection struct {
	selector *parser.VectorSelector
	hints    *storage.SelectHints
	path     []parser.Node
}

func (s subquerySelection) selectSeries(ctx context.Context, querier storage.Querier) {
	selectCtx, span := otel.Tracer("").Start(ctx, "querierSelect", trace.WithAttributes(
		attribute.String("selector", s.selector.String()),
		attribute.Int64("start", s.hints.Start),
		attribute.Int64("end", s.hints.End),
	))
	s.selector.UnexpandedSeriesSet = querier.Select(selectCtx, false, s.hints, s.selector.LabelMatchers...)
	span.End()
}

type subqueryCandidate struct {
	expr       *parser.SubqueryExpr
	selections []subquerySelection
	prepared   bool
	group      *subqueryGroup
}

type subqueryGroup struct {
	plan      []subquerySelection
	remaining int
	matrix    Matrix
	ready     bool
	samples   int
}

type subqueryKey struct {
	expression string
	start      int64
	end        int64
	interval   int64
	rangeMs    int64
	offsetMs   int64
	timestamp  int64
	hasAt      bool
}

type subqueryReuse struct {
	candidates map[*parser.SubqueryExpr]*subqueryCandidate
	skipped    map[*parser.VectorSelector]bool
	maxSamples int
	retained   int
	stats      *SubqueryReuseStats
}

const maxRetainedSubquerySamples = 500_000

func planSubqueryReuse(stmt *parser.EvalStmt, selections []subquerySelection, noStepInterval func(int64) int64, maxSamples int, reuseStats *SubqueryReuseStats, hintsEquivalent func(a, b *storage.SelectHints) bool) *subqueryReuse {
	rootInterval := durationMilliseconds(stmt.Interval)
	if rootInterval == 0 {
		rootInterval = 1
	}
	root := evaluator{
		startTimestamp:           timeMilliseconds(stmt.Start),
		endTimestamp:             timeMilliseconds(stmt.End),
		interval:                 rootInterval,
		noStepSubqueryIntervalFn: noStepInterval,
	}
	reuse := &subqueryReuse{
		candidates: make(map[*parser.SubqueryExpr]*subqueryCandidate),
		skipped:    make(map[*parser.VectorSelector]bool),
		maxSamples: maxSamples,
		stats:      reuseStats,
	}
	var ordered []*subqueryCandidate
	parser.Inspect(stmt.Expr, func(node parser.Node, path []parser.Node) error {
		subq, ok := node.(*parser.SubqueryExpr)
		if !ok || !floatSubqueryExpr(subq.Expr) || !subqueryCallArgument(subq, path) {
			return nil
		}
		for _, parent := range path {
			switch parent.(type) {
			case *parser.SubqueryExpr, *parser.StepInvariantExpr:
				return nil
			}
		}
		candidate := &subqueryCandidate{expr: subq, prepared: true}
		reuse.candidates[subq] = candidate
		ordered = append(ordered, candidate)
		return nil
	})
	for _, selection := range selections {
		for i := len(selection.path) - 1; i >= 0; i-- {
			if subq, ok := selection.path[i].(*parser.SubqueryExpr); ok {
				if candidate := reuse.candidates[subq]; candidate != nil {
					candidate.selections = append(candidate.selections, selection)
				}
				break
			}
		}
	}
	groups := make(map[subqueryKey][]*subqueryGroup)
	for _, candidate := range ordered {
		subq := candidate.expr
		copySubq := *subq
		copySubq.Offset = subq.OriginalOffset
		if subq.Timestamp != nil {
			copySubq.Offset += time.Duration(root.startTimestamp-*subq.Timestamp) * time.Millisecond
		}
		start, end, interval := root.subqueryTimeRange(&copySubq)
		key := subqueryKey{
			expression: subq.Expr.String(),
			start:      start,
			end:        end,
			interval:   interval,
			rangeMs:    durationMilliseconds(subq.Range),
			offsetMs:   durationMilliseconds(subq.OriginalOffset),
			hasAt:      subq.Timestamp != nil,
		}
		if subq.Timestamp != nil {
			key.timestamp = *subq.Timestamp
		}
		for _, group := range groups[key] {
			if equalSubquerySelections(group.plan, candidate.selections, hintsEquivalent) {
				candidate.group = group
				candidate.prepared = false
				group.remaining++
				for _, selection := range candidate.selections {
					reuse.skipped[selection.selector] = true
					reuse.stats.AvoidedSelections++
				}
				break
			}
		}
		if candidate.group == nil {
			candidate.group = &subqueryGroup{plan: candidate.selections, remaining: 1}
			groups[key] = append(groups[key], candidate.group)
		}
	}
	return reuse
}

func subqueryCallArgument(subq *parser.SubqueryExpr, path []parser.Node) bool {
	if len(path) == 0 {
		return false
	}
	call, ok := path[len(path)-1].(*parser.Call)
	if !ok {
		return false
	}
	for _, arg := range call.Args {
		if arg == subq {
			return true
		}
	}
	return false
}

func floatSubqueryExpr(expr parser.Expr) bool {
	switch e := expr.(type) {
	case *parser.ParenExpr:
		return floatSubqueryExpr(e.Expr)
	case *parser.StepInvariantExpr:
		return floatSubqueryExpr(e.Expr)
	case *parser.Call:
		switch e.Func.Name {
		case "histogram_count", "histogram_sum", "histogram_avg":
			return true
		}
	case *parser.AggregateExpr:
		return e.Op == parser.COUNT
	}
	return false
}

func equalSubquerySelections(a, b []subquerySelection, hintsEquivalent func(a, b *storage.SelectHints) bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !equivalentSelectionHints(a[i].hints, b[i].hints, hintsEquivalent) ||
			!reflect.DeepEqual(a[i].selector.LabelMatchers, b[i].selector.LabelMatchers) ||
			a[i].selector.SkipHistogramBuckets != b[i].selector.SkipHistogramBuckets ||
			a[i].selector.Anchored != b[i].selector.Anchored ||
			a[i].selector.Smoothed != b[i].selector.Smoothed ||
			a[i].selector.OriginalOffset != b[i].selector.OriginalOffset ||
			!reflect.DeepEqual(a[i].selector.Timestamp, b[i].selector.Timestamp) {
			return false
		}
	}
	return true
}

func equivalentSelectionHints(a, b *storage.SelectHints, equivalent func(a, b *storage.SelectHints) bool) bool {
	if reflect.DeepEqual(a, b) {
		return true
	}
	return equivalent != nil && equivalent(a, b)
}

// matrix returns a completed matrix for a new storage view. An absent release
// means the caller must evaluate normally; this also covers ineligible subqueries.
func (r *subqueryReuse) matrix(ctx context.Context, ev *evaluator, subq *parser.SubqueryExpr, outerOffset, outerRange int64) (Matrix, func(), annotations.Annotations) {
	candidate := r.candidates[subq]
	if candidate == nil {
		return nil, nil, nil
	}
	if !candidate.prepared {
		if candidate.group.ready {
			if err := contextDone(ctx, "expression evaluation"); err != nil {
				ev.error(err)
			}
			r.stats.Hits++
			return candidate.group.matrix, r.release(ev, candidate.group), nil
		}
		for _, selection := range candidate.selections {
			selection.selectSeries(ctx, ev.querier)
			r.stats.AvoidedSelections--
		}
		candidate.prepared = true
	}
	group := candidate.group
	if group.ready {
		if err := contextDone(ctx, "expression evaluation"); err != nil {
			ev.error(err)
		}
		r.stats.Hits++
		return group.matrix, r.release(ev, group), nil
	}
	val, childStats, warnings := ev.runSubquery(ctx, subq)
	ev.samplesStats.UpdatePeakFromSubquery(childStats)
	ev.samplesStats.MergeSamplesReadFromSubquery(childStats, ev.startTimestamp, ev.interval, ev.numSteps(), outerOffset, outerRange)
	mat := val.(Matrix)
	samples := mat.TotalSamples()
	// Keep reuse small relative to the engine's sample limit so cached matrices
	// leave room for the rest of the expression. Large results fall back to a
	// normal evaluation after preparing the duplicate selectors on demand.
	budget := min(r.maxSamples/128, maxRetainedSubquerySamples)
	if group.remaining > 1 && len(warnings) == 0 && floatMatrix(mat) && samples <= budget-r.retained {
		group.matrix = mat
		group.ready = true
		group.samples = samples
		r.retained += samples
		if r.retained > r.stats.RetainedSamples {
			r.stats.RetainedSamples = r.retained
		}
		return mat, r.release(ev, group), warnings
	}
	group.remaining--
	return mat, func() { ev.currentSamples -= samples }, warnings
}

func floatMatrix(mat Matrix) bool {
	for _, series := range mat {
		if len(series.Histograms) != 0 {
			return false
		}
	}
	return true
}

func (r *subqueryReuse) release(ev *evaluator, group *subqueryGroup) func() {
	return func() {
		group.remaining--
		if group.remaining == 0 {
			ev.currentSamples -= group.samples
			r.retained -= group.samples
			group.matrix = nil
			group.ready = false
		}
	}
}
