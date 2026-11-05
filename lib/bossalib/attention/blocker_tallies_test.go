package attention

import (
	"reflect"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestBlockerTallies(t *testing.T) {
	finding := func(severity, file string) Blocker {
		return Blocker{Kind: BlockerKindReviewFinding, Severity: severity, File: file}
	}
	for _, tc := range []struct {
		name     string
		blockers []Blocker
		tallies  []BlockerTally
		files    []string
	}{
		{name: "none"},
		{
			name:     "one P2 finding",
			blockers: []Blocker{finding("P2", "a.go")},
			tallies:  []BlockerTally{{Severity: "P2", Kind: BlockerKindReviewFinding, Count: 1}},
			files:    []string{"a.go"},
		},
		{
			name: "most severe first, then check, conflict and others in first-seen order",
			blockers: []Blocker{
				{Kind: "no-review"}, {Kind: "conflict"}, finding("P2", "a.go"), {Kind: "operator-note"},
				{Kind: "check"}, finding("P3", "b.go"), finding("P0", "c.go"), finding("P1", "d.go"), finding("P2", "e.go"),
			},
			tallies: []BlockerTally{
				{Severity: "P0", Kind: BlockerKindReviewFinding, Count: 1},
				{Severity: "P1", Kind: BlockerKindReviewFinding, Count: 1},
				{Severity: "P2", Kind: BlockerKindReviewFinding, Count: 2},
				{Severity: "P3", Kind: BlockerKindReviewFinding, Count: 1},
				{Kind: "check", Count: 1},
				{Kind: "conflict", Count: 1},
				{Kind: "no-review", Count: 1},
				{Kind: "operator-note", Count: 1},
			},
			files: []string{"a.go", "b.go", "c.go"},
		},
		{
			name:     "non-finding kinds drop severity and file",
			blockers: []Blocker{{Kind: "check", Severity: "P0", File: "ci.yml"}, {Kind: "check", Severity: "P1"}},
			tallies:  []BlockerTally{{Kind: "check", Count: 2}},
		},
		{
			name:     "files are distinct and skip empty paths",
			blockers: []Blocker{finding("P1", "a.go"), finding("P1", ""), finding("P1", "a.go"), finding("P2", "b.go")},
			tallies:  []BlockerTally{{Severity: "P1", Kind: BlockerKindReviewFinding, Count: 3}, {Severity: "P2", Kind: BlockerKindReviewFinding, Count: 1}},
			files:    []string{"a.go", "b.go"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tallies, files := BlockerTallies(tc.blockers)
			if !reflect.DeepEqual(tallies, tc.tallies) || !reflect.DeepEqual(files, tc.files) {
				t.Fatalf("got %+v %v, want %+v %v", tallies, files, tc.tallies, tc.files)
			}
		})
	}
}

func TestCompute_CarriesBlockerTalliesOnEveryKind(t *testing.T) {
	tallies, files := BlockerTallies([]Blocker{{Kind: BlockerKindReviewFinding, Severity: "P2", File: "a.go"}})
	for _, in := range []Input{
		{HasPR: true, PRState: PRStateOpen},
		{HasPR: true, PRState: PRStateOpen, Fix: &FixFact{AlertKey: "repair-exhausted", Sentence: "Automatic repair exhausted its attempts", Since: time.Unix(1, 0)}},
		{HasPR: true, PRState: PRStateMerged},
	} {
		in.BlockerTallies, in.BlockerFiles = tallies, files
		got := Compute(in)
		if len(got.BlockerTallies) != 1 || got.BlockerTallies[0].GetSeverity() != "P2" || got.BlockerTallies[0].GetKind() != BlockerKindReviewFinding || got.BlockerTallies[0].GetBlockerCount() != 1 || !reflect.DeepEqual(got.BlockerFiles, []string{"a.go"}) {
			t.Fatalf("%s: %v", got.GetKind(), got)
		}
	}
	if got := Compute(Input{HasPR: true, PRState: PRStateOpen}); got.BlockerTallies != nil || got.BlockerFiles != nil {
		t.Fatalf("no blockers served %v", got)
	}
	if got := Compute(Input{HasPR: true, PRState: PRStateMerged, BlockerTallies: tallies}).GetKind(); got != pb.AttentionKind_ATTENTION_KIND_DONE {
		t.Fatalf("tallies changed the kind: %s", got)
	}
}
