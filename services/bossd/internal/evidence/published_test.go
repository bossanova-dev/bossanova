package evidence_test

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/rs/zerolog"
)

const publishedPage = "https://proof.example/acme/widgets/7/" + sha + "/index.html"

// countingFinder serves comments and counts the reads that reached it.
type countingFinder struct {
	comments []vcs.PRComment
	reads    int
}

func (f *countingFinder) FindPRCommentsWithMarker(context.Context, string, int, string) ([]vcs.PRComment, error) {
	f.reads++
	return f.comments, nil
}

func publishedReader(f *countingFinder) (*evidence.Reader, *[]string) {
	var changes []string
	r := evidence.NewReader(f, zerolog.Nop(), evidence.WithOnChange(func(_ context.Context, id string) {
		changes = append(changes, id)
	}))
	return r, &changes
}

func ownerComment(body string) vcs.PRComment {
	return vcs.PRComment{Body: body, AuthorAssociation: "OWNER"}
}

func proofWithPage(t *testing.T, head string, page any) string {
	p := proofPayload()
	p["head"], p["page"] = head, page
	return proofComment(t, p)
}

func TestRecordPublishedPageServesWithoutARead(t *testing.T) {
	f := &countingFinder{}
	r, changes := publishedReader(f)
	r.RecordPublishedPage(context.Background(), "s", 7, sha, publishedPage)
	latest, ok := r.LatestProof("s")
	if !ok || latest.Page != publishedPage || latest.Head != sha {
		t.Fatalf("LatestProof = %+v, %v; want the published page on %s", latest, ok, sha)
	}
	if proof, ok := r.Proof("s", sha); !ok || proof.Page != publishedPage {
		t.Fatalf("Proof = %+v, %v", proof, ok)
	}
	if f.reads != 0 || len(*changes) != 1 || (*changes)[0] != "s" {
		t.Fatalf("reads = %d, changes = %v; want no read and one change", f.reads, *changes)
	}
	r.RecordPublishedPage(context.Background(), "s", 7, sha, publishedPage)
	if len(*changes) != 1 {
		t.Fatalf("re-recording the same page notified again: %v", *changes)
	}
}

func TestRecordPublishedPageSurvivesALaterCommentRead(t *testing.T) {
	older := strings.Repeat("b", 40)
	sharedPage := "https://proof.example/shared"
	for _, tc := range []struct {
		name       string
		comments   []vcs.PRComment
		wantPage   string
		wantDigest bool
	}{
		{"no proof comment yet", nil, publishedPage, false},
		{"only an older head's proof", []vcs.PRComment{ownerComment(proofWithPage(t, older, "https://proof.example/older"))}, publishedPage, false},
		{"this head's proof without a page", []vcs.PRComment{ownerComment(proofWithPage(t, sha, nil))}, publishedPage, true},
		{"this head's proof with its own page", []vcs.PRComment{ownerComment(proofWithPage(t, sha, sharedPage))}, sharedPage, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &countingFinder{comments: tc.comments}
			r, _ := publishedReader(f)
			r.RecordPublishedPage(context.Background(), "s", 7, sha, publishedPage)
			r.Observe(context.Background(), evidence.Observation{SessionID: "s", PRNumber: 7, HeadSHA: sha, IsPROpen: true})
			if f.reads != 1 {
				t.Fatalf("reads = %d; want the first observation to read", f.reads)
			}
			latest, ok := r.LatestProof("s")
			if !ok || latest.Page != tc.wantPage || latest.Head != sha {
				t.Fatalf("LatestProof = %+v, %v; want page %q on %s", latest, ok, tc.wantPage, sha)
			}
			if proof, _ := r.Proof("s", sha); proof.Page != tc.wantPage || (proof.Digest != "") != tc.wantDigest {
				t.Fatalf("Proof = %+v; want page %q, digest kept %v", proof, tc.wantPage, tc.wantDigest)
			}
		})
	}
}

func TestRecordPublishedPageClearedByANewHead(t *testing.T) {
	next := strings.Repeat("c", 40)
	f := &countingFinder{}
	r, changes := publishedReader(f)
	r.Observe(context.Background(), evidence.Observation{SessionID: "s", PRNumber: 7, HeadSHA: sha, IsPROpen: true})
	r.RecordPublishedPage(context.Background(), "s", 7, sha, publishedPage)
	recorded := len(*changes)
	r.Observe(context.Background(), evidence.Observation{SessionID: "s", PRNumber: 7, HeadSHA: next, IsPROpen: true})
	if latest, ok := r.LatestProof("s"); ok {
		t.Fatalf("LatestProof = %+v; want the old head's page gone after the new head's read", latest)
	}
	if _, ok := r.Proof("s", sha); ok {
		t.Fatal("Proof still serves the old head")
	}
	if recorded != 1 || len(*changes) <= recorded {
		t.Fatalf("changes = %v (%d at the record); want a notice for the record and for the new head", *changes, recorded)
	}
}

func TestRecordPublishedPageIgnoresAnotherHeadOrPR(t *testing.T) {
	other := strings.Repeat("d", 40)
	for _, tc := range []struct {
		name string
		pr   int
		head string
		page string
	}{
		{"another head", 7, other, publishedPage},
		{"another PR", 8, sha, publishedPage},
		{"not an https page", 7, sha, "http://proof.example/index.html"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &countingFinder{}
			r, changes := publishedReader(f)
			r.Observe(context.Background(), evidence.Observation{SessionID: "s", PRNumber: 7, HeadSHA: sha, IsPROpen: true})
			r.RecordPublishedPage(context.Background(), "s", tc.pr, tc.head, tc.page)
			if latest, ok := r.LatestProof("s"); ok {
				t.Fatalf("LatestProof = %+v; want nothing recorded", latest)
			}
			if len(*changes) != 0 {
				t.Fatalf("changes = %v", *changes)
			}
		})
	}
}
