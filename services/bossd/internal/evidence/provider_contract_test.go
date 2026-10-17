package evidence_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/rs/zerolog"

	libevidence "github.com/recurser/bossalib/evidence"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/vcs/github"
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

// restComment is one REST issue comment as GitHub returns it.
type restComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	User struct {
		Login string `json:"login"`
	} `json:"user"`
	AuthorAssociation string `json:"author_association"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

func operatorComment(id int64, kind, created, updated string) restComment {
	c := restComment{ID: id, AuthorAssociation: "OWNER", CreatedAt: created, UpdatedAt: updated}
	c.User.Login = "operator"
	c.Body = "**Recorded by the operator**\n\n<!-- boss-" + kind + " head=" + contractHead + " -->"
	return c
}

// TestProviderContract_GitHubOperatorRecordIsTheNewestPosted feeds REST
// comments through the GitHub provider into the reader and pins that the
// operator record posted last decides the head, the larger comment id breaking
// a tie, however recently an older record was edited. That is the order
// verify's operatorDecision uses, so bossd and verify agree on whether the
// head was sent back.
func TestProviderContract_GitHubOperatorRecordIsTheNewestPosted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		comments []restComment
		want     bool
	}{
		{"an edited old approval does not lift a newer send-back", []restComment{
			operatorComment(1, "approve", "2026-10-13T10:00:00Z", "2026-10-13T12:00:00Z"),
			operatorComment(2, "send-back", "2026-10-13T11:00:00Z", "2026-10-13T11:00:00Z"),
		}, true},
		{"an edited old send-back does not undo a newer approval", []restComment{
			operatorComment(1, "send-back", "2026-10-13T10:00:00Z", "2026-10-13T12:00:00Z"),
			operatorComment(2, "approve", "2026-10-13T11:00:00Z", "2026-10-13T11:00:00Z"),
		}, false},
		{"in the same second the later comment id decides, approved", []restComment{
			operatorComment(5, "send-back", "2026-10-13T10:00:00Z", "2026-10-13T12:00:00Z"),
			operatorComment(6, "approve", "2026-10-13T10:00:00Z", "2026-10-13T10:00:00Z"),
		}, false},
		{"in the same second the later comment id decides, sent back", []restComment{
			operatorComment(5, "approve", "2026-10-13T10:00:00Z", "2026-10-13T12:00:00Z"),
			operatorComment(6, "send-back", "2026-10-13T10:00:00Z", "2026-10-13T10:00:00Z"),
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := json.Marshal(tc.comments)
			if err != nil {
				t.Fatal(err)
			}
			p := github.New(zerolog.Nop(), github.WithRunGH(func(context.Context, ...string) (string, error) {
				return string(page), nil
			}))
			if got := readThrough(p).SentBack("s1", contractHead); got != tc.want {
				t.Fatalf("SentBack = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProviderContract_MockVCSProviderUndatedRecordIsNotAnApproval pins the
// fail-safe for a provider that leaves CreatedAt unset: an approval last
// written after a send-back would decide the head if write order stood in for
// posting order, so an undated record reads as a send-back instead.
func TestProviderContract_MockVCSProviderUndatedRecordIsNotAnApproval(t *testing.T) {
	base := time.Date(2026, 10, 13, 11, 0, 0, 0, time.UTC)
	record := func(kind string, at time.Time) vcs.PRComment {
		return vcs.PRComment{
			Body:        "**Recorded by the operator**\n\n<!-- boss-" + kind + " head=" + contractHead + " -->",
			AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: at,
		}
	}
	p := testharness.NewMockVCSProvider()
	p.PRComments = map[int][]vcs.PRComment{7: {
		record("send-back", base),
		record("approve", base.Add(time.Hour)),
	}}
	if !readThrough(p).SentBack("s1", contractHead) {
		t.Fatal("SentBack = false for undated records; want the head held as sent back")
	}
}
