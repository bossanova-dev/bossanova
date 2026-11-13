package evidence

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
)

func parkAsksLine(head string, asks ...string) string {
	wire, _ := json.Marshal(struct {
		Head string   `json:"head"`
		Asks []string `json:"asks"`
	}{head, asks})
	return "<!-- boss-park-asks: " + string(wire) + " -->"
}

func TestParseParkAsks(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
		valid      bool
	}{
		{"last valid", parkAsksLine(headA, "old") + "\n" + parkAsksLine(headA, "  UI\n change ", "", "<area>") + "\n<!-- boss-park-asks: invalid -->", []string{"UI change", "<area>"}, true},
		{"no head", parkAsksLine("", "UI"), nil, false},
		{"no asks", parkAsksLine(headA, " \n "), nil, false},
		{"oversized name", parkAsksLine(headA, strings.Repeat("界", 301), strings.Repeat("界", 300)), []string{strings.Repeat("界", 300)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseParkAsks(tc.body)
			if ok != tc.valid || !slices.Equal(got.Asks, tc.want) || ok && got.Head != headA {
				t.Fatalf("ParseParkAsks = %+v, %v", got, ok)
			}
		})
	}
}

func TestParseParkAsksRejectsOversizedPayload(t *testing.T) {
	asks := make([]string, MaxParkAsks+1)
	for i := range asks {
		asks[i] = strings.Repeat("area ", 20) + string(rune('A'+i))
	}
	if got, ok := ParseParkAsks(parkAsksLine(headA, asks[:MaxParkAsks]...)); !ok || !slices.Equal(got.Asks, asks[:MaxParkAsks]) {
		t.Fatalf("maximum-sized payload = %+v, %v", got, ok)
	}
	if got, ok := ParseParkAsks(parkAsksLine(headA, asks...)); ok || got.Asks != nil {
		t.Fatalf("oversized payload returned a partial checklist: %+v, %v", got, ok)
	}
}

func TestReaderRejectsOversizedParkAsks(t *testing.T) {
	asks := make([]string, MaxParkAsks+1)
	for i := range asks {
		asks[i] = strings.Repeat("area ", 20) + string(rune('A'+i))
	}
	f := &fakeFinder{comments: []vcs.PRComment{{
		Body: parkAsksLine(headA, asks...), AuthorLogin: "operator", AuthorAssociation: "OWNER",
	}}}
	r, _ := newTestReader(f)
	r.Observe(t.Context(), parked(headA))
	if got := r.ParkAsks("s1", headA); got != nil {
		t.Fatalf("oversized payload served a partial checklist: %q", got)
	}
}

func TestReader_ParkAsksBelongToTrustedCurrentHead(t *testing.T) {
	f := &fakeFinder{comments: []vcs.PRComment{
		{Body: parkAsksLine(headA, "forged"), AuthorLogin: "stranger", AuthorAssociation: "NONE"},
		{Body: parkAsksLine(headB, "other head"), AuthorLogin: "operator", AuthorAssociation: "OWNER"},
		{Body: parkAsksLine(headA, "UI", "Full shared record name"), AuthorLogin: "operator", AuthorAssociation: "OWNER"},
		{Body: parkAsksLine(headA, "older"), AuthorLogin: "operator", AuthorAssociation: "OWNER"},
	}}
	r, clock := newTestReader(f)
	changes := 0
	r.SetOnChange(func(context.Context, string) { changes++ })
	r.Observe(t.Context(), parked(headA))
	want := []string{"UI", "Full shared record name"}
	if got := r.ParkAsks("s1", headA); !slices.Equal(got, want) {
		t.Fatalf("asks = %q", got)
	}
	if changes != 1 {
		t.Fatalf("changes = %d", changes)
	}
	cloned := r.ParkAsks("s1", headA)
	cloned[0] = "mutated"
	if got := r.ParkAsks("s1", headA); !slices.Equal(got, want) {
		t.Fatalf("mutable cache: %q", got)
	}
	if got := r.ParkAsks("s1", headB); got != nil {
		t.Fatalf("other head asks = %q", got)
	}
	f.comments = f.comments[:1]
	clock.t = clock.t.Add(DefaultRefreshInterval)
	r.Observe(t.Context(), parked(headA))
	if got := r.ParkAsks("s1", headA); got != nil || changes != 2 {
		t.Fatalf("untrusted asks = %q, changes = %d", got, changes)
	}
	f.comments = []vcs.PRComment{{Body: parkAsksLine(headA, "old head"), AuthorLogin: "operator", AuthorAssociation: "OWNER"}}
	r.Observe(t.Context(), parked(headB))
	if got := r.ParkAsks("s1", headB); got != nil {
		t.Fatalf("stale asks = %q", got)
	}
	var absent *Reader
	if got := absent.ParkAsks("s1", headA); got != nil {
		t.Fatalf("nil asks = %q", got)
	}
}
