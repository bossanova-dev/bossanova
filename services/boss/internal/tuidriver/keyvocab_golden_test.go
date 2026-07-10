package tuidriver

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// updateGolden regenerates testdata/key-vocab.json from the live namedKeys map
// when -update is passed:
//
//	go test ./internal/tuidriver -run TestKeyVocabGolden -update
var updateGolden = flag.Bool("update", false, "regenerate testdata/key-vocab.json")

// goldenPath is the committed golden the send_keys doc-drift guard reads.
const goldenPath = "testdata/key-vocab.json"

// keyVocab is the machine-readable vocabulary the proof send_keys tool must
// document. `named` is the sorted set of KeyBytes named keys (derived from the
// unexported namedKeys map — the single source of truth); `families` are the
// two parametric key kinds KeyBytes also accepts.
type keyVocab struct {
	Named    []string `json:"named"`
	Families []string `json:"families"`
}

// generateKeyVocab builds the golden bytes deterministically from namedKeys so
// the guard cannot drift from what the Go parser actually accepts.
func generateKeyVocab() ([]byte, error) {
	named := make([]string, 0, len(namedKeys))
	for k := range namedKeys {
		named = append(named, k)
	}
	sort.Strings(named)
	vocab := keyVocab{
		Named: named,
		// Families are the parametric branches of KeyBytes (keybytes.go): the
		// len(name)==1 single-character path and the "ctrl+<a-z>" path. Unlike
		// named (map-derived), they cannot be enumerated from a map, so this
		// list is maintained BY HAND: if KeyBytes ever gains, renames, or drops
		// a parametric branch, update this slice in the same change (and the
		// send_keys doc + its guard's family switch) so the vocabulary cannot
		// silently drift.
		Families: []string{"ctrl+<a-z>", "single-printable-char"},
	}
	b, err := json.MarshalIndent(vocab, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// TestKeyVocabGolden pins testdata/key-vocab.json to the live namedKeys map.
// With -update it regenerates the file; otherwise it asserts the committed file
// is byte-identical to a fresh generation.
func TestKeyVocabGolden(t *testing.T) {
	want, err := generateKeyVocab()
	if err != nil {
		t.Fatalf("generate key vocab: %v", err)
	}

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(goldenPath, want, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		return
	}

	got, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is stale; regenerate with: go test ./internal/tuidriver -run TestKeyVocabGolden -update", goldenPath)
	}
}
