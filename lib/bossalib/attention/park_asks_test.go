package attention

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"testing"
)

type parkAskRow struct {
	Codes       []string `json:"codes"`
	Label       string   `json:"label"`
	EvidenceURL string   `json:"evidenceUrl"`
	Head        string   `json:"head"`
	Question    string   `json:"question"`
	Items       []string `json:"items"`
}

// TestParkAsksGolden publishes the operator ask contract shared by both clients.
// Regenerate with BOSS_UPDATE_PARK_ASKS=1 go test ./attention -run TestParkAsksGolden.
func TestParkAsksGolden(t *testing.T) {
	var codes []string
	for code, info := range knownParkCodes {
		if info.signOff {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)
	var rows []parkAskRow
	for _, code := range codes {
		for _, label := range []string{"", "the verify judge (cannot grade itself)"} {
			rows = append(rows, parkAskRow{Codes: []string{code}, Label: label, Head: "abc123456789"})
		}
	}
	rows = append(rows, parkAskRow{Codes: []string{"human-review:ui"}, EvidenceURL: "https://example.com/evidence.mp4", Head: "abc123456789"}, parkAskRow{Codes: []string{"human-review:ui", "human-review:database"}, Head: "abc123456789"}, parkAskRow{Codes: []string{"human-review:ui"}})
	for i := range rows {
		r := &rows[i]
		var ok bool
		r.Question, r.Items, ok = ReviewAsk(r.Codes, r.Label, r.EvidenceURL != "", r.Head)
		if !ok {
			t.Fatalf("no ask for %v", r.Codes)
		}
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/park_asks.json"
	if os.Getenv("BOSS_UPDATE_PARK_ASKS") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with BOSS_UPDATE_PARK_ASKS=1", path)
	}
}
