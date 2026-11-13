package attention

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
)

type parkAskRow struct {
	Codes       []string `json:"codes"`
	Label       string   `json:"label"`
	Asks        []string `json:"asks"`
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
	rows = append(rows,
		parkAskRow{Codes: []string{"human-review:ui"}, Label: "moderate: UI, Shared record", Asks: []string{"UI", "Shared record"}},
		parkAskRow{Codes: []string{"always-human-path", "human-review:ui"}, Label: "low: UI, Persistent write to shared referrer record, Silent clearing of stored referrer de…", Asks: []string{"UI", "Persistent write to shared referrer record", "Silent clearing of stored referrer details", "Fourth area"}},
		parkAskRow{Codes: []string{"always-human-path", "human-review:ui"}, Label: "low: UI, Shared record, Clearing stored refer…", Asks: []string{"UI", "Shared record"}},
		parkAskRow{Codes: []string{"human-review:ui"}, Label: "low: UI, Shared record, Clearing details +1", Asks: []string{"UI", "Shared record", "Clearing details", "Fourth area"}},
		parkAskRow{Codes: []string{"always-human-path"}, Label: "the verify judge (cannot grade itself)", Asks: []string{"proof-id"}},
		parkAskRow{Codes: []string{"human-review:ui"}, Label: "high: UI, Shared record", Asks: []string{"UI", "Shared record"}},
		parkAskRow{Codes: []string{"human-review:ui"}, Label: "low: Data + export +2", Asks: []string{"Data + export", "Second area", "Third area"}},
		parkAskRow{Codes: []string{"human-review:ui"}, Label: "low: UI +9223372036854775808", Asks: []string{"UI", "Second area"}},
		parkAskRow{Codes: []string{"always-human-path", "human-review:ui"}, Label: "low: Persistent write to shared refe…", Asks: []string{"config:shared-record"}},
		parkAskRow{Codes: []string{"human-review:ui"}, Label: "low: UI, Record, Third +3", Asks: []string{"UI", "Record", "Third", "Fourth"}},
	)
	for i := range rows {
		r := &rows[i]
		var ok bool
		r.Question, r.Items, ok = ReviewAsk(r.Codes, r.Label, r.Asks, r.EvidenceURL != "", r.Head)
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

func TestReviewAskCoveringNames(t *testing.T) {
	for _, tc := range []struct {
		label      string
		asks, want []string
	}{
		{"low: UI, Record, Clear…", []string{"UI", "Record", "Clearing details", "Fourth area"}, []string{"protected path", "UI", "Record", "Clearing details", "Fourth area"}},
		{"low: UI, Record, Clear…", []string{"UI", "Record"}, []string{"protected path", "low: UI, Record, Clear…"}},
		{"low: Persistent write to shared refe…", []string{"config:shared-record"}, []string{"protected path", "low: Persistent write to shared refe…"}},
		{"low: UI, Record +2", []string{"UI", "Record", "Clearing details", "Fourth area"}, []string{"protected path", "UI", "Record", "Clearing details", "Fourth area"}},
		{"low: UI, Record, Third +3", []string{"UI", "Record", "Third", "Fourth"}, []string{"protected path", "low: UI, Record, Third +3"}},
		{"moderate: UI, Record", []string{"UI", "Other"}, []string{"protected path", "moderate: UI, Record"}},
		{"moderate: UI, protected path", []string{"UI", "protected path", "UI"}, []string{"protected path", "UI"}},
		{"high: UI, Record", []string{"UI", "Record"}, []string{"protected path", "UI", "Record"}},
	} {
		_, got, ok := ReviewAsk([]string{"always-human-path", "human-review:ui"}, tc.label, tc.asks, false, "")
		if !ok || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("label %q: items = %q, want %q", tc.label, got, tc.want)
		}
	}
}
