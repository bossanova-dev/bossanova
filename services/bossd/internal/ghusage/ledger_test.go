package ghusage

import (
	"bufio"
	"context"
	"fmt"
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
	l.Record(LabelFinalize, QuotaGraphQL, "")
	l.Record(LabelFinalize, QuotaGraphQL, "")
	l.Record(LabelPRPoller, QuotaCore, "")
	l.Record("", QuotaCore, "")

	got, err := l.Read(now, time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{
		{Label: LabelFinalize, Quota: QuotaGraphQL, Calls: 2},
		{Label: LabelPRPoller, Quota: QuotaCore, Calls: 1},
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
	l.Record("old", QuotaCore, "")
	now = base.Add(-59 * time.Minute) // previous bucket, inside the window
	l.Record("recent", QuotaCore, "")
	now = base.Add(time.Minute) // after the window end
	l.Record("future", QuotaCore, "")

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
	l.Record(LabelRepair, QuotaGraphQL, "")
	now = base // 10:30, current bucket
	l.Record(LabelRepair, QuotaGraphQL, "")

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
	l.Record("before", QuotaCore, "")
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
	l.Record("after", QuotaCore, "")
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
		l.Record("x", QuotaCore, "")
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
				l.Record(LabelCallbacks, QuotaGraphQL, "")
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
	l.Record(LabelPRPoller, QuotaCore, "") // must not panic or block
	var nilLedger *Ledger
	nilLedger.Record(LabelPRPoller, QuotaCore, "")
}

func TestRecordTruncatesLongLabels(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	long := make([]byte, 4*maxLabelBytes)
	for i := range long {
		long[i] = 'a'
	}
	l.Record(string(long), QuotaCore, "")
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

// TestReadCountsCallsAndSkipsPerLabel pins the outcome contract: a record
// with no outcome or "sent" is a call, breaker and floor records are counted
// as skips, and an unknown outcome is counted as neither, so a future outcome
// can never inflate the call count. A label that was only refused still gets
// a row, sorted after every row with calls, and equal call counts sort by
// skips.
func TestReadCountsCallsAndSkipsPerLabel(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	l.Record(LabelPRPoller, QuotaGraphQL, "")
	l.Record(LabelPRPoller, QuotaGraphQL, "")
	l.RecordSkip(LabelPRPoller, QuotaGraphQL, "", OutcomeBreaker)
	l.RecordSkip(LabelPRPoller, QuotaGraphQL, "", OutcomeFloor)
	l.RecordSkip(LabelDisplayPoller, QuotaGraphQL, "", OutcomeFloor)
	l.RecordSkip(LabelDisplayPoller, QuotaGraphQL, "", OutcomeFloor)
	l.RecordSkip(LabelCallbacks, QuotaCore, "", OutcomeBreaker)
	path := filepath.Join(l.dir, bucketName(now))
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	at := now.Unix()
	// A shim-style record with no outcome, an explicit "sent" and an outcome
	// this reader does not know.
	lines := fmt.Sprintf("{\"at\":%d,\"label\":\"cron:x\",\"quota\":\"core\"}\n"+
		"{\"at\":%d,\"label\":\"cron:x\",\"quota\":\"core\",\"outcome\":\"sent\"}\n"+
		"{\"at\":%d,\"label\":\"cron:x\",\"quota\":\"core\",\"outcome\":\"future\"}\n"+
		"{\"at\":%d,\"label\":\"only-unknown\",\"quota\":\"core\",\"outcome\":\"future\"}\n", at, at, at, at)
	if _, err := f.WriteString(lines); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	got, err := l.Read(now, time.Hour)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Count{
		// Equal calls sort by skips: the refused poller before the cron.
		{Label: LabelPRPoller, Quota: QuotaGraphQL, Calls: 2, BreakerSkips: 1, FloorSkips: 1},
		{Label: "cron:x", Quota: QuotaCore, Calls: 2},
		{Label: LabelDisplayPoller, Quota: QuotaGraphQL, FloorSkips: 2},
		{Label: LabelCallbacks, Quota: QuotaCore, BreakerSkips: 1},
	}
	if !reflect.DeepEqual(got.Counts, want) {
		t.Fatalf("Counts = %+v, want %+v", got.Counts, want)
	}
}

// TestSentRecordBytesCarryNoOutcome pins that a sent call's line keeps the
// format the gh shim also writes, and only a skip carries an outcome.
func TestSentRecordBytesCarryNoOutcome(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	l.Record(LabelMerge, QuotaCore, "")
	l.RecordSkip(LabelMerge, QuotaCore, "", OutcomeBreaker)
	raw, err := os.ReadFile(filepath.Join(l.dir, bucketName(now)))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("{\"at\":%d,\"label\":\"daemon:merge\",\"quota\":\"core\",\"identity\":\"user\"}\n"+
		"{\"at\":%d,\"label\":\"daemon:merge\",\"quota\":\"core\",\"identity\":\"user\",\"outcome\":\"breaker\"}\n", now.Unix(), now.Unix())
	if string(raw) != want {
		t.Fatalf("bucket = %q, want %q", raw, want)
	}
}

func TestRecordSkipFailureIsSwallowed(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(filepath.Join(blocker, DirName), zerolog.Nop())
	l.RecordSkip(LabelPRPoller, QuotaCore, "", OutcomeFloor) // must not panic or block
	var nilLedger *Ledger
	nilLedger.RecordSkip(LabelPRPoller, QuotaCore, "", OutcomeBreaker)
}

// bucketRecords returns every record in the bucket holding at.
func bucketRecords(t *testing.T, dir string, at time.Time) []Record {
	t.Helper()
	var recs []Record
	if err := readBucket(filepath.Join(dir, bucketName(at)), func(r Record) { recs = append(recs, r) }); err != nil {
		t.Fatal(err)
	}
	return recs
}

func TestRecordCarriesTheIdentity_GitHubUsage_AC226501(t *testing.T) {
	now := base
	l := newTestLedger(t, &now)
	l.Record(LabelPRPoller, QuotaGraphQL, InstallationIdentity(4242))
	l.Record(LabelMerge, QuotaGraphQL, IdentityUser)
	l.Record(LabelCallbacks, QuotaCore, "")

	got := bucketRecords(t, l.dir, now)
	want := []Record{
		{At: now.Unix(), Label: LabelPRPoller, Quota: QuotaGraphQL, Identity: "installation:4242"},
		{At: now.Unix(), Label: LabelMerge, Quota: QuotaGraphQL, Identity: IdentityUser},
		{At: now.Unix(), Label: LabelCallbacks, Quota: QuotaCore, Identity: IdentityUser},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %+v, want %+v", got, want)
	}
}

func TestRecordWithoutIdentityReadsAsUser(t *testing.T) {
	rec, ok := parseRecord([]byte(`{"at":1,"label":"cron:x","quota":"core"}`))
	if !ok || rec.Identity != IdentityUser {
		t.Fatalf("legacy record = %+v (ok %t), want identity %q", rec, ok, IdentityUser)
	}
}
