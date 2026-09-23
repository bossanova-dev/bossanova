package agenttelemetry

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/jsonlscan"
)

// overBudgetToolOutput builds the record that actually breaches the budget in
// the wild: a codex function_call_output envelope carrying an inlined tool
// result. 13 MiB is the size measured on the reported rollout, rounded up.
func overBudgetToolOutput(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"timestamp": "2026-08-26T01:00:02Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":   "function_call_output",
			"output": strings.Repeat("y", 13*1024*1024),
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	line := string(b)
	if len(line) <= jsonlscan.MaxLineBytes {
		t.Fatalf("fixture record is %d bytes and does not exceed the %d-byte budget: the test would be vacuous",
			len(line), jsonlscan.MaxLineBytes)
	}
	return line
}

// captureSkipLog swaps the package logger for a buffer and restores it.
func captureSkipLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := skipLog
	skipLog = zerolog.New(&buf)
	t.Cleanup(func() { skipLog = previous })
	return &buf
}

func writeLines(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

var telemetryFixture = []string{
	`{"timestamp":"2026-08-26T01:00:00Z","type":"response_item","payload":{"type":"message","role":"assistant"}}`,
	`{"timestamp":"2026-08-26T01:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"output_tokens":11,"reasoning_output_tokens":5}}}}`,
	`{"timestamp":"2026-08-26T01:00:03Z","type":"response_item","payload":{"type":"function_call"}}`,
	`{"timestamp":"2026-08-26T01:00:04Z","type":"response_item","payload":{"type":"function_call_output"}}`,
}

// TestTalliesAreUnchangedByASkippedRecord is the assertion that proves
// skipping is lossless for THIS consumer: not that the record is filtered
// (that is the chat reader's argument), but that it carries nothing telemetry
// reads. Comparing against the same fixture with the record deleted outright
// measures the property instead of asserting it.
func TestTalliesAreUnchangedByASkippedRecord(t *testing.T) {
	captureSkipLog(t)
	big := overBudgetToolOutput(t)

	withBig := append([]string{}, telemetryFixture[:2]...)
	withBig = append(withBig, big)
	withBig = append(withBig, telemetryFixture[2:]...)

	got, err := TallyCodex(strings.NewReader(strings.Join(withBig, "\n") + "\n"))
	if err != nil {
		t.Fatalf("an over-long record must not fail the tally: %v", err)
	}
	want, err := TallyCodex(strings.NewReader(strings.Join(telemetryFixture, "\n") + "\n"))
	if err != nil {
		t.Fatalf("tally without the record: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tallies diverge:\n with = %+v\n without = %+v", got, want)
	}
	if want.OutputTokenCount == nil || *want.OutputTokenCount != 11 {
		t.Fatalf("the fixture must produce real counts or equality proves nothing; got %v", want.OutputTokenCount)
	}
}

func TestTimestampsAreUnchangedByASkippedRecord(t *testing.T) {
	captureSkipLog(t)
	big := overBudgetToolOutput(t)

	withBig := append([]string{}, telemetryFixture[:2]...)
	withBig = append(withBig, big)
	withBig = append(withBig, telemetryFixture[2:]...)

	firstWith, lastWith, err := timestamps(writeLines(t, withBig...))
	if err != nil {
		t.Fatalf("timestamps with the over-long record: %v", err)
	}
	firstWithout, lastWithout, err := timestamps(writeLines(t, telemetryFixture...))
	if err != nil {
		t.Fatalf("timestamps without it: %v", err)
	}
	if !firstWith.Equal(firstWithout) || !lastWith.Equal(lastWithout) {
		t.Errorf("timestamps diverge: %v..%v vs %v..%v", firstWith, lastWith, firstWithout, lastWithout)
	}
}

// TestOnlyOverLongRecordsYieldsNoErrorAndACountedSkip: the degenerate file.
// Before this change it lost the run's counts to a bare ErrTooLong logged at
// Warn; now it produces empty counts and a reported skip, which is the honest
// answer rather than a failure.
func TestOnlyOverLongRecordsYieldsNoErrorAndACountedSkip(t *testing.T) {
	buf := captureSkipLog(t)
	big := overBudgetToolOutput(t)

	counts, err := TallyCodex(strings.NewReader(big + "\n" + big + "\n"))
	if err != nil {
		t.Fatalf("a file of only over-long records must not error: %v", err)
	}
	if !reflect.DeepEqual(counts, Counts{}) {
		t.Errorf("counts = %+v, want zero", counts)
	}
	var logged struct {
		Source  string `json:"source"`
		Skipped int    `json:"skipped_records"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &logged); err != nil {
		t.Fatalf("a skip must emit one structured line; got %q: %v", buf.String(), err)
	}
	if logged.Skipped != 2 {
		t.Errorf("logged skipped_records = %d, want 2 — consecutive records count separately", logged.Skipped)
	}
	if !strings.Contains(logged.Source, "codex") {
		t.Errorf("the log line must name the stream; got %q", logged.Source)
	}
}

// TestCleanTallyEmitsNoSkipLine: zero skips is not an event.
func TestCleanTallyEmitsNoSkipLine(t *testing.T) {
	buf := captureSkipLog(t)
	if _, err := TallyCodex(strings.NewReader(strings.Join(telemetryFixture, "\n") + "\n")); err != nil {
		t.Fatalf("TallyCodex: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("a clean read must emit nothing; got %q", buf.String())
	}
}

type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// TestFatalReadSurfacesAClassifiedError: RedactedLineError shipped with zero
// callers, which is precisely why the incident log read as a bare
// `bufio.Scanner: token too long` naming neither the stream nor the line. A
// genuine read failure must now arrive named — and still unwrap to its cause,
// so a caller's errors.Is keeps working.
func TestFatalReadSurfacesAClassifiedError(t *testing.T) {
	captureSkipLog(t)
	sentinel := errors.New("input/output error")
	_, err := TallyCodex(&failingReader{data: []byte(telemetryFixture[0] + "\n"), err: sentinel})
	if err == nil {
		t.Fatal("a read failure must surface")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("the classified error must still unwrap to its cause; got %v", err)
	}
	for _, want := range []string{"codex", ":1:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q, not arrive bare; got %v", want, err)
		}
	}
}

// TestRedactedLineErrorReportsTypeNotContent pins what "redacted" means: the
// line's contents never reach the message, only where it was and what kind of
// failure it was.
func TestRedactedLineErrorReportsTypeNotContent(t *testing.T) {
	if got := RedactedLineError("/p", 0, nil); got != "" {
		t.Errorf("a nil error must produce no report; got %q", got)
	}
	got := RedactedLineError("/rollout.jsonl", 7, errors.New("secret transcript content"))
	if strings.Contains(got, "secret transcript content") {
		t.Errorf("the report must not carry the error's own text; got %q", got)
	}
	for _, want := range []string{"/rollout.jsonl", ":7:"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report must name %q; got %q", want, got)
		}
	}
}

// TestClassifiedErrorRedactsTheCauseAndStaysUnwrappable asserts the half
// TestRedactedLineErrorReportsTypeNotContent cannot see. That test exercises
// the helper in isolation; this one pins the error the only production caller
// actually returns. Composing with %w used to render the cause's own text
// after the redacted summary, so the helper's guarantee held for a string
// nobody shipped. Both properties have to hold at once: the cause's text stays
// out of the message AND errors.Is still reaches the cause.
func TestClassifiedErrorRedactsTheCauseAndStaysUnwrappable(t *testing.T) {
	if got := classifyJSONLErr("/p", 0, nil); got != nil {
		t.Errorf("a nil cause must classify to nil; got %v", got)
	}
	cause := errors.New("secret transcript content")
	got := classifyJSONLErr("/rollout.jsonl", 7, cause)
	if got == nil {
		t.Fatal("a non-nil cause must classify to an error")
	}
	if strings.Contains(got.Error(), "secret transcript content") {
		t.Errorf("the classified error must not carry the cause's own text; got %v", got)
	}
	if !errors.Is(got, cause) {
		t.Errorf("the classified error must still unwrap to its cause; got %v", got)
	}
	for _, want := range []string{"/rollout.jsonl", ":7:"} {
		if !strings.Contains(got.Error(), want) {
			t.Errorf("the classified error must name %q; got %v", want, got)
		}
	}
}
