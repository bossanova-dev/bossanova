package attention

import (
	"bytes"
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type confidenceSentenceRow struct {
	Level     string   `json:"level"`
	LoweredBy []string `json:"loweredBy"`
	Sentence  string   `json:"sentence"`
}

// TestConfidenceSentenceGolden publishes the level sentence both clients show.
// Regenerate with BOSS_UPDATE_CONFIDENCE_SENTENCES=1 go test ./attention -run TestConfidenceSentenceGolden.
func TestConfidenceSentenceGolden(t *testing.T) {
	reasonSets := [][]string{
		nil,
		{"Changes Infrastructure. Reviewing this area would raise this."},
		{"Changes Infrastructure. Reviewing this area would raise this.", "The review did not run. Rerunning it would raise this.", "Evidence is missing. Collecting it would raise this."},
		{"CI failed. Resolving it would raise this."},
		{"", "  Évidence is missing. Collecting it would raise this.  ", " "},
	}
	var rows []confidenceSentenceRow
	for _, value := range slices.Sorted(maps.Keys(pb.ConfidenceLevel_name)) {
		level := pb.ConfidenceLevel(value)
		for _, reasons := range reasonSets {
			rows = append(rows, confidenceSentenceRow{Level: level.String(), LoweredBy: reasons, Sentence: ConfidenceSentence(level, reasons)})
		}
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/confidence_sentences.json"
	if os.Getenv("BOSS_UPDATE_CONFIDENCE_SENTENCES") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with BOSS_UPDATE_CONFIDENCE_SENTENCES=1", path)
	}
}

func TestConfidenceSentence(t *testing.T) {
	const infra = "Changes Infrastructure. Reviewing this area would raise this."
	for _, tc := range []struct {
		level   pb.ConfidenceLevel
		reasons []string
		want    string
	}{
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, []string{infra}, "Moderate: changes Infrastructure. Reviewing this area would raise this."},
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_MODERATE, nil, "Moderate."},
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_HIGH, nil, "High."},
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_LOW, []string{"Large diff.", "Auth path."}, "Low: large diff. Auth path."},
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_BLOCKED, []string{"CI failed."}, "Blocked: CI failed."},
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_CANNOT_ASSESS, []string{" ", ""}, "Cannot assess."},
		{pb.ConfidenceLevel_CONFIDENCE_LEVEL_UNSPECIFIED, []string{infra}, ""},
		{pb.ConfidenceLevel(99), []string{infra}, ""},
	} {
		if got := ConfidenceSentence(tc.level, tc.reasons); got != tc.want {
			t.Errorf("ConfidenceSentence(%v, %q) = %q, want %q", tc.level, tc.reasons, got, tc.want)
		}
	}
}
