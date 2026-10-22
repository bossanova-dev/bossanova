package runmetrics

import "time"

// Span is one half-open interval. Zero or inverted spans have no duration.
type Span struct {
	Start time.Time
	Stop  time.Time
}

// Duration returns stopped - started. Open or inverted runs return zero.
func Duration(started, stopped time.Time) time.Duration {
	if started.IsZero() || stopped.IsZero() || !stopped.After(started) {
		return 0
	}
	return stopped.Sub(started)
}

// UnionDuration returns the duration covered by at least one child span after
// clipping each child to parent. Open children are treated as running until the
// parent stop by the caller.
func UnionDuration(parent Span, children []Span) time.Duration {
	parentDuration := Duration(parent.Start, parent.Stop)
	if parentDuration == 0 || len(children) == 0 {
		return 0
	}
	clipped := make([]Span, 0, len(children))
	for _, child := range children {
		start := child.Start
		if start.Before(parent.Start) {
			start = parent.Start
		}
		stop := child.Stop
		if stop.After(parent.Stop) {
			stop = parent.Stop
		}
		if stop.After(start) {
			clipped = append(clipped, Span{Start: start, Stop: stop})
		}
	}
	if len(clipped) == 0 {
		return 0
	}
	sortSpans(clipped)
	union := time.Duration(0)
	cur := clipped[0]
	for _, next := range clipped[1:] {
		if !next.Start.After(cur.Stop) {
			if next.Stop.After(cur.Stop) {
				cur.Stop = next.Stop
			}
			continue
		}
		union += cur.Stop.Sub(cur.Start)
		cur = next
	}
	union += cur.Stop.Sub(cur.Start)
	return union
}

// ParentOnlyDuration subtracts child union time from the parent span.
func ParentOnlyDuration(parent Span, children []Span) time.Duration {
	parentDuration := Duration(parent.Start, parent.Stop)
	childUnion := UnionDuration(parent, children)
	if childUnion >= parentDuration {
		return 0
	}
	return parentDuration - childUnion
}

// Parallelism is sum(child durations) / union(child spans). The bool is false
// when no interpretable child span exists.
func Parallelism(parent Span, children []Span) (float64, bool) {
	union := UnionDuration(parent, children)
	if union == 0 {
		return 0, false
	}
	sum := time.Duration(0)
	for _, child := range children {
		start := child.Start
		if start.Before(parent.Start) {
			start = parent.Start
		}
		stop := child.Stop
		if stop.After(parent.Stop) {
			stop = parent.Stop
		}
		sum += Duration(start, stop)
	}
	return float64(sum) / float64(union), true
}

// Coverage is child union time divided by the parent span.
func Coverage(parent Span, children []Span) (float64, bool) {
	parentDuration := Duration(parent.Start, parent.Stop)
	if parentDuration == 0 {
		return 0, false
	}
	return float64(UnionDuration(parent, children)) / float64(parentDuration), true
}

func sortSpans(spans []Span) {
	for i := 1; i < len(spans); i++ {
		for j := i; j > 0 && spans[j].Start.Before(spans[j-1].Start); j-- {
			spans[j], spans[j-1] = spans[j-1], spans[j]
		}
	}
}

// MedianDuration returns an interpolated median and false for an empty input.
func MedianDuration(values []time.Duration) (time.Duration, bool) {
	if len(values) == 0 {
		return 0, false
	}
	cp := append([]time.Duration(nil), values...)
	sortDurations(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid], true
	}
	return (cp[mid-1] + cp[mid]) / 2, true
}

func sortDurations(values []time.Duration) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// MedianInt64 returns the median of values and whether one exists. An even-sized
// sample takes the truncated mean of the two middle values, matching
// MedianDuration — a "median reviewer dispatches" of 2 rather than 2.5 is the
// honest resolution for a whole-count metric.
func MedianInt64(values []int64) (int64, bool) {
	if len(values) == 0 {
		return 0, false
	}
	cp := append([]int64(nil), values...)
	sortInt64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid], true
	}
	return (cp[mid-1] + cp[mid]) / 2, true
}

func sortInt64s(values []int64) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// TerminalStateMix counts how many runs ended in each terminal state. The empty
// token means "not recorded" (a run that printed no terminal state, or a runner
// whose transcript is not parsed) and is counted under its own key so a caller
// can tell "no boss-build runs in this window" from "every run blocked".
func TerminalStateMix(states []string) map[string]int64 {
	if len(states) == 0 {
		return nil
	}
	out := make(map[string]int64, len(states))
	for _, state := range states {
		out[state]++
	}
	return out
}
