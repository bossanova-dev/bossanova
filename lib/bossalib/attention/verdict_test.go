package attention

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestParkVerdict_Sessions_AC215001(t *testing.T) {
	for _, tc := range []struct {
		codes []string
		label string
		ev    *Evidence
		items int
		want  string
	}{
		{[]string{"always-human-path"}, "the verify judge (cannot grade itself)", nil, 0, "Not verified · waiting for you · this PR changes the verify judge (cannot grade itself)"},
		{[]string{"always-human-path"}, "", nil, 0, "Not verified · waiting for you · touches a path that always needs a person's sign-off"},
		{[]string{"human-review:ui"}, "", &Evidence{URL: "https://example.com/video", VideoDuration: 83 * time.Second}, 0, "Not verified · waiting for you · a UI change · evidence video (1:23)"},
		{[]string{"repair-exhausted"}, "", nil, 0, "Not verified · blocked · automatic repair gave up on this PR"},
		{[]string{"partial-build,human-review:ui"}, "", nil, 0, "Not verified · blocked · only part of the ticket was finished, so the PR is held"},
		{[]string{"human-merge"}, "", nil, 0, "Verified · waiting for you · verify passed; this PR's policy leaves the merge to a person"},
		{[]string{"ledger-open"}, "", nil, 2, "Not verified · blocked · the PR checklist has 2 open items"},
		{[]string{"human-review:security"}, "authentication", nil, 0, "Not verified · waiting for you · this PR changes authentication"},
		{[]string{"brand-new"}, "", nil, 0, "Not verified · blocked · an issue this version of boss does not recognise"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			got, ok := ParkVerdict(tc.codes, tc.label, tc.ev, tc.items)
			if !ok || got.Sentence() != tc.want {
				t.Fatalf("ParkVerdict = %+v, %v; want %q", got, ok, tc.want)
			}
		})
	}
	for _, codes := range [][]string{nil, {"stack-busy"}, {"waiting:base"}} {
		if got, ok := ParkVerdict(codes, "", nil, 0); ok || got != (Verdict{}) {
			t.Errorf("waiting verdict = %+v, %v", got, ok)
		}
	}
}

func TestComputeParkLabel(t *testing.T) {
	const label = "the verify judge (cannot grade itself)"
	got := Compute(Input{ParkCodes: []string{"always-human-path"}, ParkLabel: label})
	if got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW || got.GetParkLabel() != label || got.GetSentence() != "Not verified · waiting for you · this PR changes "+label {
		t.Fatalf("park attention = %v", got)
	}
	if ParkCodeCategory("always-human-path") != "protected path" || ParkCodeCategory("human-review:security") != "security" {
		t.Fatal("park categories disagree")
	}
	for _, in := range []Input{{}, {QuestionChatID: "chat"}, {HasPR: true, PRState: PRStateMerged}} {
		in.ParkLabel = label
		if got := Compute(in); got.GetParkLabel() != "" {
			t.Errorf("non-park has label: %v", got)
		}
	}
}
