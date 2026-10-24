package attention

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// parkCodeWordsGolden is knownParkCodes' words as data. The verify park note's
// PARK_CODE_WORDS (skills-toolbox/verify/verify-park-note.mjs) and the web's
// PARK_CODE_CATEGORIES (services/web/src/sessionStatus.ts) are compared against
// the same file by their own tests, so the PR comment, the web and the TUI
// cannot describe a park differently. Regenerate it after changing
// knownParkCodes with
// BOSS_UPDATE_PARK_CODE_WORDS=1 go test ./attention -run TestParkCodeWordsGolden
// from lib/bossalib.
const parkCodeWordsGolden = "testdata/park_code_words.json"

type parkCodeWord struct {
	Plain    string `json:"plain"`
	Category string `json:"category"`
	SignOff  bool   `json:"signOff"`
	// Restates is whether Plain only repeats Category, so a line naming the
	// category already says it (ParkCodeRestatesCategory).
	Restates bool `json:"restates"`
}

type parkCodeWordsFile struct {
	Codes   map[string]parkCodeWord `json:"codes"`
	Unknown parkCodeWord            `json:"unknown"`
	// Order is the verify gate's report order (humanReasonsOrder).
	Order []string `json:"order"`
	// Waiting maps each code that is not the operator's to act on, besides
	// any code with the waiting: prefix, to its in-progress label.
	Waiting map[string]string `json:"waiting"`
}

func parkCodeWordsJSON(t *testing.T) []byte {
	t.Helper()
	table := parkCodeWordsFile{
		Codes: map[string]parkCodeWord{},
		Unknown: parkCodeWord{
			Plain: unknownParkCodePlain, Category: unknownParkCodeCategory,
			Restates: ParkCodeRestatesCategory("brand-new-code"),
		},
		Order:   humanReasonsOrder,
		Waiting: waitingLabels,
	}
	for code, info := range knownParkCodes {
		table.Codes[code] = parkCodeWord{
			Plain: info.plain, Category: info.category, SignOff: info.signOff,
			Restates: ParkCodeRestatesCategory(code),
		}
	}
	b, err := json.MarshalIndent(table, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestParkCodeWordsGolden requires the committed golden to be exactly what
// knownParkCodes generates, so the file the park note is checked against is
// never stale.
func TestParkCodeWordsGolden(t *testing.T) {
	got := parkCodeWordsJSON(t)
	if os.Getenv("BOSS_UPDATE_PARK_CODE_WORDS") != "" {
		if err := os.WriteFile(parkCodeWordsGolden, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(parkCodeWordsGolden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate it (see parkCodeWordsGolden). Generated:\n%s", parkCodeWordsGolden, got)
	}
	if again := parkCodeWordsJSON(t); !bytes.Equal(again, got) {
		t.Fatal("generating the park code words twice gave different bytes")
	}
}
