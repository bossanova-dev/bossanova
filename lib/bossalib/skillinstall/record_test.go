package skillinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/safego"
)

// treeFingerprint hashes every entry under dir — path, mode, symlink target and
// bytes — so a hold case can assert the installed tree did not move at all.
func treeFingerprint(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s %v", filepath.ToSlash(rel), info.Mode())
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			line += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:])
		}
		lines = append(lines, line)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func mustReadRecord(t *testing.T, dir string) (PayloadRecord, bool) {
	t.Helper()
	record, recorded, err := ReadPayloadRecord(dir)
	if err != nil {
		t.Fatalf("ReadPayloadRecord: %v", err)
	}
	return record, recorded
}

func assertTreeMatches(t *testing.T, dir string, fsys fs.FS) {
	t.Helper()
	needs, err := NeedsUpdate(dir, fsys)
	if err != nil {
		t.Fatalf("NeedsUpdate: %v", err)
	}
	if needs {
		t.Fatal("installed tree does not match the expected payload")
	}
}

var (
	oldCheckoutRecord = PayloadRecord{Origin: OriginCheckout, Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Writer: WriterExplicit}
	explicitEmbedded  = PayloadRecord{Origin: OriginEmbedded, Revision: "bbbbbbb", Version: "v1.0.0-5-gbbbbbbb", Writer: WriterExplicit}
	unattendedEmbed   = PayloadRecord{Origin: OriginEmbedded, Revision: "ccccccc", Version: "v1.0.0-6-gccccccc", Writer: WriterUnattended}
	payloadEmbedded   = PayloadRecord{Origin: OriginEmbedded, Revision: "ddddddd", Version: "v1.0.0-7-gddddddd", Writer: WriterUnattended}
	pinnedEmbed       = PayloadRecord{Origin: OriginEmbedded, Revision: "fffffff", Version: "v1.0.0-6-gfffffff", Writer: WriterUnattended, Pinned: true}
)

// pinned is want as the guarded pipeline stamps it after refreshing over an
// explicit, checkout or already-pinned record.
func pinned(want PayloadRecord) PayloadRecord {
	want.Pinned = true
	return want
}

func fixedOrder(ordering Ordering, calls *int) OrderFunc {
	return func(PayloadRecord, PayloadRecord) Ordering {
		if calls != nil {
			*calls++
		}
		return ordering
	}
}

// TestNoDowngradeMatrix covers every row of the unattended refresh matrix. Each
// hold asserts the installed tree is byte- and mode-identical afterwards; each
// refresh asserts the tree equals the payload and the record names it, pinned
// exactly when the tree it replaced carried an explicit, checkout or pinned floor.
func TestNoDowngradeMatrix(t *testing.T) {
	cases := []struct {
		name        string
		installed   *PayloadRecord // nil: legacy, unstamped tree
		payload     PayloadRecord
		order       Ordering
		wantRefresh bool
		wantPinned  bool
	}{
		{name: "legacy tree refreshes and is stamped", installed: nil, payload: payloadEmbedded, order: OrderOlder, wantRefresh: true},
		{name: "same revision byte-compares and refreshes", installed: &PayloadRecord{Origin: OriginCheckout, Revision: "ddddddd0123456789abcdef0123456789abcdef", Writer: WriterExplicit}, payload: payloadEmbedded, order: OrderOlder, wantRefresh: true, wantPinned: true},
		{name: "provably newer payload refreshes over a checkout install", installed: &oldCheckoutRecord, payload: payloadEmbedded, order: OrderNotOlder, wantRefresh: true, wantPinned: true},
		{name: "provably newer payload refreshes over an explicit install", installed: &explicitEmbedded, payload: payloadEmbedded, order: OrderNotOlder, wantRefresh: true, wantPinned: true},
		{name: "provably newer payload refreshes over a pinned refresh", installed: &pinnedEmbed, payload: payloadEmbedded, order: OrderNotOlder, wantRefresh: true, wantPinned: true},
		{name: "unprovable order holds over a pinned unattended refresh", installed: &pinnedEmbed, payload: payloadEmbedded, order: OrderUnknown, wantRefresh: false},
		{name: "provably older payload holds over a checkout install", installed: &oldCheckoutRecord, payload: payloadEmbedded, order: OrderOlder, wantRefresh: false},
		{name: "provably older payload holds over an unattended refresh", installed: &unattendedEmbed, payload: payloadEmbedded, order: OrderOlder, wantRefresh: false},
		{name: "unprovable order holds over an explicit install", installed: &explicitEmbedded, payload: payloadEmbedded, order: OrderUnknown, wantRefresh: false},
		{name: "unprovable order holds over a checkout-origin record", installed: &PayloadRecord{Origin: OriginCheckout, Revision: "eeeeeee", Writer: WriterUnattended}, payload: payloadEmbedded, order: OrderUnknown, wantRefresh: false},
		{name: "unprovable order holds when the payload is unstamped", installed: &oldCheckoutRecord, payload: PayloadRecord{Origin: OriginEmbedded, Revision: "unknown", Version: "dev", Writer: WriterUnattended}, order: OrderUnknown, wantRefresh: false},
		{name: "unprovable order refreshes over an unattended embedded refresh", installed: &unattendedEmbed, payload: payloadEmbedded, order: OrderUnknown, wantRefresh: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.installed == nil {
				if err := Extract(dir, testFS()); err != nil {
					t.Fatalf("Extract: %v", err)
				}
			} else if err := ExtractRecorded(dir, testFS(), *tc.installed); err != nil {
				t.Fatalf("ExtractRecorded: %v", err)
			}
			before := treeFingerprint(t, dir)

			result, err := EnsureUpdatedGuarded(dir, changedFS(), tc.payload, fixedOrder(tc.order, nil))
			if err != nil {
				t.Fatalf("EnsureUpdatedGuarded: %v", err)
			}
			if result.Updated != tc.wantRefresh || result.Held == tc.wantRefresh {
				t.Fatalf("result = %+v, want refresh=%t", result, tc.wantRefresh)
			}
			record, recorded := mustReadRecord(t, dir)
			if !tc.wantRefresh {
				if after := treeFingerprint(t, dir); after != before {
					t.Fatalf("held refresh moved the installed tree:\nbefore:\n%s\nafter:\n%s", before, after)
				}
				if result.Decision.Reason == "" {
					t.Fatal("held refresh carries no reason")
				}
				return
			}
			assertTreeMatches(t, dir, changedFS())
			want := tc.payload
			want.Pinned = tc.wantPinned
			if !recorded || record != want {
				t.Fatalf("record = %+v (recorded %t), want %+v", record, recorded, want)
			}
		})
	}
}

// TestNoDowngradeCurrentTreeNeedsNoDecision proves a matching tree is a no-op
// that neither consults the oracle nor rewrites the record.
func TestNoDowngradeCurrentTreeNeedsNoDecision(t *testing.T) {
	dir := t.TempDir()
	if err := ExtractRecorded(dir, testFS(), oldCheckoutRecord); err != nil {
		t.Fatal(err)
	}
	before := treeFingerprint(t, dir)
	calls := 0
	result, err := EnsureUpdatedGuarded(dir, testFS(), payloadEmbedded, fixedOrder(OrderNotOlder, &calls))
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || result.Held || calls != 0 {
		t.Fatalf("result = %+v, oracle calls = %d; want a silent no-op", result, calls)
	}
	if after := treeFingerprint(t, dir); after != before {
		t.Fatal("current tree was rewritten")
	}
}

// TestNoDowngradeCurrentLegacyTreeIsStamped pins R4 for a tree that already
// matches the guarded payload: the first writer claims an unstamped tree, so a
// later older writer finds a record instead of taking the legacy refresh row.
func TestNoDowngradeCurrentLegacyTreeIsStamped(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir, testFS()); err != nil {
		t.Fatal(err)
	}
	calls := 0
	result, err := EnsureUpdatedGuarded(dir, testFS(), payloadEmbedded, fixedOrder(OrderNotOlder, &calls))
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || result.Held || calls != 0 {
		t.Fatalf("result = %+v, oracle calls = %d; want a no-op write", result, calls)
	}
	assertTreeMatches(t, dir, testFS())
	if record, recorded := mustReadRecord(t, dir); !recorded || record != payloadEmbedded {
		t.Fatalf("record = %+v (recorded %t), want the guarded payload stamped", record, recorded)
	}
	older := PayloadRecord{Origin: OriginEmbedded, Revision: "unknown", Version: "dev", Writer: WriterUnattended}
	before := treeFingerprint(t, dir)
	held, err := EnsureUpdatedGuarded(dir, changedFS(), older, fixedOrder(OrderOlder, nil))
	if err != nil || !held.Held {
		t.Fatalf("older writer after the stamp = %+v, %v; want held", held, err)
	}
	if after := treeFingerprint(t, dir); after != before {
		t.Fatal("held refresh moved the installed tree")
	}
}

// TestNoDowngradePinnedFloorSurvivesAnUnprovableWriter chains the three writers
// of the restorer window: an explicit checkout install at N0, a headless
// embedded refresh the checkout proves forward to N1, then a dev-built plugin
// whose only oracle is the release version. The forward refresh must carry the
// checkout floor, so the unprovable writer holds instead of downgrading.
func TestNoDowngradePinnedFloorSurvivesAnUnprovableWriter(t *testing.T) {
	dir := t.TempDir()
	checkout := PayloadRecord{Origin: OriginCheckout, Revision: "0123456789abcdef0123456789abcdef01234567", Writer: WriterExplicit}
	if err := ExtractRecorded(dir, testFS(), checkout); err != nil {
		t.Fatal(err)
	}
	forward := PayloadRecord{Origin: OriginEmbedded, Revision: "89abcde", Version: "v1.0.0-9-g89abcde", Writer: WriterUnattended}
	result, err := EnsureUpdatedGuarded(dir, changedFS(), forward, fixedOrder(OrderNotOlder, nil))
	if err != nil || !result.Updated {
		t.Fatalf("forward refresh = %+v, %v; want refreshed", result, err)
	}
	if record, recorded := mustReadRecord(t, dir); !recorded || record != pinned(forward) {
		t.Fatalf("record after the forward refresh = %+v (recorded %t), want %+v", record, recorded, pinned(forward))
	}
	before := treeFingerprint(t, dir)

	stale := PayloadRecord{Origin: OriginEmbedded, Revision: "fedcba9", Version: "dev", Writer: WriterUnattended}
	held, err := EnsureUpdatedGuarded(dir, testFS(), stale, ReleaseVersionOrder)
	if err != nil {
		t.Fatal(err)
	}
	if !held.Held || held.Updated || held.Decision.Ordering != OrderUnknown {
		t.Fatalf("unprovable writer over a pinned refresh = %+v; want held on unknown order", held)
	}
	if after := treeFingerprint(t, dir); after != before {
		t.Fatalf("held refresh moved the installed tree:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestNoDowngradeNeverFreshInstalls(t *testing.T) {
	dir := t.TempDir()
	result, err := EnsureUpdatedGuarded(dir, testFS(), payloadEmbedded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated || IsInstalled(dir) {
		t.Fatalf("result = %+v; an empty target must stay empty", result)
	}
	if result, err := EnsureUpdatedGuarded(filepath.Join(dir, "missing"), testFS(), payloadEmbedded, nil); err != nil || result.Updated {
		t.Fatalf("missing dir: result = %+v, err = %v", result, err)
	}
}

// TestNoDowngradeConcurrentWritersLeaveOneCoherentPayload races a hold-worthy
// (older) and a refresh-worthy (newer) writer over one checkout-stamped tree.
// Whatever the interleaving, the final tree and its record must describe one
// payload, never a mix.
func TestNoDowngradeConcurrentWritersLeaveOneCoherentPayload(t *testing.T) {
	older := PayloadRecord{Origin: OriginEmbedded, Revision: "1111111", Version: "v1.0.0-1-g1111111", Writer: WriterUnattended}
	newer := PayloadRecord{Origin: OriginEmbedded, Revision: "2222222", Version: "v1.0.0-2-g2222222", Writer: WriterUnattended}
	installed := PayloadRecord{Origin: OriginCheckout, Revision: "1234567", Writer: WriterExplicit}
	order := func(base, payload PayloadRecord) Ordering {
		rank := map[string]int{older.Revision: 1, installed.Revision: 2, newer.Revision: 3}
		switch {
		case rank[payload.Revision] >= rank[base.Revision]:
			return OrderNotOlder
		default:
			return OrderOlder
		}
	}
	olderFS := testFS()
	newerFS := changedFS()
	for round := 0; round < 20; round++ {
		dir := t.TempDir()
		if err := ExtractRecorded(dir, fstest.MapFS{"skills/boss/SKILL.md": {Data: []byte("installed")}}, installed); err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var errs []error
		run := func(fsys fs.FS, payload PayloadRecord) <-chan struct{} {
			return safego.Go(zerolog.Nop(), func() {
				if _, err := EnsureUpdatedGuarded(dir, fsys, payload, order); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			})
		}
		doneOlder := run(olderFS, older)
		doneNewer := run(newerFS, newer)
		<-doneOlder
		<-doneNewer
		if len(errs) > 0 {
			t.Fatalf("round %d: %v", round, errs)
		}
		record, recorded := mustReadRecord(t, dir)
		if !recorded || record != pinned(newer) {
			t.Fatalf("round %d: record = %+v (recorded %t), want the newer payload pinned", round, record, recorded)
		}
		assertTreeMatches(t, dir, newerFS)
	}
}

func TestNoDowngradeRecordIsNotReportedAsDrift(t *testing.T) {
	dir := t.TempDir()
	if err := ExtractRecorded(dir, testFS(), oldCheckoutRecord); err != nil {
		t.Fatal(err)
	}
	report, err := driftReport(dir, testFS())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Entries) != 0 {
		t.Fatalf("drift entries = %+v, want none: the record sits outside the namespace", report.Entries)
	}
}

func TestNoDowngradeUnstampedExtractClearsAStaleRecord(t *testing.T) {
	dir := t.TempDir()
	if err := ExtractRecorded(dir, testFS(), oldCheckoutRecord); err != nil {
		t.Fatal(err)
	}
	if err := Extract(dir, changedFS()); err != nil {
		t.Fatal(err)
	}
	if _, recorded := mustReadRecord(t, dir); recorded {
		t.Fatal("an unstamped extract left a record naming the previous payload")
	}
}

func TestNoDowngradeMalformedRecordIsLegacy(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir, testFS()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, payloadRecordFile), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := EnsureUpdatedGuarded(dir, changedFS(), payloadEmbedded, fixedOrder(OrderOlder, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Decision.Recorded {
		t.Fatalf("result = %+v, want a legacy refresh", result)
	}
	if record, _ := mustReadRecord(t, dir); record != payloadEmbedded {
		t.Fatalf("record = %+v, want the payload restamped", record)
	}
}

func TestNoDowngradeEnsureUpdatedRecordedOverwritesAndStamps(t *testing.T) {
	dir := t.TempDir()
	if err := ExtractRecorded(dir, testFS(), oldCheckoutRecord); err != nil {
		t.Fatal(err)
	}
	explicit := PayloadRecord{Origin: OriginEmbedded, Revision: "0000000", Version: "dev", Writer: WriterExplicit}
	updated, err := EnsureUpdatedRecorded(dir, changedFS(), explicit)
	if err != nil || !updated {
		t.Fatalf("EnsureUpdatedRecorded = %t, %v; want an explicit overwrite", updated, err)
	}
	if record, _ := mustReadRecord(t, dir); record != explicit {
		t.Fatalf("record = %+v, want %+v", record, explicit)
	}
}

func TestNoDowngradeReleaseVersionOrder(t *testing.T) {
	rec := func(version string) PayloadRecord { return PayloadRecord{Version: version} }
	cases := []struct {
		installed, payload string
		want               Ordering
	}{
		{"v1.2.3", "v1.2.4", OrderNotOlder},
		{"v1.2.3", "v1.10.0", OrderNotOlder},
		{"v1.2.3", "v1.2.3", OrderNotOlder},
		{"v1.2.4", "v1.2.3", OrderOlder},
		{"v2.0.0", "v1.99.99", OrderOlder},
		{"v1.2.3", "v1.2.4-dirty", OrderUnknown},
		{"v1.2.3-5-gabc", "v1.2.4", OrderUnknown},
		{"dev", "v1.2.4", OrderUnknown},
		{"", "v1.2.4", OrderUnknown},
	}
	for _, tc := range cases {
		if got := ReleaseVersionOrder(rec(tc.installed), rec(tc.payload)); got != tc.want {
			t.Errorf("ReleaseVersionOrder(%q, %q) = %d, want %d", tc.installed, tc.payload, got, tc.want)
		}
	}
	dirty := PayloadRecord{Version: "v1.2.4", Dirty: true}
	if got := ReleaseVersionOrder(rec("v1.2.3"), dirty); got != OrderUnknown {
		t.Errorf("dirty payload ordered as %d, want unknown", got)
	}
}

func TestNoDowngradeSameRevisionNeedsComparableRecords(t *testing.T) {
	cases := []struct {
		a, b PayloadRecord
		want bool
	}{
		{PayloadRecord{Revision: "abcdef0"}, PayloadRecord{Revision: "abcdef0123456789"}, true},
		{PayloadRecord{Revision: "abcdef0123456789"}, PayloadRecord{Revision: "abcdef0"}, true},
		{PayloadRecord{Revision: "abc"}, PayloadRecord{Revision: "abc"}, false},
		{PayloadRecord{Revision: "unknown"}, PayloadRecord{Revision: "unknown"}, false},
		{PayloadRecord{Revision: "abcdef0", Version: "dev"}, PayloadRecord{Revision: "abcdef0"}, false},
		{PayloadRecord{Revision: "abcdef0", Dirty: true}, PayloadRecord{Revision: "abcdef0"}, false},
		{PayloadRecord{Revision: "abcdef0"}, PayloadRecord{Revision: "1234567"}, false},
	}
	for _, tc := range cases {
		if got := sameRevision(tc.a, tc.b); got != tc.want {
			t.Errorf("sameRevision(%+v, %+v) = %t, want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// TestNoDowngradeExplicitNoOpStillStamps pins the restorer window: a trusted
// reinstall that finds the tree already current must still claim it, or the
// next older unattended writer sees a legacy tree and overwrites it.
func TestNoDowngradeExplicitNoOpStillStamps(t *testing.T) {
	dir := t.TempDir()
	if err := Extract(dir, testFS()); err != nil {
		t.Fatal(err)
	}
	updated, err := EnsureUpdatedRecorded(dir, testFS(), oldCheckoutRecord)
	if err != nil || updated {
		t.Fatalf("EnsureUpdatedRecorded = %t, %v; want a no-op write", updated, err)
	}
	if record, recorded := mustReadRecord(t, dir); !recorded || record != oldCheckoutRecord {
		t.Fatalf("record = %+v (recorded %t), want the explicit stamp", record, recorded)
	}
	result, err := EnsureUpdatedGuarded(dir, changedFS(), payloadEmbedded, fixedOrder(OrderUnknown, nil))
	if err != nil || !result.Held {
		t.Fatalf("guarded refresh after an explicit no-op = %+v, %v; want held", result, err)
	}
}

// TestNoDowngradeEmbeddedPayloadRecordNormalises pins the one rule both the CLI
// and the claude plugin use to describe their embedded payload.
func TestNoDowngradeEmbeddedPayloadRecordNormalises(t *testing.T) {
	cases := []struct {
		name            string
		commit, version string
		writer          PayloadWriter
		want            PayloadRecord
	}{
		{name: "stamped release", commit: "abc1234", version: "v1.2.3", writer: WriterUnattended,
			want: PayloadRecord{Origin: OriginEmbedded, Revision: "abc1234", Version: "v1.2.3", Writer: WriterUnattended}},
		{name: "blank commit is unknown", commit: "", version: "dev", writer: WriterExplicit,
			want: PayloadRecord{Origin: OriginEmbedded, Revision: "unknown", Version: "dev", Writer: WriterExplicit}},
		{name: "whitespace commit is unknown", commit: " \t\n", version: "v1.2.3", writer: WriterUnattended,
			want: PayloadRecord{Origin: OriginEmbedded, Revision: "unknown", Version: "v1.2.3", Writer: WriterUnattended}},
		{name: "dirty version marks the payload dirty", commit: "abc1234", version: "v1.2.3-4-gabc1234-dirty", writer: WriterUnattended,
			want: PayloadRecord{Origin: OriginEmbedded, Revision: "abc1234", Version: "v1.2.3-4-gabc1234-dirty", Dirty: true, Writer: WriterUnattended}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EmbeddedPayloadRecord(tc.commit, tc.version, tc.writer)
			if got != tc.want {
				t.Fatalf("EmbeddedPayloadRecord(%q, %q, %q) = %+v, want %+v", tc.commit, tc.version, tc.writer, got, tc.want)
			}
			if got.Comparable() && (tc.want.Revision == "unknown" || tc.want.Dirty) {
				t.Fatalf("%+v is comparable; an unknown or dirty payload must not be", got)
			}
		})
	}
}
