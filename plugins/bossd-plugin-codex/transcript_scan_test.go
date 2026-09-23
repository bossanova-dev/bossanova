package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/jsonlscan"
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

// overBudgetToolOutputLine builds the record shape that actually goes over the
// budget in the wild: a function_call_output envelope carrying an inlined tool
// result. parseRolloutMessages discards every envelope whose type is not
// event_msg, which is why skipping one costs no chat message. The size is
// checked here so no caller can drive the skip path with a fixture that never
// exceeded the budget.
func overBudgetToolOutputLine(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": "response_item",
		"payload": map[string]any{
			"type":   "function_call_output",
			"output": strings.Repeat("y", overBudgetPad),
		},
	})
	if err != nil {
		t.Fatalf("marshal tool output line: %v", err)
	}
	line := string(b)
	requireOverBudget(t, line)
	return line
}

// overBudgetPad is the padding that produces a record larger than any ceiling
// this path has ever carried. 13 MiB is the size measured on the reported
// rollout (12.83 MiB), rounded up — the fixture must genuinely exceed the
// budget or every assertion below is vacuous, which each test asserts.
const overBudgetPad = 13 * 1024 * 1024

func writeRollout(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	return path
}

// requireOverBudget fails loudly when a fixture line does not exceed the parse
// budget. A skip test whose fixture never produced an over-long record proves
// nothing about the skip path, and it passes just as green as a real one.
func requireOverBudget(t *testing.T, line string) {
	t.Helper()
	if len(line) <= jsonlscan.MaxLineBytes {
		t.Fatalf("fixture line is %d bytes and does not exceed the %d-byte budget: the test would be vacuous",
			len(line), jsonlscan.MaxLineBytes)
	}
}

// TestParseRolloutMessagesReadsLineAboveOldCap is the original defect: a
// transcript line larger than the previous 256 KiB cap made the whole read
// fail with a bare `bufio.Scanner: token too long`. A single codex event
// carrying an inlined tool result was observed at roughly 742 KiB, which is the
// size driven here. That record is still PARSEABLE under the replacement — it
// fits the budget — so this test must keep passing unchanged in substance.
func TestParseRolloutMessagesReadsLineAboveOldCap(t *testing.T) {
	const observedMax = 742 * 1024
	if observedMax <= 256*1024 {
		t.Fatal("the fixture must exceed the OLD cap or it proves nothing")
	}
	if observedMax >= jsonlscan.MaxLineBytes {
		t.Fatal("the fixture must fit under the parse budget — it must be parsed, not skipped")
	}
	path := writeRollout(t, rolloutLineWithText(t, observedMax))

	msgs, final, skipped, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatalf("a %d-byte line must parse under the %d-byte budget: %v", observedMax, jsonlscan.MaxLineBytes, err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 — this record fits and must be parsed, not dropped", skipped)
	}
	if len(msgs) != 1 {
		t.Fatalf("len(messages) = %d, want 1", len(msgs))
	}
	if len(final) != observedMax {
		t.Errorf("final assistant text = %d bytes, want %d", len(final), observedMax)
	}
}

// TestParseRolloutMessagesSkipsTheRecordThePreChangeReaderRefused is the
// ticket. It proves non-vacuity by running the same fixture through the
// pre-change reader first — bufio.Scanner with the exact Buffer(64 KiB, 8 MiB)
// this file used to carry. That reader must fail; the replacement must not.
func TestParseRolloutMessagesSkipsTheRecordThePreChangeReaderRefused(t *testing.T) {
	big := overBudgetToolOutputLine(t)
	path := writeRollout(t, rolloutLineWithText(t, 16), big, rolloutLineWithText(t, 32))

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	old := bufio.NewScanner(f)
	old.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for old.Scan() {
	}
	oldErr := old.Err()
	_ = f.Close()
	if !errors.Is(oldErr, bufio.ErrTooLong) {
		t.Fatalf("the pre-change reader must fail on this fixture, or this test does not prove the fix; got %v", oldErr)
	}

	msgs, final, skipped, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatalf("a record over the budget must be skipped, not fail the read: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(messages) = %d, want 2 — every event_msg must survive", len(msgs))
	}
	if len(final) != 32 {
		t.Errorf("final assistant text = %d bytes, want 32", len(final))
	}
}

// TestSkippingAnOverLongRecordChangesNoChatMessage compares the parse of a
// fixture containing an over-long tool-output record against the parse of the
// same fixture with that record deleted outright. Equality against a second
// fixture is what makes "skipping is lossless for this consumer" a measured
// property rather than a claim about a filter.
func TestSkippingAnOverLongRecordChangesNoChatMessage(t *testing.T) {
	a := rolloutLineWithText(t, 16)
	b := rolloutLineWithText(t, 64)
	big := overBudgetToolOutputLine(t)

	withBig, finalWith, skipped, err := parseRolloutMessages(writeRollout(t, a, big, b))
	if err != nil {
		t.Fatalf("read with the over-long record: %v", err)
	}
	without, finalWithout, skippedWithout, err := parseRolloutMessages(writeRollout(t, a, b))
	if err != nil {
		t.Fatalf("read without it: %v", err)
	}
	if skipped != 1 || skippedWithout != 0 {
		t.Errorf("skipped = %d / %d, want 1 / 0", skipped, skippedWithout)
	}
	if finalWith != finalWithout {
		t.Errorf("FinalAssistantText diverges: %d vs %d bytes", len(finalWith), len(finalWithout))
	}
	if len(withBig) != len(without) {
		t.Fatalf("message counts diverge: %d vs %d", len(withBig), len(without))
	}
	for i := range without {
		if withBig[i].Role != without[i].Role || withBig[i].Text != without[i].Text ||
			withBig[i].Timestamp != without[i].Timestamp || withBig[i].Kind != without[i].Kind {
			t.Errorf("message %d differs between the two fixtures", i)
		}
	}
}

// TestParseRolloutMessagesStillSkipsMalformedLines: the skip path must not
// have swallowed real corruption. A non-JSON line is under budget, so it is
// NOT an over-long record — it must not be counted as one, and it keeps the
// per-line tolerance codex transcripts need while they are being appended to.
func TestParseRolloutMessagesStillSkipsMalformedLines(t *testing.T) {
	path := writeRollout(t, rolloutLineWithText(t, 8), "{not json at all", rolloutLineWithText(t, 9))

	msgs, _, skipped, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatalf("a malformed line must not fail the read: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 — corruption is not an over-long record and must not be reported as one", skipped)
	}
	if len(msgs) != 2 {
		t.Errorf("len(messages) = %d, want 2", len(msgs))
	}
}

// TestChatTitleAtPathReadsPastAnOverLongRecord: under the old scanner this
// function survived only because the first over-long record in the reported
// rollout fell at line 398, outside the 200-line window. That is luck, not a
// bound, and it is why all three readers were converted together.
func TestChatTitleAtPathReadsPastAnOverLongRecord(t *testing.T) {
	big := overBudgetToolOutputLine(t)
	userLine, err := json.Marshal(map[string]any{
		"type": "event_msg",
		"payload": map[string]any{
			"type":    "user_message",
			"message": "the title we want",
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := writeRollout(t, big, string(userLine))

	if got := chatTitleAtPath(path); got != "the title we want" {
		t.Errorf("chatTitleAtPath = %q, want %q", got, "the title we want")
	}
}

// titleScanFixture builds a rollout whose head alternates `overPerBurst`
// over-budget records with one in-budget non-title record, `bursts` times, and
// puts the only user_message at the very end. Returned records therefore grow
// far more slowly than physical lines, which is the shape that makes
// maxScanLines alone stop bounding the read.
//
// The limits are tiny so the fixture stays kilobytes rather than gigabytes:
// with the production 8 MiB budget, tripping a 2000-physical-line bound would
// need a multi-gigabyte file, so the bound could not be tested at all.
func titleScanFixture(t *testing.T, bursts, overPerBurst, maxLineBytes int) string {
	t.Helper()
	over := `{"type":"response_item","payload":{"pad":"` + strings.Repeat("y", maxLineBytes*2) + `"}}`
	if len(over) <= maxLineBytes {
		t.Fatalf("fixture record is %d bytes and does not exceed the %d-byte budget: the test would be vacuous",
			len(over), maxLineBytes)
	}
	small := `{"type":"turn_context"}`
	if len(small) > maxLineBytes {
		t.Fatalf("the in-budget filler is %d bytes, over the %d-byte budget: it would be skipped too",
			len(small), maxLineBytes)
	}
	title := `{"type":"event_msg","payload":{"type":"user_message","message":"the tail title"}}`
	if len(title) > maxLineBytes {
		t.Fatalf("the title record is %d bytes, over the %d-byte budget: it could never be returned", len(title), maxLineBytes)
	}
	var lines []string
	for i := 0; i < bursts; i++ {
		for j := 0; j < overPerBurst; j++ {
			lines = append(lines, over)
		}
		lines = append(lines, small)
	}
	return writeRollout(t, append(lines, title)...)
}

// TestChatTitleScanIsBoundedByPhysicalLinesNotJustReturnedRecords pins the
// bound that maxScanLines stopped providing once the reader started skipping:
// Scan drains over-budget records without advancing the returned-record
// counter, so a file whose records are mostly over budget would otherwise be
// read to EOF — the reported subject rollout is 421.5 MiB.
//
// The control half is what keeps it non-vacuous: the same fixture shape under
// the bound still finds its title, so the miss above is the physical cap
// firing and not the fixture being unreadable.
func TestChatTitleScanIsBoundedByPhysicalLinesNotJustReturnedRecords(t *testing.T) {
	const (
		chunkBytes   = 64
		maxLineBytes = 128
		overPerBurst = 20
	)
	perBurst := overPerBurst + 1

	t.Run("past the physical bound the tail title is not reached", func(t *testing.T) {
		bursts := (maxTitleScanPhysicalLines / perBurst) + 10
		if returned := bursts; returned >= maxScanLines {
			t.Fatalf("the fixture returns %d records, at or over maxScanLines=%d: maxScanLines would stop the walk and the physical bound would go untested",
				returned, maxScanLines)
		}
		if physical := bursts * perBurst; physical <= maxTitleScanPhysicalLines {
			t.Fatalf("the fixture is %d physical lines, within the %d-line bound: the test would be vacuous",
				physical, maxTitleScanPhysicalLines)
		}
		path := titleScanFixture(t, bursts, overPerBurst, maxLineBytes)
		if got := chatTitleAtPathWithLimits(path, chunkBytes, maxLineBytes); got != "" {
			t.Errorf("chatTitleAtPathWithLimits = %q, want \"\": the walk must stop at the physical bound rather than read to EOF", got)
		}
	})

	t.Run("within the physical bound the same shape still finds its title", func(t *testing.T) {
		bursts := 5
		if physical := bursts*perBurst + 1; physical > maxTitleScanPhysicalLines {
			t.Fatalf("the control fixture is %d physical lines, over the bound: it cannot act as a control", physical)
		}
		path := titleScanFixture(t, bursts, overPerBurst, maxLineBytes)
		if got := chatTitleAtPathWithLimits(path, chunkBytes, maxLineBytes); got != "the tail title" {
			t.Errorf("chatTitleAtPathWithLimits = %q, want %q: the fixture shape itself must not hide a title", got, "the tail title")
		}
	})
}

// TestSessionIndexThreadNameReadsPastAnOverLongRow converts the third reader.
// It shares the budget by design, and leaving it behind is exactly the
// divergence the constant block warns about: "a divergent cap is precisely
// this defect reappearing at a different call site."
func TestSessionIndexThreadNameReadsPastAnOverLongRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)

	big, err := json.Marshal(map[string]any{"id": "other", "thread_name": strings.Repeat("z", overBudgetPad)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	requireOverBudget(t, string(big))
	row, err := json.Marshal(map[string]any{"id": "sess-1", "thread_name": "named thread"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(big) + "\n" + string(row) + "\n"
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}

	if got := sessionIndexThreadName("sess-1"); got != "named thread" {
		t.Errorf("sessionIndexThreadName = %q, want %q", got, "named thread")
	}
}

// TestRolloutReadersShareOneBudget replaces
// TestRolloutScanBufferIsGrowNotPreallocate. That test asserted
// `rolloutScanInitialBytes < rolloutScanMaxBytes` — a relation between two
// constants that no longer exist, because the ceiling they described was
// removed rather than raised. The property it protected does still exist and
// is stronger now: none of these readers has a length cliff of its own, so a
// record that would have broken one breaks none of them. Asserting that
// behaviourally beats asserting the constants, which is why this is a rewrite
// and not a deletion.
func TestRolloutReadersShareOneBudget(t *testing.T) {
	big := overBudgetToolOutputLine(t)
	userLine, err := json.Marshal(map[string]any{
		"type":    "event_msg",
		"payload": map[string]any{"type": "user_message", "message": "reached past the big record"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	path := writeRollout(t, big, string(userLine), rolloutLineWithText(t, 16))

	if _, _, skipped, err := parseRolloutMessages(path); err != nil || skipped != 1 {
		t.Errorf("parseRolloutMessages: err=%v skipped=%d, want nil/1", err, skipped)
	}
	if got := chatTitleAtPath(path); got != "reached past the big record" {
		t.Errorf("chatTitleAtPath = %q, want the title that follows the over-long record", got)
	}

	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	row, err := json.Marshal(map[string]any{"id": "sess-2", "thread_name": "after the big row"})
	if err != nil {
		t.Fatalf("marshal row: %v", err)
	}
	indexBody := big + "\n" + string(row) + "\n"
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(indexBody), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if got := sessionIndexThreadName("sess-2"); got != "after the big row" {
		t.Errorf("sessionIndexThreadName = %q, want %q", got, "after the big row")
	}
}

// TestReadTranscriptAtReportsTheSkipCount: a silent skip is a quieter version
// of the failure this replaced. The count reaches the plugin's structured log
// — not the CLI, because `boss chat show` renders chat messages and a skipped
// function_call_output was never one.
func TestReadTranscriptAtReportsTheSkipCount(t *testing.T) {
	big := overBudgetToolOutputLine(t)

	root := t.TempDir()
	dir := filepath.Join(root, "2026", "09", "20")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	const uuid = "0199a0d2-1111-2222-3333-444455556666"
	write := func(t *testing.T, lines ...string) {
		t.Helper()
		path := filepath.Join(dir, "rollout-2026-09-20T00-00-00-"+uuid+".jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatalf("write rollout: %v", err)
		}
	}

	t.Run("non-zero count is logged", func(t *testing.T) {
		write(t, rolloutLineWithText(t, 16), big)
		var buf bytes.Buffer
		resp, err := readTranscriptAt(root, "", uuid, 0, zerolog.New(&buf))
		if err != nil {
			t.Fatalf("readTranscriptAt: %v", err)
		}
		if !resp.Exists || len(resp.Messages) != 1 {
			t.Fatalf("exists=%v messages=%d, want true/1", resp.Exists, len(resp.Messages))
		}
		var logged struct {
			Path    string `json:"path"`
			Skipped int    `json:"skipped_records"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logged); err != nil {
			t.Fatalf("a skip must emit one structured line; got %q: %v", buf.String(), err)
		}
		if logged.Skipped != 1 {
			t.Errorf("logged skipped_records = %d, want 1", logged.Skipped)
		}
		if !strings.Contains(logged.Path, uuid) {
			t.Errorf("the log line must name the rollout path; got %q", logged.Path)
		}
	})

	t.Run("zero count emits nothing", func(t *testing.T) {
		write(t, rolloutLineWithText(t, 16))
		var buf bytes.Buffer
		if _, err := readTranscriptAt(root, "", uuid, 0, zerolog.New(&buf)); err != nil {
			t.Fatalf("readTranscriptAt: %v", err)
		}
		if buf.Len() != 0 {
			t.Errorf("a clean read must emit nothing — zero skips is not an event; got %q", buf.String())
		}
	})
}

// TestOverBudgetRecordNeverSurfacesErrTooLong is why the errRolloutLineTooLong
// sentinel and the classifyRolloutScanErr wrapper were deleted rather than kept
// as defence. The branch had no production caller left: every reader on this
// path reads through jsonlscan, where an over-budget record is a counted skip,
// and the only thing keeping the branch covered was a test handing
// bufio.ErrTooLong to the classifier by hand. This asserts the reachability
// claim on the real path instead. Non-vacuity comes from the fixture itself:
// overBudgetToolOutputLine fails the test unless the record genuinely exceeds
// the budget, and the sibling test above proves the pre-change reader returns
// bufio.ErrTooLong on exactly this shape.
func TestOverBudgetRecordNeverSurfacesErrTooLong(t *testing.T) {
	path := writeRollout(t, overBudgetToolOutputLine(t), rolloutLineWithText(t, 16))

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	r := jsonlscan.New(f)
	// Drain every record; Err() afterwards is what this test asserts on.
	for r.Scan() {
	}
	readerErr := r.Err()
	_ = f.Close()
	if readerErr != nil {
		t.Errorf("jsonlscan.Err() = %v, want nil on an over-budget record", readerErr)
	}
	if errors.Is(readerErr, bufio.ErrTooLong) {
		t.Error("jsonlscan returned bufio.ErrTooLong: the deleted classifier branch was reachable after all")
	}

	msgs, _, skipped, err := parseRolloutMessages(path)
	if err != nil {
		t.Fatalf("parseRolloutMessages must not fail on an over-budget record; got %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(msgs) != 1 {
		t.Errorf("len(messages) = %d, want 1", len(msgs))
	}
}

// TestReadSessionMetaRejectsAPromotedSecondLine pins the one behaviour the
// conversion to jsonlscan could have changed silently. bufio.Reader.ReadBytes
// always returned the FIRST PHYSICAL line; Scan returns the first line that
// FITS THE BUDGET. session_meta is line 1 by contract, so an over-budget line 1
// has to read as a miss rather than promote line 2 into the header slot — a
// wrong id and cwd here misroute session resolution and look like a real read.
func TestReadSessionMetaRejectsAPromotedSecondLine(t *testing.T) {
	big := overBudgetToolOutputLine(t)
	meta := `{"timestamp":"2026-09-20T01:00:01Z","type":"session_meta","payload":{"id":"sess-promoted","timestamp":"2026-09-20T01:00:01Z","cwd":"/tmp/work"}}`

	// The control: the very same meta record on line 1 IS read, so the miss
	// below is the position guard firing and not an unreadable fixture.
	if got, ok := readSessionMeta(writeRollout(t, meta, big)); !ok || got.ID != "sess-promoted" {
		t.Fatalf("readSessionMeta on a line-1 header = (%+v, %v), want the header: the guard test would prove nothing", got, ok)
	}

	if got, ok := readSessionMeta(writeRollout(t, big, meta)); ok {
		t.Errorf("readSessionMeta = (%+v, true) with the header on line 2: an over-budget line 1 must read as a miss, not promote a later record", got)
	}
}
