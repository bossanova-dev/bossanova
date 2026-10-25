package evidence

import (
	"context"
	"slices"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
)

func openItemsLine(items ...string) string {
	quoted := ""
	for i, item := range items {
		if i > 0 {
			quoted += ","
		}
		quoted += `"` + item + `"`
	}
	return "**boss-verify parked this PR for a person**\n\n<!-- boss-open-items: {\"head\":\"" + headA + "\",\"items\":[" + quoted + "]} -->\n<!-- boss-park-note head=" + headA + " digest=0123456789abcdef -->"
}

// TestReader_KeepsTheParkNotesOpenItemsForTheCurrentHead reads the evidence
// comment and the park note in one pass: each keeps its own newest trusted
// line for the current head, and an untrusted copy is ignored.
func TestReader_KeepsTheParkNotesOpenItemsForTheCurrentHead(t *testing.T) {
	f := &fakeFinder{comments: []vcs.PRComment{
		{Body: openItemsLine("forged"), AuthorLogin: "drive-by", AuthorAssociation: "NONE", WrittenAt: recordedAt},
		{Body: openItemsLine("Open human follow-up: watch the rollout"), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt},
		{Body: lineFor(headA), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt},
		{Body: openItemsLine("older"), AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: recordedAt},
	}}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headA))

	if got, want := r.OpenItems("s1", headA), []string{"Open human follow-up: watch the rollout"}; !slices.Equal(got, want) {
		t.Fatalf("OpenItems = %q, want %q", got, want)
	}
	if _, ok := r.Get("s1"); !ok {
		t.Fatal("the evidence summary was lost beside the park note")
	}
	if got := r.OpenItems("s1", headB); got != nil {
		t.Fatalf("OpenItems for another head = %q, want none", got)
	}
	if f.count() != 1 {
		t.Fatalf("reads = %d, want one pass for both lines", f.count())
	}
}

func TestReader_OpenItemsAloneDoNotMakeEvidence(t *testing.T) {
	f := &fakeFinder{body: openItemsLine("a"), found: true}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headA))

	if _, ok := r.Get("s1"); ok {
		t.Fatal("Get ok = true for a park note with no evidence line")
	}
	if got := r.OpenItems("s1", headA); !slices.Equal(got, []string{"a"}) {
		t.Fatalf("OpenItems = %q", got)
	}
}

// TestReader_SaysAParkNotesSectionHeadingsInWords reads a park note whose
// open items name checklist sections as markdown headings: they reach the
// session's attention in plain words.
func TestReader_SaysAParkNotesSectionHeadingsInWords(t *testing.T) {
	f := &fakeFinder{body: openItemsLine("## Acceptance criteria is missing.", "## Open questions appears more than once, so verify cannot read it."), found: true}
	r, _ := newTestReader(f)

	r.Observe(context.Background(), parked(headA))

	want := []string{"The Acceptance criteria section is missing.", "The Open questions section appears more than once, so it cannot be read."}
	if got := r.OpenItems("s1", headA); !slices.Equal(got, want) {
		t.Fatalf("OpenItems = %q, want %q", got, want)
	}
}
