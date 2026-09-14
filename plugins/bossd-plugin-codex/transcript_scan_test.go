package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rolloutLineWithText builds one codex rollout event_msg whose agent text is
// padBytes long, so a test can drive a line of a chosen size through the real
// parser rather than a synthetic scanner.
func rolloutLineWithText(t *testing.T, padBytes int) string {
	t.Helper()
	payload := map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":    "agent_message",
			"message": strings.Repeat("x", padBytes),
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal rollout line: %v", err)
	}
	return string(b)
}

func writeRollout(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// TestParseRolloutMessagesReadsLineAboveOldCap is the defect: a transcript line
// larger than the previous 256 KiB cap made the whole read fail with a bare
// `bufio.Scanner: token too long`. A single codex event carrying an inlined
// tool result was observed at roughly 742 KiB, which is the size driven here.
func TestParseRolloutMessagesReadsLineAboveOldCap(t *testing.T) {
	const observedMax = 742 * 1024
	if observedMax <= 256*1024 {
		t.Fatal("the fixture must exceed the OLD cap or it proves nothing")
	}
	if observedMax >= rolloutScanMaxBytes {
		t.Fatal("the fixture must fit under the NEW cap")
	}
	path := writeRollout(t, rolloutLineWithText(t, observedMax))

	msgs, final, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatalf("a %d-byte line must parse under the %d-byte cap: %v", observedMax, rolloutScanMaxBytes, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(msgs))
	}
	if len(final) != observedMax {
		t.Errorf("final assistant text = %d bytes, want %d", len(final), observedMax)
	}
}

// TestParseRolloutMessagesNamesTheOverLongLine covers the other side: a line
// over the NEW cap must report a named reason rather than a bare scanner error,
// because `get transcript: bufio.Scanner: token too long` names neither the
// file, nor the limit, nor the fact that the transcript exists and was refused.
func TestParseRolloutMessagesNamesTheOverLongLine(t *testing.T) {
	path := writeRollout(t,
		rolloutLineWithText(t, 16),
		rolloutLineWithText(t, rolloutScanMaxBytes+1),
	)

	_, _, err := parseRolloutMessages(path)
	if err == nil {
		t.Fatal("a line over the cap must fail rather than silently truncate the transcript")
	}
	if !errors.Is(err, errRolloutLineTooLong) {
		t.Errorf("error is not classified as an over-long line: %v", err)
	}
	for _, want := range []string{path, "line 2", "cannot be parsed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q; got %v", want, err)
		}
	}
}

func TestClassifyRolloutScanErr(t *testing.T) {
	if got := classifyRolloutScanErr(nil, "/p", 0); got != nil {
		t.Errorf("nil must stay nil, got %v", got)
	}
	// Every other scanner error passes through untouched: misclassifying an I/O
	// failure as an over-long line would send a reader after the wrong cause.
	other := errors.New("read /p: input/output error")
	got := classifyRolloutScanErr(other, "/p", 3)
	if !errors.Is(got, other) {
		t.Errorf("an unrelated scanner error must pass through, got %v", got)
	}
	if errors.Is(got, errRolloutLineTooLong) {
		t.Error("an unrelated error was classified as an over-long line")
	}
	tooLong := classifyRolloutScanErr(bufio.ErrTooLong, "/p", 3)
	if !errors.Is(tooLong, errRolloutLineTooLong) {
		t.Errorf("ErrTooLong must classify, got %v", tooLong)
	}
	if !strings.Contains(tooLong.Error(), "line 4") {
		t.Errorf("the reported line is the one AFTER the last completed line, got %v", tooLong)
	}
}

// TestRolloutScanBufferIsGrowNotPreallocate pins the split the fix introduced:
// the old code passed the same value for both, so every scan paid the ceiling
// eagerly and raising the cap would have raised the steady-state cost with it.
func TestRolloutScanBufferIsGrowNotPreallocate(t *testing.T) {
	if rolloutScanInitialBytes >= rolloutScanMaxBytes {
		t.Fatal("the initial buffer must be smaller than the ceiling, or the ceiling is allocated eagerly")
	}
	if rolloutScanMaxBytes <= 742*1024 {
		t.Errorf("the ceiling (%d) must exceed the largest observed rollout line", rolloutScanMaxBytes)
	}
}
