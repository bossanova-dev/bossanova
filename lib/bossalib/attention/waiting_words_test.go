package attention_test

import (
	"slices"
	"testing"

	"github.com/recurser/bossalib/attention"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestWaitingWords checks a park's waiting codes read as the in-progress label
// does, in report order and once each, with operator codes left out.
func TestWaitingWords(t *testing.T) {
	for _, tc := range []struct {
		raw  []string
		want []string
	}{
		{nil, nil},
		{[]string{"base-ci-red"}, []string{"waiting for main CI"}},
		{[]string{"stack-busy,base-ci-red"}, []string{"waiting for the test stack", "waiting for main CI"}},
		{[]string{"waiting: base-ci-red", "base-ci-red"}, []string{"waiting for main CI"}},
		{[]string{"waiting:release freeze"}, []string{"waiting: release freeze"}},
		{[]string{"human-review:ui", "stack-busy"}, []string{"waiting for the test stack"}},
	} {
		if got := attention.WaitingWords(tc.raw); !slices.Equal(got, tc.want) {
			t.Errorf("WaitingWords(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// TestWaitingWordsMatchTheProgressLabel checks the timeline's words for a lone
// waiting code are the label the session row shows for it.
func TestWaitingWordsMatchTheProgressLabel(t *testing.T) {
	for _, code := range []string{"base-ci-red", "stack-busy", "waiting:x"} {
		row := attention.Compute(attention.Input{ParkCodes: []string{code}})
		if row.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS {
			t.Fatalf("%s: kind = %v", code, row.GetKind())
		}
		if got := attention.WaitingWords([]string{code}); !slices.Equal(got, []string{row.GetProgressLabel()}) {
			t.Errorf("%s: words %q, row label %q", code, got, row.GetProgressLabel())
		}
	}
}
