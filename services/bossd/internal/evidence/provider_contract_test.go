package evidence_test

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	libevidence "github.com/recurser/bossalib/evidence"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/testharness"
)

const contractHead = "cccccccccccccccccccccccccccccccccccccccc"

// readThrough drives a Reader through p as the daemon wires it: the reader's
// comment source is the daemon's vcs.Provider, so every provider must keep the
// FindPRCommentsWithMarker contract the reader relies on.
func readThrough(p vcs.Provider) *evidence.Reader {
	r := evidence.NewReader(p, zerolog.Nop())
	r.Observe(context.Background(), evidence.Observation{
		SessionID: "s1", RepoPath: "https://github.com/o/r", PRNumber: 7,
		HeadSHA: contractHead, IsPROpen: true, IsVerifyActive: true,
	})
	return r
}

func contractComment(proven int, author, association string, at time.Time) vcs.PRComment {
	return vcs.PRComment{
		Body: "<!-- boss-verify-evidence -->\n" + libevidence.Format(libevidence.Summary{
			Head: contractHead, Proven: proven, Total: 5,
		}),
		AuthorLogin: author, AuthorAssociation: association, WrittenAt: at,
	}
}

// TestProviderContract_MockVCSProvider pins that the testharness provider
// returns marked comments newest first with their authors, which is what lets
// the reader take the newest trusted comment: stored oldest first here, the
// reader must still pick the newest trusted one over an older trusted one and
// a newer untrusted one.
func TestProviderContract_MockVCSProvider(t *testing.T) {
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	p := testharness.NewMockVCSProvider()
	p.PRComments = map[int][]vcs.PRComment{7: {
		contractComment(1, "operator", "OWNER", base),
		{Body: "unmarked", AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: base.Add(3 * time.Hour)},
		contractComment(4, "operator", "MEMBER", base.Add(time.Hour)),
		contractComment(5, "drive-by", "NONE", base.Add(2*time.Hour)),
	}}

	entry, ok := readThrough(p).Entry("s1")
	if !ok {
		t.Fatal("Entry ok = false; want the newest trusted evidence comment")
	}
	if entry.Summary.Proven != 4 || !entry.RecordedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("Entry = %+v; want the trusted comment written at %v (proven 4)", entry, base.Add(time.Hour))
	}
}

// TestProviderContract_StubProvider pins that a provider that cannot read
// comments yields no evidence rather than a fabricated summary.
func TestProviderContract_StubProvider(t *testing.T) {
	if _, ok := readThrough(testharness.NewStubProvider()).Entry("s1"); ok {
		t.Fatal("Entry ok = true through a provider that cannot read comments")
	}
}
