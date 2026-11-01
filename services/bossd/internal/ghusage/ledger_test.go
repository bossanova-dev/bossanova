package ghusage

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// base sits 30 minutes into an hour so a one-hour window spans two buckets.
var base = time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)

func newTestLedger(t *testing.T, clock *time.Time) *Ledger {
	t.Helper()
	return New(filepath.Join(t.TempDir(), DirName), zerolog.Nop(), WithNow(func() time.Time { return *clock }))
}

func TestRecordThenReadRoundTrip(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	l.Record(LabelFinalize, QuotaGraphQL)
	l.Record(LabelFinalize, QuotaGraphQL)
	l.Record(LabelPoller, QuotaCore)
	l.Record("", QuotaCore)

	got, err := l.Read(now, time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{
		{Label: LabelFinalize, Quota: QuotaGraphQL, Calls: 2},
		{Label: LabelPoller, Quota: QuotaCore, Calls: 1},
		{Label: LabelUnattributed, Quota: QuotaCore, Calls: 1},
	}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("Counts = %+v, want %+v", got.Counts, want)
	}
	if !got.WindowStart.Equal(now.Add(-time.Hour)) || !got.WindowEnd.Equal(now) {
		t.Fatalf("window = [%v, %v], want the hour ending at %v", got.WindowStart, got.WindowEnd, now)
	}
}

func TestReadExcludesRecordsOutsideTheWindow(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	now = base.Add(-61 * time.Minute) // previous bucket, before the window
	l.Record("old", QuotaCore)
	now = base.Add(-59 * time.Minute) // previous bucket, inside the window
	l.Record("recent", QuotaCore)
	now = base.Add(time.Minute) // after the window end
	l.Record("future", QuotaCore)

	got, err := l.Read(base, time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{{Label: "recent", Quota: QuotaCore, Calls: 1}}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("Counts = %+v, want %+v", got.Counts, want)
	}
}

func TestReadMergesBothHourBuckets(t *testing.T) {
	now := base.Add(-40 * time.Minute) // 09:50, previous bucket
	l := newTestLedger(t, &now)
	l.Record(LabelRepair, QuotaGraphQL)
	now = base // 10:30, current bucket
	l.Record(LabelRepair, QuotaGraphQL)

	entries, err := os.ReadDir(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("bucket files = %d, want 2 (one per hour)", len(entries))
	}
	got, err := l.Read(base, time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{{Label: LabelRepair, Quota: QuotaGraphQL, Calls: 2}}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("Counts = %+v, want %+v", got.Counts, want)
	}
}

func TestReadSkipsLinesThatDoNotParse(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	l.Record("before", QuotaCore)
	path := filepath.Join(l.dir, bucketName(now))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Garbage, a record with no quota, and a torn final line with no newline
	// written after the next record must all be skipped.
	if _, err := f.WriteString("not json\n{\"at\":1,\"label\":\"x\"}\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	l.Record("after", QuotaCore)
	f, err = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"at":`); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, err := l.Read(now, time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{
		{Label: "after", Quota: QuotaCore, Calls: 1},
		{Label: "before", Quota: QuotaCore, Calls: 1},
	}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("Counts = %+v, want %+v", got.Counts, want)
	}
}

func TestReadWithNoLedgerReturnsNoCounts(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	got, err := l.Read(now, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Counts) != 0 || got.Counts == nil {
		t.Fatalf("Counts = %#v, want an empty non-nil slice", got.Counts)
	}
	if !got.WindowStart.Equal(now.Add(-DefaultWindow)) {
		t.Fatalf("WindowStart = %v, want the default window", got.WindowStart)
	}
}

func TestPruneRemovesOnlyStaleBuckets(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	for _, back := range []time.Duration{0, time.Hour, 2 * time.Hour, 3 * time.Hour, 5 * time.Hour} {
		now = base.Add(-back)
		l.Record("x", QuotaCore)
	}
	foreign := filepath.Join(l.dir, "notes.txt")
	if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := l.Prune(base)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// At 10:30 the cutoff is 08:30: the 08:00 bucket still holds in-window
	// records, the 07:00 and 05:00 buckets ended before it.
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	var names []string
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"2026110108.jsonl", "2026110109.jsonl", "2026110110.jsonl", "notes.txt"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("remaining = %v, want %v", names, want)
	}
	got, err := l.Read(base, MaxWindow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Count{{Label: "x", Quota: QuotaCore, Calls: 3}}; !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("after prune, a MaxWindow read = %+v, want %+v", got.Counts, want)
	}
}

func TestPruneWithoutLedgerDirIsNoop(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	if removed, err := l.Prune(base); err != nil || removed != 0 {
		t.Fatalf("Prune = (%d, %v), want (0, nil)", removed, err)
	}
}

func TestConcurrentRecordsAppendOneLineEach(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	const writers, perWriter = 8, 50
	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perWriter {
				l.Record(LabelCallbacks, QuotaGraphQL)
			}
		}()
	}
	wg.Wait()

	f, err := os.Open(filepath.Join(l.dir, bucketName(now)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lines := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if _, ok := parseRecord(sc.Bytes()); !ok {
			t.Fatalf("line %d does not parse: %q", lines+1, sc.Text())
		}
		lines++
	}
	if lines != writers*perWriter {
		t.Fatalf("parseable lines = %d, want %d", lines, writers*perWriter)
	}
}

func TestRecordFailureIsSwallowed(t *testing.T) {
	// A ledger "directory" that is a regular file cannot be written to.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(filepath.Join(blocker, DirName), zerolog.Nop())
	l.Record(LabelPoller, QuotaCore) // must not panic or block
	var nilLedger *Ledger
	nilLedger.Record(LabelPoller, QuotaCore)
}

func TestRecordTruncatesLongLabels(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	long := make([]byte, 4*maxLabelBytes)
	for i := range long {
		long[i] = 'a'
	}
	l.Record(string(long), QuotaCore)
	got, err := l.Read(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Counts) != 1 || len(got.Counts[0].Label) != maxLabelBytes {
		t.Fatalf("Counts = %+v, want one label truncated to %d bytes", got.Counts, maxLabelBytes)
	}
}

func TestLabelTravelsInContext(t *testing.T) {
	ctx := context.Background()
	if got := LabelFrom(ctx); got != LabelUnattributed {
		t.Fatalf("unlabelled = %q, want %q", got, LabelUnattributed)
	}
	outer := WithLabel(ctx, LabelRepair)
	if got := LabelFrom(outer); got != LabelRepair {
		t.Fatalf("labelled = %q, want %q", got, LabelRepair)
	}
	if got := LabelFrom(WithLabel(outer, LabelFinalize)); got != LabelFinalize {
		t.Fatalf("inner label = %q, want it to override the outer one", got)
	}
	if got := LabelFrom(WithLabel(outer, "")); got != LabelRepair {
		t.Fatalf("empty label = %q, want the outer label kept", got)
	}
	if got := PluginLabel("dependabot"); got != "plugin:dependabot" {
		t.Fatalf("PluginLabel = %q", got)
	}
}
