package runmetrics

import (
	"testing"
	"time"
)

func TestParentOnlyDurationSubtractsUnionedChildren(t *testing.T) {
	base := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	parent := Span{Start: base, Stop: base.Add(10 * time.Minute)}
	children := []Span{
		{Start: base.Add(-time.Minute), Stop: base.Add(4 * time.Minute)},
		{Start: base.Add(3 * time.Minute), Stop: base.Add(8 * time.Minute)},
		{Start: base.Add(9 * time.Minute), Stop: base.Add(12 * time.Minute)},
	}

	if got, want := UnionDuration(parent, children), 9*time.Minute; got != want {
		t.Fatalf("UnionDuration = %s, want %s", got, want)
	}
	if got, want := ParentOnlyDuration(parent, children), time.Minute; got != want {
		t.Fatalf("ParentOnlyDuration = %s, want %s", got, want)
	}
}

func TestParallelismUsesSummedChildDurationsOverUnion(t *testing.T) {
	base := time.Date(2026, 8, 26, 1, 0, 0, 0, time.UTC)
	parent := Span{Start: base, Stop: base.Add(10 * time.Minute)}
	children := []Span{
		{Start: base.Add(time.Minute), Stop: base.Add(5 * time.Minute)},
		{Start: base.Add(3 * time.Minute), Stop: base.Add(7 * time.Minute)},
	}

	got, ok := Parallelism(parent, children)
	if !ok {
		t.Fatal("Parallelism reported no interpretable child span")
	}
	if got != 8.0/6.0 {
		t.Fatalf("Parallelism = %f, want %f", got, 8.0/6.0)
	}
	coverage, ok := Coverage(parent, children)
	if !ok {
		t.Fatal("Coverage reported no parent span")
	}
	if coverage != 0.6 {
		t.Fatalf("Coverage = %f, want 0.6", coverage)
	}
}

func TestMedianDurationInterpolatesEvenLength(t *testing.T) {
	got, ok := MedianDuration([]time.Duration{4 * time.Second, time.Second, 3 * time.Second, 2 * time.Second})
	if !ok {
		t.Fatal("MedianDuration returned false")
	}
	if got != 2500*time.Millisecond {
		t.Fatalf("MedianDuration = %s, want 2.5s", got)
	}
}
