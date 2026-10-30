package evidence_test

import (
	"os"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/evidence"
)

// TestParse_TheVerifyRenderersGoldenComment pins the cross-language contract:
// testdata/js-evidence-comment.txt is the comment the verify skill's renderer
// (.claude/skills/verify/lib/pr-evidence.mjs renderComment) writes for a fixed
// manifest, and that renderer's test asserts it still renders exactly this.
// Parse must read the summary the renderer meant, including the HTML-escaped
// "&" in the video URL.
func TestParse_TheVerifyRenderersGoldenComment(t *testing.T) {
	body, err := os.ReadFile("testdata/js-evidence-comment.txt")
	if err != nil {
		t.Fatalf("read golden comment: %v", err)
	}

	got, ok := evidence.Parse(string(body))
	if !ok {
		t.Fatal("Parse ok = false for the verify renderer's comment")
	}
	want := evidence.Summary{
		Head:          sha,
		VideoURL:      "https://proof.example.test/golden/video.mp4",
		VideoDuration: 134 * time.Second,
		Proven:        2,
		Total:         3,
	}
	if got != want {
		t.Fatalf("Parse = %+v, want %+v", got, want)
	}
}

// TestParseProofTheVerifyRenderersGoldenComment shares the writer's real fixture.
func TestParseProofTheVerifyRenderersGoldenComment(t *testing.T) {
	body, err := os.ReadFile("testdata/js-evidence-comment.txt")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := evidence.ParseProof(string(body))
	if !ok || got.Head != sha || got.Level != evidence.LevelCannotAssess ||
		got.Digest != "sha256:ec976fdeef241ca9dbd55d88629af7b56541e8776665999faa29703dd805e697" || got.Page != "" || len(got.Asks) != 0 {
		t.Fatalf("golden proof: %+v, %v", got, ok)
	}
}
