package jsonlscan

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recordWithPad builds one codex-shaped JSONL envelope whose payload text is
// padBytes long, so a test drives a line of a chosen size through the real
// reader instead of a synthetic splitter.
func recordWithPad(t *testing.T, kind string, padBytes int) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"type": kind,
		"payload": map[string]any{
			"type":    "agent_message",
			"message": strings.Repeat("x", padBytes),
		},
	})
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	return string(b)
}

// writeJSONL joins lines with sep and returns the path. sep is explicit so the
// CRLF case reuses exactly the same fixture bytes as the LF case.
func writeJSONL(t *testing.T, sep string, trailing bool, lines ...string) string {
	t.Helper()
	body := strings.Join(lines, sep)
	if trailing {
		body += sep
	}
	path := filepath.Join(t.TempDir(), "records.jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

// readAll drives the Reader the way every call site does.
func readAll(t *testing.T, path string) (records [][]byte, skipped int, err error) {
	t.Helper()
	f, oerr := os.Open(path)
	if oerr != nil {
		t.Fatalf("open fixture: %v", oerr)
	}
	defer func() { _ = f.Close() }()
	r := New(f)
	for r.Scan() {
		records = append(records, append([]byte(nil), r.Bytes()...))
	}
	return records, r.Skipped(), r.Err()
}

// TestOverLongRecordFailsThePreChangeReader is the non-vacuity proof for every
// skip test below. It runs one fixture through BOTH readers: the pre-change
// `bufio.Scanner` with the exact Buffer(64 KiB, 8 MiB) call the codex plugin
// carried, and this package. The old reader must fail with ErrTooLong and the
// new one must not — otherwise the fixture never produced an over-long record
// and every assertion about skipping proves nothing.
func TestOverLongRecordFailsThePreChangeReader(t *testing.T) {
	big := recordWithPad(t, "response_item", 13*1024*1024)
	if len(big) <= MaxLineBytes {
		t.Fatalf("fixture record is %d bytes, which does not exceed the %d-byte budget: the test would be vacuous", len(big), MaxLineBytes)
	}
	path := writeJSONL(t, "\n", true, recordWithPad(t, "event_msg", 16), big)

	// The pre-change reader, reproduced verbatim.
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	old := bufio.NewScanner(f)
	old.Buffer(make([]byte, ChunkBytes), MaxLineBytes)
	for old.Scan() {
	}
	oldErr := old.Err()
	_ = f.Close()
	if !errors.Is(oldErr, bufio.ErrTooLong) {
		t.Fatalf("the pre-change reader must fail on this fixture or the skip path is untested; got %v", oldErr)
	}

	records, skipped, err := readAll(t, path)
	if err != nil {
		t.Fatalf("the bounded reader must not fail where the scanner did: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(records) != 1 {
		t.Fatalf("len(records) = %d, want 1 (the short record survives)", len(records))
	}
}

func TestCleanFileYieldsEveryRecordAndNoSkips(t *testing.T) {
	want := []string{
		recordWithPad(t, "event_msg", 0),
		recordWithPad(t, "event_msg", 10),
		recordWithPad(t, "response_item", ChunkBytes*3), // spans several read chunks
	}
	path := writeJSONL(t, "\n", true, want...)

	records, skipped, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 — nothing here exceeds the budget", skipped)
	}
	if len(records) != len(want) {
		t.Fatalf("len(records) = %d, want %d", len(records), len(want))
	}
	for i := range want {
		if string(records[i]) != want[i] {
			t.Errorf("record %d differs from the source line (%d bytes vs %d)", i, len(records[i]), len(want[i]))
		}
	}
}

// TestSkippingIsLosslessForTheSurvivingRecords compares the parse of a fixture
// containing an over-long record against the parse of the same fixture with
// that record deleted. Asserting equality against a second fixture is what
// makes "skipping is lossless" a measured property rather than a claim.
func TestSkippingIsLosslessForTheSurvivingRecords(t *testing.T) {
	a := recordWithPad(t, "event_msg", 32)
	b := recordWithPad(t, "event_msg", 64)
	big := recordWithPad(t, "response_item", 13*1024*1024)

	withBig, skipped, err := readAll(t, writeJSONL(t, "\n", true, a, big, b))
	if err != nil {
		t.Fatalf("read with the over-long record: %v", err)
	}
	without, skippedWithout, err := readAll(t, writeJSONL(t, "\n", true, a, b))
	if err != nil {
		t.Fatalf("read without it: %v", err)
	}
	if skipped != 1 || skippedWithout != 0 {
		t.Errorf("skipped = %d / %d, want 1 / 0", skipped, skippedWithout)
	}
	if len(withBig) != len(without) {
		t.Fatalf("len = %d vs %d", len(withBig), len(without))
	}
	for i := range without {
		if !bytes.Equal(withBig[i], without[i]) {
			t.Errorf("record %d is not byte-identical across the two fixtures", i)
		}
	}
}

// TestUnterminatedOverLongFinalRecordTerminates covers the drain loop hitting
// EOF mid-drain: without an EOF exit the reader spins forever on a file whose
// last record is over-long and carries no trailing newline — which is exactly
// the shape of a rollout being written to right now.
func TestUnterminatedOverLongFinalRecordTerminates(t *testing.T) {
	keep := recordWithPad(t, "event_msg", 16)
	path := writeJSONL(t, "\n", false, keep, recordWithPad(t, "response_item", 13*1024*1024))

	records, skipped, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if skipped != 1 {
		t.Errorf("skipped = %d, want 1", skipped)
	}
	if len(records) != 1 || string(records[0]) != keep {
		t.Fatalf("the record before the unterminated one must survive; got %d records", len(records))
	}
}

func TestConsecutiveOverLongRecordsCountSeparately(t *testing.T) {
	keep := recordWithPad(t, "event_msg", 16)
	big := recordWithPad(t, "response_item", 9*1024*1024)
	path := writeJSONL(t, "\n", true, big, big, keep)

	records, skipped, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 — two records were dropped, not one run of them", skipped)
	}
	if len(records) != 1 || string(records[0]) != keep {
		t.Fatalf("the surviving record is wrong; got %d records", len(records))
	}
}

// TestCRLFSplitsIdenticallyToLF: bufio.ScanLines strips a trailing '\r' and a
// hand-rolled splitter that forgets to is invisible under LF-only fixtures,
// which is every fixture this repo had before this package.
func TestCRLFSplitsIdenticallyToLF(t *testing.T) {
	lines := []string{
		recordWithPad(t, "event_msg", 8),
		recordWithPad(t, "response_item", ChunkBytes*2+7), // '\r' can land on a chunk boundary
		recordWithPad(t, "event_msg", 0),
	}
	lf, lfSkipped, err := readAll(t, writeJSONL(t, "\n", true, lines...))
	if err != nil {
		t.Fatalf("LF read: %v", err)
	}
	crlf, crlfSkipped, err := readAll(t, writeJSONL(t, "\r\n", true, lines...))
	if err != nil {
		t.Fatalf("CRLF read: %v", err)
	}
	if lfSkipped != crlfSkipped {
		t.Errorf("skip counts diverge: LF %d, CRLF %d", lfSkipped, crlfSkipped)
	}
	if len(lf) != len(crlf) {
		t.Fatalf("record counts diverge: LF %d, CRLF %d", len(lf), len(crlf))
	}
	for i := range lf {
		if !bytes.Equal(lf[i], crlf[i]) {
			t.Errorf("record %d is not byte-identical across LF and CRLF (%d vs %d bytes)", i, len(lf[i]), len(crlf[i]))
		}
	}
}

// TestPeakAllocationIsBoundedByTheRecordItSkips asserts against the size of
// the record the fixture actually contains, never against MaxLineBytes. An
// assertion measured against the implementation's own constant can only
// confirm the code agrees with itself.
func TestPeakAllocationIsBoundedByTheRecordItSkips(t *testing.T) {
	big := recordWithPad(t, "response_item", 13*1024*1024)
	recordBytes := len(big)
	if recordBytes <= MaxLineBytes {
		t.Fatalf("fixture record is %d bytes and does not exceed the budget: the measurement would prove nothing", recordBytes)
	}
	path := writeJSONL(t, "\n", true, recordWithPad(t, "event_msg", 16), big, recordWithPad(t, "event_msg", 16))

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	r := New(f)
	var kept int
	for r.Scan() {
		kept += len(r.Bytes())
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated >= uint64(recordBytes) {
		t.Errorf("reading a file with a %d-byte record allocated %d bytes: the record was materialised rather than drained",
			recordBytes, allocated)
	}
	if r.Skipped() != 1 {
		t.Errorf("skipped = %d, want 1", r.Skipped())
	}
}

// TestCleanReadNeverAllocatesTheBudget is the replacement for the codex
// plugin's TestRolloutScanBufferIsGrowNotPreallocate. The old invariant —
// "initial buffer < ceiling" — was a statement about two constants that no
// longer exist. The property it was protecting does: a read whose records all
// fit one chunk must not pay the budget.
func TestCleanReadNeverAllocatesTheBudget(t *testing.T) {
	lines := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		lines = append(lines, recordWithPad(t, "event_msg", 128))
	}
	path := writeJSONL(t, "\n", true, lines...)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := New(f)
	for r.Scan() {
	}
	runtime.ReadMemStats(&after)

	if err := r.Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= uint64(MaxLineBytes) {
		t.Errorf("a clean read of %d short records allocated %d bytes, at or above the %d-byte budget: the budget is being paid eagerly",
			len(lines), allocated, MaxLineBytes)
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

// TestNonTooLongErrorPropagatesUnchanged: misclassifying an I/O failure as an
// over-long record would send a reader after the wrong cause, and swallowing
// it would be worse.
func TestNonTooLongErrorPropagatesUnchanged(t *testing.T) {
	sentinel := errors.New("read /rollout.jsonl: input/output error")
	r := New(&failingReader{data: []byte("{\"type\":\"event_msg\"}\n"), err: sentinel})

	if !r.Scan() {
		t.Fatal("the record before the failure must still be delivered")
	}
	if r.Scan() {
		t.Fatal("the reader must stop at the failure")
	}
	if !errors.Is(r.Err(), sentinel) {
		t.Errorf("Err() = %v, want the underlying error untouched", r.Err())
	}
	if r.Skipped() != 0 {
		t.Errorf("an I/O failure must not be counted as a skipped record; got %d", r.Skipped())
	}
}

// TestMalformedLineIsDeliveredNotSkipped proves the skip path has not
// swallowed real corruption: a non-JSON line is under budget, so it is a
// record the reader must hand to the caller — whose own classification then
// sees it. Counting it as a skip would hide corruption behind the same
// mechanism that hides tool output.
func TestMalformedLineIsDeliveredNotSkipped(t *testing.T) {
	const corrupt = "{not json at all"
	path := writeJSONL(t, "\n", true, recordWithPad(t, "event_msg", 8), corrupt)

	records, skipped, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if skipped != 0 {
		t.Errorf("skipped = %d, want 0 — corruption is not an over-long record", skipped)
	}
	if len(records) != 2 {
		t.Fatalf("len(records) = %d, want 2", len(records))
	}
	if string(records[1]) != corrupt {
		t.Errorf("the corrupt line must reach the caller verbatim; got %q", records[1])
	}
	if json.Valid(records[1]) {
		t.Error("the fixture line must actually be invalid JSON or this proves nothing")
	}
}

// TestLineCountsEveryPhysicalLine: a skipped record still occupies a line of
// the file, so a reported line number must keep matching what a human counts.
func TestLineCountsEveryPhysicalLine(t *testing.T) {
	path := writeJSONL(t, "\n", true,
		recordWithPad(t, "event_msg", 8),
		recordWithPad(t, "response_item", 9*1024*1024),
		recordWithPad(t, "event_msg", 8),
	)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	r := New(f)
	var lines []int
	for r.Scan() {
		lines = append(lines, r.Line())
	}
	if fmt.Sprint(lines) != fmt.Sprint([]int{1, 3}) {
		t.Errorf("returned records reported lines %v, want [1 3]", lines)
	}
}

// TestCustomLimitsDriveTheSamePath keeps the cheap-fixture route honest: the
// budget is a parameter, and a test that lowers it exercises the same drain
// loop the 8 MiB default does.
func TestCustomLimitsDriveTheSamePath(t *testing.T) {
	r := NewWithLimits(strings.NewReader("short\n"+strings.Repeat("y", 300)+"\nalso short\n"), 32, 100)
	var got []string
	for r.Scan() {
		got = append(got, string(r.Bytes()))
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if r.Skipped() != 1 {
		t.Errorf("skipped = %d, want 1", r.Skipped())
	}
	if strings.Join(got, "|") != "short|also short" {
		t.Errorf("records = %v, want [short also short]", got)
	}
}

// TestEmptyAndBlankLines pins the boundary behaviour against bufio.Scanner's:
// an empty line is a record, not an end of stream.
func TestEmptyAndBlankLines(t *testing.T) {
	r := New(strings.NewReader("a\n\nb\n"))
	var got []string
	for r.Scan() {
		got = append(got, string(r.Bytes()))
	}
	if err := r.Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Join(got, "|") != "a||b" {
		t.Errorf("records = %v, want [a  b]", got)
	}
}

var _ io.Reader = (*failingReader)(nil)
