package evidence_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/rs/zerolog"
)

type proofCommentFinder struct {
	comments []vcs.PRComment
	err      error
}

func (f *proofCommentFinder) FindPRCommentsWithMarker(_ context.Context, _ string, _ int, marker string) ([]vcs.PRComment, error) {
	if marker != evidence.CommentMarker {
		return nil, errors.New("unexpected marker")
	}
	return f.comments, f.err
}

func TestReaderProofCombinesRecords(t *testing.T) {
	p := proofPayload()
	p["page"] = "https://example.test/proof"
	p["asks"] = []string{"ui"}
	p["level"] = "moderate"
	proof := proofComment(t, p)
	video := evidence.Format(evidence.Summary{Head: sha, VideoURL: "http://example.test/video", VideoDuration: time.Second})
	noVideo := evidence.Format(evidence.Summary{Head: sha})
	at := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, body                           string
		legacy, hasMedia, hasProof, hasEntry bool
	}{
		{"both", proof + video, false, true, true, true},
		{"legacy video", video, true, true, false, true},
		{"legacy no video", noVideo, true, false, false, true},
		{"proof only", proof, false, false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := &proofCommentFinder{comments: []vcs.PRComment{{Body: tt.body, AuthorAssociation: "OWNER", WrittenAt: at}}}
			r := evidence.NewReader(f, zerolog.Nop())
			r.Observe(context.Background(), evidence.Observation{SessionID: "s", HeadSHA: sha})
			got, ok := r.Proof("s", strings.ToUpper(sha))
			if !ok || got.Head != sha || got.Legacy != tt.legacy || !got.RecordedAt.Equal(at) {
				t.Fatalf("record: %+v %v", got, ok)
			}
			if tt.hasMedia {
				if !reflect.DeepEqual(got.Media, []evidence.Media{{Kind: "video", URL: "http://example.test/video", Duration: time.Second}}) {
					t.Fatalf("media: %+v", got.Media)
				}
			} else if len(got.Media) != 0 {
				t.Fatalf("unexpected media %+v", got.Media)
			}
			if tt.hasProof {
				if got.Page != "https://example.test/proof" || got.Level != "moderate" || !reflect.DeepEqual(got.Asks, []string{"ui"}) || got.Digest != p["digest"] {
					t.Fatalf("proof: %+v", got)
				}
			} else if got.Page != "" || got.Level != "" || len(got.Asks) != 0 || got.Digest != "" {
				t.Fatalf("unexpected proof fields %+v", got)
			}
			if _, ok := r.Entry("s"); ok != tt.hasEntry {
				t.Fatalf("Entry ok = %v", ok)
			}
			if _, ok := r.Proof("s", strings.Repeat("b", 40)); ok {
				t.Fatal("returned another head")
			}
			if len(got.Asks) > 0 {
				got.Asks[0] = "changed"
			}
			if len(got.Media) > 0 {
				got.Media[0].URL = "changed"
			}
			next, _ := r.Proof("s", sha)
			if len(next.Asks) > 0 && next.Asks[0] != "ui" || len(next.Media) > 0 && next.Media[0].URL != "http://example.test/video" {
				t.Fatal("cache mutated through accessor")
			}
		})
	}
}

func TestReaderProofTrustAndHead(t *testing.T) {
	p := proofPayload()
	trusted := proofComment(t, p)
	p["level"] = "low"
	untrusted := proofComment(t, p)
	old := proofPayload()
	old["head"] = strings.Repeat("b", 40)
	for _, tt := range []struct {
		name     string
		comments []vcs.PRComment
		want     bool
	}{
		{"trusted behind untrusted", []vcs.PRComment{{Body: untrusted, AuthorAssociation: "NONE"}, {Body: trusted, AuthorAssociation: "OWNER"}}, true},
		{"untrusted only", []vcs.PRComment{{Body: untrusted, AuthorAssociation: "NONE"}}, false},
		{"old head", []vcs.PRComment{{Body: proofComment(t, old), AuthorAssociation: "OWNER"}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := evidence.NewReader(&proofCommentFinder{comments: tt.comments}, zerolog.Nop())
			r.Observe(context.Background(), evidence.Observation{SessionID: "s", HeadSHA: sha})
			got, ok := r.Proof("s", sha)
			if ok != tt.want || ok && got.Level != "high" {
				t.Fatalf("Proof: %+v %v", got, ok)
			}
		})
	}
}

func TestReaderProofLifecycle(t *testing.T) {
	p := proofPayload()
	at := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	video := evidence.Format(evidence.Summary{Head: sha, VideoURL: "https://example.test/v"})
	f := &proofCommentFinder{comments: []vcs.PRComment{{Body: proofComment(t, p) + video, AuthorAssociation: "OWNER", WrittenAt: at}}}
	now := at
	changes := 0
	r := evidence.NewReader(f, zerolog.Nop(), evidence.WithClock(func() time.Time { return now }), evidence.WithOnChange(func(context.Context, string) { changes++ }))
	obs := evidence.Observation{SessionID: "s", HeadSHA: sha, IsPROpen: true, IsVerifyActive: true}
	refresh := func() { now = now.Add(evidence.DefaultRefreshInterval); r.Observe(context.Background(), obs) }
	refresh()
	if changes != 1 {
		t.Fatalf("initial changes = %d", changes)
	}
	refresh()
	if changes != 1 {
		t.Fatalf("identical reread changes = %d", changes)
	}
	p["level"] = "low"
	f.comments[0].Body = proofComment(t, p) + video
	refresh()
	if changes != 2 {
		t.Fatalf("proof-only change count = %d", changes)
	}
	f.err = errors.New("offline")
	refresh()
	if got, ok := r.Proof("s", sha); !ok || got.Level != "low" {
		t.Fatalf("failed read lost proof: %+v %v", got, ok)
	}
	f.err = nil
	f.comments = nil
	refresh()
	if _, ok := r.Proof("s", sha); ok || changes != 3 {
		t.Fatalf("empty reread retained proof; changes = %d", changes)
	}
	f.comments = []vcs.PRComment{{Body: proofComment(t, p), AuthorAssociation: "OWNER"}}
	refresh()
	if _, ok := r.Proof("s", sha); !ok {
		t.Fatal("did not restore proof")
	}
	obs.HeadSHA = strings.Repeat("b", 40)
	refresh()
	if _, ok := r.Proof("s", sha); ok {
		t.Fatal("old head record survived")
	}
	if changes != 5 {
		t.Fatalf("proof-only head invalidation count = %d", changes)
	}
}
