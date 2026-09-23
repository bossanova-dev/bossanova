package db

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/recurser/bossalib/jsonlscan"
)

// TestBackfillReadersSurviveAnOverLongRecord covers the third reader in this
// class. Its 10 MiB ceiling was independent of the codex plugin's 8 MiB one
// and breached by the same file, and the failure was silent here: an over-long
// record ended the scan, so transcriptBounds returned an end time taken from
// somewhere in the middle of the transcript rather than its last line, with no
// error anywhere to say so.
func TestBackfillReadersSurviveAnOverLongRecord(t *testing.T) {
	big, err := json.Marshal(map[string]any{
		"timestamp": "2026-09-20T01:00:01Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":   "function_call_output",
			"output": strings.Repeat("y", 13*1024*1024),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(big) <= jsonlscan.MaxLineBytes {
		t.Fatalf("fixture record is %d bytes and does not exceed the %d-byte budget: the test would be vacuous",
			len(big), jsonlscan.MaxLineBytes)
	}

	const (
		firstTS = "2026-09-20T01:00:00Z"
		lastTS  = "2026-09-20T02:00:00Z"
	)
	meta := `{"timestamp":"` + firstTS + `","type":"session_meta","payload":{"id":"sess-9","timestamp":"` + firstTS + `","cwd":"/tmp/work"}}`
	tail := `{"timestamp":"` + lastTS + `","type":"event_msg","payload":{"type":"agent_message","message":"done"}}`

	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	body := strings.Join([]string{meta, string(big), tail}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	got, ok := readCodexMeta(path)
	if !ok {
		t.Fatal("readCodexMeta must still read the session_meta line")
	}
	if got.ID != "sess-9" || got.CWD != "/tmp/work" {
		t.Errorf("meta = %+v, want id sess-9 cwd /tmp/work", got)
	}

	first, last, ok := transcriptBounds(path)
	if !ok {
		t.Fatal("transcriptBounds must resolve both ends")
	}
	if first.Format("2006-01-02T15:04:05Z") != firstTS {
		t.Errorf("first = %v, want %s", first, firstTS)
	}
	// The assertion that matters: the bound reaches the line AFTER the
	// over-long record. A reader that stops at the record would report the
	// over-long record's own timestamp, or the meta line's, and look fine.
	if last.Format("2006-01-02T15:04:05Z") != lastTS {
		t.Errorf("last = %v, want %s — the read must continue past the skipped record", last, lastTS)
	}
}

// TestReadCodexMetaRejectsAPromotedSecondLine pins the one behaviour the
// conversion to jsonlscan could have changed silently. bufio.Reader.ReadBytes
// always returned the FIRST PHYSICAL line; Scan returns the first line that
// FITS THE BUDGET. The meta header is line 1 by contract, so an over-budget
// line 1 has to read as a miss — not as line 2 promoted into the header slot,
// which would hand back a wrong id, cwd and start time that nothing downstream
// could tell from a real one.
func TestReadCodexMetaRejectsAPromotedSecondLine(t *testing.T) {
	big, err := json.Marshal(map[string]any{
		"timestamp": "2026-09-20T01:00:00Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":   "function_call_output",
			"output": strings.Repeat("y", 13*1024*1024),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(big) <= jsonlscan.MaxLineBytes {
		t.Fatalf("fixture record is %d bytes and does not exceed the %d-byte budget: the test would be vacuous",
			len(big), jsonlscan.MaxLineBytes)
	}
	meta := `{"timestamp":"2026-09-20T01:00:01Z","type":"session_meta","payload":{"id":"sess-promoted","timestamp":"2026-09-20T01:00:01Z","cwd":"/tmp/work"}}`

	write := func(t *testing.T, lines ...string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write rollout: %v", err)
		}
		return path
	}

	// The control: the very same meta record on line 1 IS read, so the miss
	// below is the position guard firing and not an unreadable fixture.
	if got, ok := readCodexMeta(write(t, meta, string(big))); !ok || got.ID != "sess-promoted" {
		t.Fatalf("readCodexMeta on a line-1 header = (%+v, %v), want the header: the guard test would prove nothing", got, ok)
	}

	if got, ok := readCodexMeta(write(t, string(big), meta)); ok {
		t.Errorf("readCodexMeta = (%+v, true) with the header on line 2: an over-budget line 1 must read as a miss, not promote a later record", got)
	}
}
