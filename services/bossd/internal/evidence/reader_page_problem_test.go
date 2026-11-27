package evidence_test

import (
	"strings"
	"testing"

	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/rs/zerolog"
)

// missingNote is verify's proof-missing note for head, as verify-proof-missing.mjs writes it.
func missingNote(head, line string) string {
	return "The verdict for `" + head[:12] + "` stands, but the proof is incomplete:\n\n- `page`: `upload-failed`\n\n" +
		line + "\n\n<!-- boss-proof-missing head=" + head + " digest=0123456789abcdef -->"
}

func problemsLine(head, problems string) string {
	return `<!-- boss-proof-problems: {"head":"` + head + `","problems":` + problems + `} -->`
}

func TestReaderProofPageProblemNamesWhyAHeadHasNoPage(t *testing.T) {
	block := "## Verify"
	withPage := proofPayload()
	withPage["page"] = "https://example.test/proof"
	notConfigured := problemsLine(sha, `[{"part":"check-run","reason":"check-run-failed"},{"part":"page","reason":"upload-failed","detail":"hosted proof pages are not configured <"}]`)
	other := strings.Repeat("b", 40)
	for _, tt := range []struct {
		name     string
		comments []vcs.PRComment
		want     evidence.PageProblem
	}{
		{"page problem", []vcs.PRComment{
			{Body: missingNote(sha, notConfigured), AuthorAssociation: "OWNER"},
			{Body: coreProofNote(t, block, proofPayload()), AuthorAssociation: "OWNER"},
		}, evidence.PageProblem{Reason: "upload-failed", Detail: "hosted proof pages are not configured <"}},
		{"no missing note", []vcs.PRComment{{Body: coreProofNote(t, block, proofPayload()), AuthorAssociation: "OWNER"}}, evidence.PageProblem{}},
		{"only a check-run problem", []vcs.PRComment{
			{Body: missingNote(sha, problemsLine(sha, `[{"part":"check-run","reason":"check-run-failed"}]`)), AuthorAssociation: "OWNER"},
			{Body: coreProofNote(t, block, proofPayload()), AuthorAssociation: "OWNER"},
		}, evidence.PageProblem{}},
		{"another head", []vcs.PRComment{
			{Body: missingNote(other, problemsLine(other, `[{"part":"page","reason":"upload-failed"}]`)), AuthorAssociation: "OWNER"},
			{Body: coreProofNote(t, block, proofPayload()), AuthorAssociation: "OWNER"},
		}, evidence.PageProblem{}},
		{"untrusted author", []vcs.PRComment{
			{Body: missingNote(sha, notConfigured), AuthorAssociation: "NONE"},
			{Body: coreProofNote(t, block, proofPayload()), AuthorAssociation: "OWNER"},
		}, evidence.PageProblem{}},
		{"a page was published", []vcs.PRComment{
			{Body: missingNote(sha, notConfigured), AuthorAssociation: "OWNER"},
			{Body: coreProofNote(t, block, withPage), AuthorAssociation: "OWNER"},
		}, evidence.PageProblem{}},
		{"malformed line", []vcs.PRComment{
			{Body: missingNote(sha, problemsLine(sha, `[{"part":"page"}]`)), AuthorAssociation: "OWNER"},
			{Body: coreProofNote(t, block, proofPayload()), AuthorAssociation: "OWNER"},
		}, evidence.PageProblem{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := evidence.NewReader(&proofCommentFinder{comments: tt.comments}, zerolog.Nop())
			r.Observe(t.Context(), evidence.Observation{SessionID: "s", PRNumber: 1, HeadSHA: sha, IsPROpen: true})
			got, ok := r.LatestProof("s")
			if !ok || got.PageProblem != tt.want {
				t.Fatalf("LatestProof page problem = %+v %v; want %+v", got.PageProblem, ok, tt.want)
			}
		})
	}
}
