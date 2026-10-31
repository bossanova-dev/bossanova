package attention

import (
	"bytes"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

type parkVerdictRow struct {
	Codes        []string `json:"codes"`
	Label        string   `json:"label"`
	EvidenceURL  string   `json:"evidenceUrl"`
	VideoSeconds int      `json:"videoSeconds"`
	OpenItems    int      `json:"openItems"`
	State        string   `json:"state"`
	Wait         string   `json:"wait"`
	Reason       string   `json:"reason"`
	Evidence     string   `json:"evidence"`
	Sentence     string   `json:"sentence"`
}

// TestParkVerdictsGolden publishes the verdict contract for the JS and web mirrors.
// Regenerate from lib/bossalib with BOSS_UPDATE_PARK_VERDICTS=1 go test ./attention -run TestParkVerdictsGolden.
func TestParkVerdictsGolden(t *testing.T) {
	codes := make([]string, 0, len(knownParkCodes))
	for code := range knownParkCodes {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	var rows []parkVerdictRow
	for _, code := range append(codes, "brand-new-code") {
		rows = append(rows, parkVerdictRow{Codes: []string{code}})
		if code == "always-human-path" || strings.HasPrefix(code, humanReviewPrefix) {
			rows = append(rows, parkVerdictRow{Codes: []string{code}, Label: "the verify judge (cannot grade itself)"})
		}
	}
	rows = append(rows,
		parkVerdictRow{Codes: []string{"human-review:ui"}, EvidenceURL: "https://example.com/evidence.mp4", VideoSeconds: 83},
		parkVerdictRow{Codes: []string{"human-review:ui"}, EvidenceURL: "https://example.com/evidence.mp4"},
		parkVerdictRow{Codes: []string{"human-review:ui"}, VideoSeconds: 83},
		parkVerdictRow{Codes: []string{"ledger-open"}, OpenItems: 1},
		parkVerdictRow{Codes: []string{"ledger-open"}, OpenItems: 2},
		parkVerdictRow{Codes: []string{"partial-build,human-review:ui"}},
		parkVerdictRow{Codes: []string{"repair-exhausted", "ledger-open", "human-review:ui"}, OpenItems: 2},
	)
	for i := range rows {
		r := &rows[i]
		var ev *Evidence
		if r.EvidenceURL != "" || r.VideoSeconds != 0 {
			ev = &Evidence{URL: r.EvidenceURL, VideoDuration: time.Duration(r.VideoSeconds) * time.Second}
		}
		v, ok := ParkVerdict(r.Codes, r.Label, ev, r.OpenItems)
		if !ok {
			t.Fatalf("no verdict for %v", r.Codes)
		}
		r.State, r.Wait, r.Reason, r.Evidence, r.Sentence = v.State, v.Wait, v.Reason, v.Evidence, v.Sentence()
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	const path = "testdata/park_verdicts.json"
	if os.Getenv("BOSS_UPDATE_PARK_VERDICTS") != "" {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with BOSS_UPDATE_PARK_VERDICTS=1", path)
	}
}
