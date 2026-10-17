package github

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/vcs"
)

const prCommentMarker = "<!-- boss-evidence:"

func TestFindPRCommentsWithMarker_MarkedCommentsNewestWrittenFirst(t *testing.T) {
	var gotArgs []string
	// Two pages, as `gh api --paginate` writes them: arrays back to back. The
	// marked comment edited last comes first even though an older one was
	// created later, because a reader wants the most recently written body;
	// every marked comment is returned with its author so the reader can
	// judge trust, and with its creation time and id, which the reader needs
	// to order operator records (a record without them reads as a send-back).
	pages := `[
		{"body":"unrelated","user":{"login":"a"},"author_association":"OWNER","created_at":"2026-10-13T23:00:00Z","updated_at":"2026-10-13T23:00:00Z"},
		{"id":11,"body":"old ` + prCommentMarker + ` {} -->","user":{"login":"operator"},"author_association":"OWNER","created_at":"2026-10-13T23:10:00Z","updated_at":"2026-10-13T23:40:00Z"}
	][
		{"id":12,"body":"stale ` + prCommentMarker + ` {} -->","user":{"login":"drive-by"},"author_association":"NONE","created_at":"2026-10-13T23:20:00Z","updated_at":"2026-10-13T23:20:00Z"},
		{"body":"later, unmarked","user":{"login":"a"},"author_association":"OWNER","created_at":"2026-10-13T23:50:00Z","updated_at":"2026-10-13T23:50:00Z"}
	]`
	fakeGH := func(_ context.Context, args ...string) (string, error) {
		gotArgs = args
		return pages, nil
	}
	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	got, err := p.FindPRCommentsWithMarker(context.Background(), "https://github.com/owner/repo", 2979, prCommentMarker)
	if err != nil {
		t.Fatalf("FindPRCommentsWithMarker: %v", err)
	}
	want := []vcs.PRComment{
		{Body: "old " + prCommentMarker + " {} -->", AuthorLogin: "operator", AuthorAssociation: "OWNER", WrittenAt: time.Date(2026, 10, 13, 23, 40, 0, 0, time.UTC), CreatedAt: time.Date(2026, 10, 13, 23, 10, 0, 0, time.UTC), ID: 11},
		{Body: "stale " + prCommentMarker + " {} -->", AuthorLogin: "drive-by", AuthorAssociation: "NONE", WrittenAt: time.Date(2026, 10, 13, 23, 20, 0, 0, time.UTC), CreatedAt: time.Date(2026, 10, 13, 23, 20, 0, 0, time.UTC), ID: 12},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d comments %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].Body != want[i].Body || got[i].AuthorLogin != want[i].AuthorLogin ||
			got[i].AuthorAssociation != want[i].AuthorAssociation || !got[i].WrittenAt.Equal(want[i].WrittenAt) ||
			!got[i].CreatedAt.Equal(want[i].CreatedAt) || got[i].ID != want[i].ID {
			t.Errorf("comment %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "repos/owner/repo/issues/2979/comments") || !strings.Contains(joined, "--paginate") {
		t.Errorf("gh args = %q, want a paginated issue-comments read for owner/repo#2979", joined)
	}
}

func TestFindPRCommentsWithMarker_NoneFound(t *testing.T) {
	fakeGH := func(_ context.Context, _ ...string) (string, error) {
		return `[{"body":"nothing here","created_at":"2026-10-13T23:00:00Z","updated_at":"2026-10-13T23:00:00Z"}][]`, nil
	}
	p := New(zerolog.Nop(), WithRunGH(fakeGH))

	got, err := p.FindPRCommentsWithMarker(context.Background(), "https://github.com/owner/repo", 1, prCommentMarker)
	if err != nil {
		t.Fatalf("FindPRCommentsWithMarker: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none", got)
	}
}

func TestFindPRCommentsWithMarker_Errors(t *testing.T) {
	p := New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) {
		return "", errors.New("boom")
	}))
	if _, err := p.FindPRCommentsWithMarker(context.Background(), "https://github.com/owner/repo", 1, prCommentMarker); err == nil {
		t.Fatal("gh failure: err = nil, want an error")
	}

	p = New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) {
		return "not json", nil
	}))
	if _, err := p.FindPRCommentsWithMarker(context.Background(), "https://github.com/owner/repo", 1, prCommentMarker); err == nil {
		t.Fatal("unparseable response: err = nil, want an error")
	}

	called := false
	p = New(zerolog.Nop(), WithRunGH(func(context.Context, ...string) (string, error) {
		called = true
		return "[]", nil
	}))
	if _, err := p.FindPRCommentsWithMarker(context.Background(), "https://github.com/owner/repo", 1, "  "); err == nil || called {
		t.Fatalf("empty marker: err = %v, gh called = %v; want an error before any read", err, called)
	}
}

// TestDecodePaginatedPages pins the shared `gh api --paginate` decode both the
// review and the PR comment reads use: pages written back to back merge in
// order, and each caller's errors keep its own "parse <what>" prefix.
func TestDecodePaginatedPages(t *testing.T) {
	got, err := decodePaginatedPages[rawIssueComment](`[{"body":"a"}][][{"body":"b"},{"body":"c"}]`, "PR comments")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var bodies []string
	for _, c := range got {
		bodies = append(bodies, c.Body)
	}
	if strings.Join(bodies, ",") != "a,b,c" {
		t.Fatalf("bodies = %v, want a,b,c across pages", bodies)
	}

	for _, tc := range []struct{ out, want string }{
		{"", "parse reviews: empty response"},
		{"not json", "parse reviews: "},
	} {
		_, err := decodePaginatedPages[rawReview](tc.out, "reviews")
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Fatalf("decode %q: err = %v, want prefix %q", tc.out, err, tc.want)
		}
	}
	if _, err := decodePaginatedPages[rawIssueComment]("", "PR comments"); err == nil || err.Error() != "parse PR comments: empty response" {
		t.Fatalf("empty comments: err = %v, want the PR comments prefix", err)
	}
}
