package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// copyFixture copies a file from src to dst (creating dst's parent dirs).
// Used by tests that need to drop transcript fixtures into a temporary
// ~/.codex/sessions/ shard.
func copyFixture(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(dst), err)
	}
	in, err := os.Open(src)
	if err != nil {
		t.Fatalf("open src %s: %v", src, err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create dst %s: %v", dst, err)
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatalf("copy: %v", err)
	}
}

// shardedRolloutPath returns the canonical codex sessions filename for a
// given UUID anchored at root: root/YYYY/MM/DD/rollout-<iso>-<uuid>.jsonl.
// Date matches the test fixture timestamps for fidelity, not today's date.
func shardedRolloutPath(root, uuid string) string {
	ts := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)
	dir := filepath.Join(root,
		ts.Format("2006"),
		ts.Format("01"),
		ts.Format("02"),
	)
	name := "rollout-" + ts.Format("2006-01-02T15-04-05") + "-" + uuid + ".jsonl"
	return filepath.Join(dir, name)
}

func writeSessionMetaRollout(t *testing.T, root, id, workDir, originator string, modTime time.Time) string {
	t.Helper()
	dir := filepath.Join(root,
		modTime.Format("2006"),
		modTime.Format("01"),
		modTime.Format("02"),
	)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, "rollout-"+modTime.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	line := fmt.Sprintf(
		`{"timestamp":"%s","type":"session_meta","payload":{"id":%q,"cwd":%q,"originator":%q,"cli_version":"test"}}`+"\n",
		modTime.Format(time.RFC3339Nano),
		id,
		workDir,
		originator,
	)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatalf("write rollout %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("chtimes rollout %s: %v", path, err)
	}
	return path
}

// shardRolloutPathAt returns the canonical codex rollout path for `id` inside
// the `root/YYYY/MM/DD` shard named by shardDate. Unlike shardedRolloutPath it
// takes the shard date from the caller, so a test can deliberately place a
// rollout in a shard that disagrees with the rollout's own timestamp.
func shardRolloutPathAt(root string, shardDate time.Time, id string) string {
	d := shardDate.UTC()
	return filepath.Join(root,
		d.Format("2006"),
		d.Format("01"),
		d.Format("02"),
		"rollout-"+d.Format("2006-01-02T15-04-05")+"-"+id+".jsonl",
	)
}

// writeRolloutAt writes a single-line codex-tui session_meta rollout at an
// explicit path with an explicit envelope timestamp and an explicit mtime.
//
// writeSessionMetaRollout deliberately couples all three (shard directory,
// envelope timestamp and mtime all derive from one modTime), which is exactly
// the coupling the BOS-843 prefilter falsification tests must break: the whole
// risk of an mtime/shard-date prefilter is that it rejects a file whose
// authoritative meta.Timestamp is inside the window. Originator is fixed at
// "codex-tui" because every caller here is exercising the prefilter, not the
// originator filter — use writeSessionMetaRollout for the other originators.
func writeRolloutAt(tb testing.TB, path, id, workDir string, metaTime, modTime time.Time) string {
	tb.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	line := fmt.Sprintf(
		`{"timestamp":"%s","type":"session_meta","payload":{"id":%q,"cwd":%q,"originator":"codex-tui","cli_version":"test"}}`+"\n",
		metaTime.UTC().Format(time.RFC3339Nano),
		id,
		workDir,
	)
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		tb.Fatalf("write rollout %s: %v", path, err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		tb.Fatalf("chtimes rollout %s: %v", path, err)
	}
	return path
}

// buildSyntheticRolloutCorpus writes a date-sharded codex sessions tree under
// root: `shardCount` day-shards of `perShard` rollouts each, every one of them
// strictly older than notBefore, plus `inWindow` rollouts inside the window.
//
// The out-of-window rollouts are stamped at 09:00 UTC on their shard day while
// notBefore is expected to be later in the day, so the day-shard immediately
// preceding notBefore is still rejected on mtime rather than by directory
// pruning — that keeps the read-count assertions honest about both filters.
//
// Returns the ids of the in-window rollouts, newest last.
func buildSyntheticRolloutCorpus(tb testing.TB, root, workDir string, notBefore time.Time, shardCount, perShard, inWindow int) []string {
	tb.Helper()
	for shard := 1; shard <= shardCount; shard++ {
		day := notBefore.UTC().AddDate(0, 0, -shard)
		for i := 0; i < perShard; i++ {
			ts := time.Date(day.Year(), day.Month(), day.Day(), 9, 0, i%60, 0, time.UTC)
			id := fmt.Sprintf("old-%d-%d", shard, i)
			writeRolloutAt(tb, shardRolloutPathAt(root, ts, id), id, workDir, ts, ts)
		}
	}
	ids := make([]string, 0, inWindow)
	for i := 0; i < inWindow; i++ {
		ts := notBefore.UTC().Add(time.Duration(i+1) * time.Minute)
		id := fmt.Sprintf("in-window-%d", i)
		writeRolloutAt(tb, shardRolloutPathAt(root, ts, id), id, workDir, ts, ts)
		ids = append(ids, id)
	}
	return ids
}

// countingSessionMetaReader wraps readSessionMeta and records how many
// rollouts the scan actually opened. The counter is per-call state, never a
// package-level var, so parallel tests cannot share it.
func countingSessionMetaReader(reads *int) sessionMetaReader {
	return func(path string) (codexSessionMetaPayload, bool) {
		*reads++
		return readSessionMeta(path)
	}
}

// TestScanInteractiveSessionCandidatesInWindowSkipsReadsOutsideWindow is the
// primary BOS-843 gate: it asserts the EXACT number of readSessionMeta calls
// for a corpus whose in-window subset is a small fraction of the whole. Before
// the prefilter the scan opened and read every rollout under the sessions root
// (32 here, ~10.5k on the reporting host), which is what blew the 2-second
// ResolveInteractiveSessionID budget. Deliberately not -short-guarded.
func TestScanInteractiveSessionCandidatesInWindowSkipsReadsOutsideWindow(t *testing.T) {
	const (
		shardCount = 10
		perShard   = 3
		inWindow   = 2
		total      = shardCount*perShard + inWindow
	)
	notBefore := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(10 * time.Minute)

	t.Run("reads only the in-window rollouts", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		ids := buildSyntheticRolloutCorpus(t, root, workDir, notBefore, shardCount, perShard, inWindow)

		reads := 0
		got := scanInteractiveSessionCandidatesInWindowWith(root, workDir, notBefore, notAfter, countingSessionMetaReader(&reads))

		if len(got) != inWindow {
			t.Fatalf("candidates = %d, want %d", len(got), inWindow)
		}
		if got[0].ID != ids[len(ids)-1] {
			t.Errorf("candidates[0].ID = %q, want %q (newest first)", got[0].ID, ids[len(ids)-1])
		}
		if reads != inWindow {
			t.Errorf("readSessionMeta called %d times over a %d-rollout corpus, want exactly %d "+
				"(the in-window subset); the mtime/shard prefilter is not being applied",
				reads, total, inWindow)
		}
		t.Logf("readSessionMeta calls = %d over a %d-rollout corpus (in-window = %d)", reads, total, inWindow)
	})

	t.Run("zero notBefore disables all pruning", func(t *testing.T) {
		root := t.TempDir()
		workDir := t.TempDir()
		buildSyntheticRolloutCorpus(t, root, workDir, notBefore, shardCount, perShard, inWindow)

		reads := 0
		got := scanInteractiveSessionCandidatesInWindowWith(root, workDir, time.Time{}, time.Time{}, countingSessionMetaReader(&reads))

		if len(got) != total {
			t.Errorf("candidates = %d, want %d (a zero notBefore is 'no lower bound')", len(got), total)
		}
		if reads != total {
			t.Errorf("readSessionMeta called %d times, want %d — a zero notBefore must disable "+
				"both the file and the directory prefilter", reads, total)
		}
	})
}

// TestScanInteractiveSessionCandidatesInWindowPrefilterIsConservativeSuperset
// falsifies the four ways an mtime/shard-date prefilter can silently NARROW
// behaviour. Each case writes a rollout whose authoritative meta.Timestamp is
// inside the window but whose mtime or shard directory disagrees, and requires
// the scan to still return it. A naive implementation — an upper-bound mtime
// prune, a zero-slack lower bound, an upper-bound shard prune, or fail-CLOSED
// handling of an unrecognised directory — fails the corresponding case. The
// shard-date boundaries themselves, which this walk-level test cannot isolate,
// are pinned directly by TestShardDirEndsBefore.
func TestScanInteractiveSessionCandidatesInWindowPrefilterIsConservativeSuperset(t *testing.T) {
	notBefore := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	notAfter := notBefore.Add(2 * time.Minute) // the legacy resolver's real upper bound
	metaTime := notBefore.Add(time.Minute)     // authoritative session time, inside the window

	tests := []struct {
		name  string
		write func(t *testing.T, root, workDir, id string) string
	}{
		{
			// mtime runs AHEAD of meta.Timestamp by up to ~a week on real
			// corpora. Falsifies any mtime.After(notAfter) prune.
			name: "mtime seven days after an in-window meta.Timestamp",
			write: func(t *testing.T, root, workDir, id string) string {
				t.Helper()
				return writeRolloutAt(t, shardRolloutPathAt(root, metaTime, id), id, workDir,
					metaTime, metaTime.Add(7*24*time.Hour))
			},
		},
		{
			// Inside mtimePrefilterSlack. Falsifies a zero-slack lower bound.
			name: "mtime twelve hours below notBefore",
			write: func(t *testing.T, root, workDir, id string) string {
				t.Helper()
				return writeRolloutAt(t, shardRolloutPathAt(root, metaTime, id), id, workDir,
					metaTime, notBefore.Add(-12*time.Hour))
			},
		},
		{
			// Codex dates shards from the LOCAL date; west of UTC that lands a
			// day early. Pins that sign: a shard dated a day BEFORE an
			// in-window session must still be descended into. It does not
			// falsify shardDateSlack — mtimePrefilterSlack alone already puts
			// the derived floorDate a full day below date(notBefore), so this
			// case survives at shardDateSlack = 0. See TestShardDirEndsBefore.
			name: "shard directory dated one day before the UTC session date",
			write: func(t *testing.T, root, workDir, id string) string {
				t.Helper()
				shard := metaTime.AddDate(0, 0, -1)
				return writeRolloutAt(t, shardRolloutPathAt(root, shard, id), id, workDir,
					metaTime, metaTime)
			},
		},
		{
			// East of UTC it lands a day late (22.7% of a real corpus).
			// Falsifies any upper-bound shard prune.
			name: "shard directory dated one day after the UTC session date",
			write: func(t *testing.T, root, workDir, id string) string {
				t.Helper()
				shard := metaTime.AddDate(0, 0, 1)
				return writeRolloutAt(t, shardRolloutPathAt(root, shard, id), id, workDir,
					metaTime, metaTime)
			},
		},
		{
			// Codex's shard layout is undocumented; pruning must fail OPEN.
			name: "rollout under a non-numeric directory",
			write: func(t *testing.T, root, workDir, id string) string {
				t.Helper()
				path := filepath.Join(root, "archive", "rollout-2026-05-08T12-01-00-"+id+".jsonl")
				return writeRolloutAt(t, path, id, workDir, metaTime, metaTime)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			workDir := t.TempDir()
			const id = "needle"
			want := tc.write(t, root, workDir, id)

			got := scanInteractiveSessionCandidatesInWindow(root, workDir, notBefore, notAfter)

			if len(got) != 1 {
				t.Fatalf("candidates = %d, want 1 — the prefilter rejected a rollout whose "+
					"meta.Timestamp is inside the window", len(got))
			}
			if got[0].ID != id {
				t.Errorf("candidates[0].ID = %q, want %q", got[0].ID, id)
			}
			if got[0].Path != want {
				t.Errorf("candidates[0].Path = %q, want %q", got[0].Path, want)
			}
		})
	}
}

// TestShardDirEndsBefore pins shardDirEndsBefore directly, at a granularity the
// walk-level prefilter tests cannot reach. In the walk the floor is always
// derived (fileFloor is mtimePrefilterSlack below notBefore, dirFloor another
// shardDateSlack below that), so its floorDate already sits days early and the
// day-level boundary is never exercised at its edge — which is why zeroing
// shardDateSlack leaves those tests green. Called with an explicit floor, every
// boundary is reachable: same-day equality (must NOT prune), year rollover, and
// each malformed segment that must fail OPEN.
//
// Reachable is not the same as falsified. The same-day, year-rollover and
// month/day comparisons are each pinned by a case that flips when the
// comparison changes. The fail-open guards are not: a plain malformed case
// returns false by arithmetic even with its guard deleted, so each guard —
// the depth cap, the month range, and day > 31 — additionally has a
// guard-falsifying sibling case that returns the wrong answer without it.
func TestShardDirEndsBefore(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "sessions")
	defaultFloor := time.Date(2026, 5, 8, 13, 30, 0, 0, time.UTC)

	tests := []struct {
		name  string
		rel   string    // shard path relative to root, always slash-separated
		floor time.Time // zero means defaultFloor
		want  bool
	}{
		{name: "the root itself is never pruned", rel: ".", want: false},

		{name: "year strictly older than the floor year", rel: "2025", want: true},
		{name: "the floor's own year", rel: "2026", want: false},
		{name: "year after the floor year", rel: "2027", want: false},

		{name: "month before the floor month in the floor year", rel: "2026/04", want: true},
		{name: "the floor's own month", rel: "2026/05", want: false},
		{name: "month after the floor month in the floor year", rel: "2026/06", want: false},

		// Year rollover: the month comparison is only valid within the floor's
		// own year, so both signs are pinned.
		{name: "December of the year before a January floor", rel: "2025/12",
			floor: time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC), want: true},
		{name: "January of the year after a December floor", rel: "2026/01",
			floor: time.Date(2025, 12, 15, 0, 0, 0, 0, time.UTC), want: false},

		{name: "day strictly before the floor date", rel: "2026/05/07", want: true},
		// Same-day equality: the floor's own date still holds rollouts inside
		// the window, so it must survive the prune.
		{name: "the floor's own date", rel: "2026/05/08", want: false},
		{name: "day after the floor date", rel: "2026/05/09", want: false},
		{name: "last day of the previous month against a first-of-month floor", rel: "2026/04/30",
			floor: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), want: true},

		// Everything below is malformed and must fail OPEN (false).
		{name: "month out of range fails open", rel: "2026/13", want: false},
		{name: "day out of range fails open", rel: "2026/05/32", want: false},
		{name: "day zero fails open", rel: "2026/05/00", want: false},
		{name: "three-digit year fails open", rel: "202/05", want: false},
		{name: "non-digit in the year fails open", rel: "20a6/05/08", want: false},
		{name: "signed year fails open", rel: "+2026/05/08", want: false},
		{name: "negative-looking year fails open", rel: "-026/05/08", want: false},
		{name: "whitespace in a segment fails open", rel: "20 6/05", want: false},
		{name: "a level deeper than YYYY/MM/DD fails open", rel: "2026/05/08/extra", want: false},
		// time.Date normalizes February 31 forward to March 3, which is NOT
		// before a March 1 floor — so an out-of-month day fails open rather
		// than being pruned on the month alone.
		{name: "February 31 normalizes forward and fails open", rel: "2026/02/31",
			floor: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), want: false},

		// Guard-falsifying siblings of the three plain cases above. Those
		// return false by arithmetic — drop the guard they are named after and
		// they still pass — so only these pin the guards: each one is pruned
		// (true), wrongly, if its guard is deleted.
		{name: "a deeper level whose truncation would prune fails open", rel: "2026/05/07/extra", want: false},
		{name: "month out of range normalizing past the floor fails open", rel: "2026/13/05",
			floor: time.Date(2027, 1, 10, 0, 0, 0, 0, time.UTC), want: false},
		{name: "day above 31 normalizing past the floor fails open", rel: "2026/05/32",
			floor: time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			floor := tc.floor
			if floor.IsZero() {
				floor = defaultFloor
			}
			dir := filepath.Join(append([]string{root}, strings.Split(tc.rel, "/")...)...)
			if got := shardDirEndsBefore(root, dir, floor); got != tc.want {
				t.Errorf("shardDirEndsBefore(%q, %q, %s) = %v, want %v",
					root, dir, floor.Format(time.RFC3339), got, tc.want)
			}
		})
	}
}

// TestScanInteractiveSessionCandidatesInWindowLargeCorpusBoundsReads exercises
// the large-corpus behaviour a small fixture cannot show: 8,000 rollouts across
// 200 day-shards, of which 3 are in the window. The read count must stay
// proportional to the window, not to the corpus. Guarded by testing.Short() so
// the module-local -short loop stays fast; it runs under the root/CI bazel run.
func TestScanInteractiveSessionCandidatesInWindowLargeCorpusBoundsReads(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an 8,000-rollout synthetic corpus on disk; skipped in -short mode")
	}
	const (
		shardCount = 200
		perShard   = 40
		inWindow   = 3
		total      = shardCount*perShard + inWindow
	)
	root := t.TempDir()
	workDir := t.TempDir()
	notBefore := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	ids := buildSyntheticRolloutCorpus(t, root, workDir, notBefore, shardCount, perShard, inWindow)

	reads := 0
	got := scanInteractiveSessionCandidatesInWindowWith(root, workDir, notBefore, notBefore.Add(10*time.Minute), countingSessionMetaReader(&reads))

	if len(got) != inWindow {
		t.Fatalf("candidates = %d, want %d", len(got), inWindow)
	}
	if got[0].ID != ids[len(ids)-1] {
		t.Errorf("candidates[0].ID = %q, want %q (newest in-window rollout first)", got[0].ID, ids[len(ids)-1])
	}
	if reads != inWindow {
		t.Errorf("readSessionMeta called %d times over a %d-rollout corpus, want %d — the scan's "+
			"cost must be proportional to the window, not the corpus size", reads, total, inWindow)
	}
	t.Logf("readSessionMeta calls = %d over a %d-rollout corpus (in-window = %d)", reads, total, inWindow)
}

// BenchmarkScanInteractiveSessionCandidatesInWindow measures the whole scan
// over a realistically sized corpus (BOS-843: a real host carried 10,529
// rollouts and the pre-prefilter scan opened every one of them, blowing the
// 2-second ResolveInteractiveSessionID budget). The corpus is built once,
// outside the timed loop.
func BenchmarkScanInteractiveSessionCandidatesInWindow(b *testing.B) {
	root := b.TempDir()
	workDir := b.TempDir()
	notBefore := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	const inWindow = 3
	buildSyntheticRolloutCorpus(b, root, workDir, notBefore, 200, 40, inWindow)
	notAfter := notBefore.Add(10 * time.Minute)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got := scanInteractiveSessionCandidatesInWindow(root, workDir, notBefore, notAfter)
		if len(got) != inWindow {
			b.Fatalf("candidates = %d, want %d", len(got), inWindow)
		}
	}
}

// TestTranscriptPathFindsShardedFile verifies findRolloutPath globs the
// YYYY/MM/DD shard tree and returns the rollout file for a given UUID.
func TestTranscriptPathFindsShardedFile(t *testing.T) {
	root := t.TempDir()
	uuid := "abcd-1234"
	dst := shardedRolloutPath(root, uuid)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

	got, err := findRolloutPath(root, uuid)
	if err != nil {
		t.Fatalf("findRolloutPath: %v", err)
	}
	if got != dst {
		t.Errorf("path = %q, want %q", got, dst)
	}
}

// TestTranscriptPathReturnsErrorWhenMissing exercises the "no rollout for
// session" error path — used by transcriptExists to safely return false.
func TestTranscriptPathReturnsErrorWhenMissing(t *testing.T) {
	root := t.TempDir()
	if _, err := findRolloutPath(root, "missing-uuid"); err == nil {
		t.Error("expected error for missing rollout, got nil")
	}
}

// TestTranscriptPathPicksMostRecentOnMultiMatch verifies that when more
// than one rollout file matches a UUID (clock skew, crashed-and-restarted
// runs, or a daemon that re-resumed) we return the most-recently-modified
// match rather than failing. The plan-spec'd behavior — earlier versions
// of this code returned an "ambiguous" error which propagated to
// transcriptExists() collapsing to false, defeating the resume path.
func TestTranscriptPathPicksMostRecentOnMultiMatch(t *testing.T) {
	root := t.TempDir()
	uuid := "duplicate-uuid"

	// Stamp the older rollout into a 2026-05-01 shard.
	older := filepath.Join(root, "2026", "05", "01",
		"rollout-2026-05-01T00-00-00-"+uuid+".jsonl")
	copyFixture(t, "testdata/transcripts/sample.jsonl", older)
	oldTime := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatalf("chtimes older: %v", err)
	}

	// Stamp the newer rollout into a 2026-05-08 shard.
	newer := filepath.Join(root, "2026", "05", "08",
		"rollout-2026-05-08T07-45-47-"+uuid+".jsonl")
	copyFixture(t, "testdata/transcripts/sample.jsonl", newer)
	newTime := time.Date(2026, 5, 8, 7, 45, 47, 0, time.UTC)
	if err := os.Chtimes(newer, newTime, newTime); err != nil {
		t.Fatalf("chtimes newer: %v", err)
	}

	got, err := findRolloutPath(root, uuid)
	if err != nil {
		t.Fatalf("findRolloutPath: %v", err)
	}
	if got != newer {
		t.Errorf("findRolloutPath = %q, want %q (most recently modified)", got, newer)
	}
}

// TestFindRolloutPathRejectsTraversalSessionID verifies the BOS-415
// root-containment guard: an agentSessionID carrying path separators or ".."
// is rejected before it can collapse the glob pattern out of the sessions
// root, even when a matching file is planted outside the shard tree.
func TestFindRolloutPathRejectsTraversalSessionID(t *testing.T) {
	root := t.TempDir()

	// Plant a rollout-shaped file one level above the sessions root that a
	// traversal id would otherwise glob into.
	outside := filepath.Join(filepath.Dir(root), "rollout-2026-05-08T07-45-47-escape.jsonl")
	if err := os.WriteFile(outside, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	for _, id := range []string{"../escape", "a/b", `a\b`, ".."} {
		if _, err := findRolloutPath(root, id); err == nil {
			t.Errorf("findRolloutPath(root, %q) = nil error, want rejection", id)
		}
	}
}

// TestChatTitleExtractsFirstUserMessage verifies the chat-title scan picks
// the first event_msg/user_message text out of a real codex transcript
// (sample.jsonl, which begins with the developer prompt + an
// environment_context user message + the real "say hello and exit").
func TestChatTitleExtractsFirstUserMessage(t *testing.T) {
	got := chatTitleAtPath("testdata/transcripts/sample.jsonl")
	want := "say hello and exit"
	if got != want {
		t.Errorf("chatTitleAtPath = %q, want %q", got, want)
	}
}

// TestLastTurnIsUserHandlesCodexFormat verifies the codex-specific JSONL
// envelope walker: it returns true when the last meaningful entry is an
// event_msg/user_message, and false when the transcript ends with an
// agent_message (or only contains assistant turns).
func TestLastTurnIsUserHandlesCodexFormat(t *testing.T) {
	if !lastTurnIsUser("testdata/transcripts/last_user.jsonl") {
		t.Error("expected lastTurnIsUser=true for last_user.jsonl (ends in user_message)")
	}
	// sample.jsonl ends with task_complete + agent_message — the last
	// meaningful turn is agent.
	if lastTurnIsUser("testdata/transcripts/sample.jsonl") {
		t.Error("expected lastTurnIsUser=false for sample.jsonl (ends in agent_message)")
	}
}

// TestLastTurnIsUserTreatsTaskCompleteAsAgentTurn pins the contract from
// the codex Lane 0 spike: a turn that ends with `task_complete` (the
// envelope codex emits when the agent finishes, regardless of whether it
// also produced an `agent_message`) belongs to the agent. The bug it
// guards against: a transcript shaped `user_message → task_complete` (the
// agent's response was all tool calls / no final text, so no
// `agent_message` was emitted) would walk past the task_complete, hit the
// preceding user_message, and wrongly report user-last — which suppresses
// legitimate question-state detection downstream.
func TestLastTurnIsUserTreatsTaskCompleteAsAgentTurn(t *testing.T) {
	if lastTurnIsUser("testdata/transcripts/user_then_task_complete.jsonl") {
		t.Error("expected lastTurnIsUser=false for user_then_task_complete.jsonl " +
			"(transcript ends with task_complete; agent finished its turn)")
	}
}

// TestTranscriptExistsAcrossStates covers the "happy path", "no file",
// and "empty file" branches of transcriptExists. We point HOME at a temp
// dir so transcriptPath's globbing sees only fixtures we control.
func TestTranscriptExistsAcrossStates(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment

	// 1) Missing → false.
	if transcriptExists("/anywhere", "no-such-uuid") {
		t.Error("transcriptExists should be false when no rollout exists")
	}

	// 2) Empty → false (file present but zero bytes).
	emptyUUID := "empty-uuid"
	emptyDst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), emptyUUID)
	copyFixture(t, "testdata/transcripts/empty.jsonl", emptyDst)
	if transcriptExists("/anywhere", emptyUUID) {
		t.Error("transcriptExists should be false for empty rollout file")
	}

	// 3) Real → true.
	realUUID := "abcd-1234"
	realDst := shardedRolloutPath(filepath.Join(tmpHome, codexSessionsDir), realUUID)
	copyFixture(t, "testdata/transcripts/sample.jsonl", realDst)
	if !transcriptExists("/anywhere", realUUID) {
		t.Error("transcriptExists should be true for non-empty rollout file")
	}
}

func TestResolveInteractiveSessionIDAtFindsMatchingCodexTUIRollout(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	modTime := launchedAfter.Add(500 * time.Millisecond)
	path := writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", modTime)

	id, gotPath, ambiguous, reason := resolveInteractiveSessionIDAt(root, workDir, launchedAfter)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveInteractiveSessionIDAtIgnoresOldExecAndDifferentCWD(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	otherDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)

	writeSessionMetaRollout(t, root, "old-session", workDir, "codex-tui", launchedAfter.Add(-3*time.Second))
	writeSessionMetaRollout(t, root, "exec-session", workDir, "codex_exec", launchedAfter.Add(time.Second))
	writeSessionMetaRollout(t, root, "other-cwd", otherDir, "codex-tui", launchedAfter.Add(2*time.Second))

	id, path, ambiguous, reason := resolveInteractiveSessionIDAt(root, workDir, launchedAfter)

	if id != "" || path != "" {
		t.Errorf("got id/path = %q/%q, want empty", id, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "no matching codex-tui rollout found" {
		t.Errorf("reason = %q, want no matching reason", reason)
	}
}

func TestResolveInteractiveSessionIDAtReturnsAmbiguousForDifferentIDsInWindow(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)

	writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", launchedAfter.Add(time.Second))
	writeSessionMetaRollout(t, root, "session-2", workDir, "codex-tui", launchedAfter.Add(2*time.Second))

	id, path, ambiguous, reason := resolveInteractiveSessionIDAt(root, workDir, launchedAfter)

	if id != "" || path != "" {
		t.Errorf("got id/path = %q/%q, want empty on ambiguity", id, path)
	}
	if !ambiguous {
		t.Error("ambiguous = false, want true")
	}
	if reason == "" {
		t.Error("reason empty, want ambiguity reason")
	}
}

func TestResolveInteractiveSessionIDAtAcceptsSymlinkEquivalentCWD(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("mkdir real workdir: %v", err)
	}
	linkDir := filepath.Join(t.TempDir(), "linked-work")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatalf("symlink workdir: %v", err)
	}
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	path := writeSessionMetaRollout(t, root, "session-1", realDir, "codex-tui", launchedAfter.Add(time.Second))

	id, gotPath, ambiguous, reason := resolveInteractiveSessionIDAt(root, linkDir, launchedAfter)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveLegacyInteractiveSessionIDAtFindsSingleRolloutInCreatedWindow(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	now := createdAt.Add(10 * time.Minute)

	writeSessionMetaRollout(t, root, "too-old", workDir, "codex-tui", createdAt.Add(-6*time.Minute))
	writeSessionMetaRollout(t, root, "future", workDir, "codex-tui", now.Add(time.Second))
	writeSessionMetaRollout(t, root, "next-chat", workDir, "codex-tui", createdAt.Add(3*time.Minute))
	writeSessionMetaRollout(t, root, "exec-session", workDir, "codex_exec", createdAt.Add(time.Minute))
	path := writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", createdAt.Add(time.Minute))

	id, gotPath, ambiguous, reason := resolveLegacyInteractiveSessionIDAt(root, workDir, createdAt, now)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveLegacyInteractiveSessionIDAtUsesSessionMetaTimestamp(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	now := createdAt.Add(10 * time.Minute)

	path := writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", createdAt.Add(time.Minute))
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatalf("chtimes rollout %s: %v", path, err)
	}

	id, gotPath, ambiguous, reason := resolveLegacyInteractiveSessionIDAt(root, workDir, createdAt, now)

	if id != "session-1" {
		t.Errorf("id = %q, want session-1", id)
	}
	if gotPath != path {
		t.Errorf("path = %q, want %q", gotPath, path)
	}
	if ambiguous {
		t.Error("ambiguous = true, want false")
	}
	if reason != "" {
		t.Errorf("reason = %q, want empty", reason)
	}
}

func TestResolveLegacyInteractiveSessionIDAtReturnsAmbiguousForMultipleRollouts(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	createdAt := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	now := createdAt.Add(10 * time.Minute)

	writeSessionMetaRollout(t, root, "session-1", workDir, "codex-tui", createdAt.Add(time.Minute))
	writeSessionMetaRollout(t, root, "session-2", workDir, "codex-tui", createdAt.Add(2*time.Minute))

	id, path, ambiguous, reason := resolveLegacyInteractiveSessionIDAt(root, workDir, createdAt, now)

	if id != "" || path != "" {
		t.Errorf("got id/path = %q/%q, want empty on ambiguity", id, path)
	}
	if !ambiguous {
		t.Error("ambiguous = false, want true")
	}
	if reason == "" {
		t.Error("reason empty, want ambiguity reason")
	}
}

// TestReadTranscriptAt covers the three key branches of readTranscriptAt:
// happy-path multi-turn parse, MaxMessages tail-cut, and missing-file
// (Exists=false, nil error).
func TestReadTranscriptAt(t *testing.T) {
	t.Run("parses ordered messages from rollout", func(t *testing.T) {
		root := t.TempDir()
		uuid := "read-transcript-uuid"
		dst := shardedRolloutPath(root, uuid)
		copyFixture(t, "testdata/transcripts/chat.jsonl", dst)

		resp, err := readTranscriptAt(root, "", uuid, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.Exists {
			t.Error("expected Exists=true")
		}
		// chat.jsonl has 4 chat messages: user, assistant, user, assistant.
		// token_count and task_complete envelopes must be skipped.
		if len(resp.Messages) != 4 {
			t.Errorf("len(Messages) = %d, want 4", len(resp.Messages))
		}
		wantRoles := []string{"user", "assistant", "user", "assistant"}
		wantTexts := []string{"hello codex", "hi there, how can I help?", "what is 2+2?", "4"}
		for i, msg := range resp.Messages {
			if msg.Role != wantRoles[i] {
				t.Errorf("Messages[%d].Role = %q, want %q", i, msg.Role, wantRoles[i])
			}
			if msg.Text != wantTexts[i] {
				t.Errorf("Messages[%d].Text = %q, want %q", i, msg.Text, wantTexts[i])
			}
			if msg.Kind != "text" {
				t.Errorf("Messages[%d].Kind = %q, want %q", i, msg.Kind, "text")
			}
			if msg.Timestamp == "" {
				t.Errorf("Messages[%d].Timestamp is empty", i)
			}
		}
		if resp.FinalAssistantText != "4" {
			t.Errorf("FinalAssistantText = %q, want %q", resp.FinalAssistantText, "4")
		}
	})

	t.Run("MaxMessages limits to most recent N", func(t *testing.T) {
		root := t.TempDir()
		uuid := "read-transcript-max-uuid"
		dst := shardedRolloutPath(root, uuid)
		copyFixture(t, "testdata/transcripts/chat.jsonl", dst)

		resp, err := readTranscriptAt(root, "", uuid, 2)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !resp.Exists {
			t.Error("expected Exists=true")
		}
		if len(resp.Messages) != 2 {
			t.Errorf("len(Messages) = %d, want 2 (tail-cut to MaxMessages=2)", len(resp.Messages))
		}
		// The tail-2 should be: user "what is 2+2?" + assistant "4".
		if len(resp.Messages) >= 2 {
			if resp.Messages[0].Text != "what is 2+2?" {
				t.Errorf("Messages[0].Text = %q, want %q", resp.Messages[0].Text, "what is 2+2?")
			}
			if resp.Messages[1].Text != "4" {
				t.Errorf("Messages[1].Text = %q, want %q", resp.Messages[1].Text, "4")
			}
		}
		// FinalAssistantText must reflect the true last assistant turn (not just the tail).
		if resp.FinalAssistantText != "4" {
			t.Errorf("FinalAssistantText = %q, want %q", resp.FinalAssistantText, "4")
		}
	})

	t.Run("no rollout file returns Exists=false nil error", func(t *testing.T) {
		root := t.TempDir()
		resp, err := readTranscriptAt(root, "", "no-such-uuid", 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resp.Exists {
			t.Error("expected Exists=false for missing rollout")
		}
		if resp.Messages != nil {
			t.Error("expected nil Messages for missing rollout")
		}
		if resp.FinalAssistantText != "" {
			t.Errorf("expected empty FinalAssistantText for missing rollout, got %q", resp.FinalAssistantText)
		}
	})
}

// TestCodexSessionsRootHonorsCodexHome is the core CODEX_HOME regression: the
// PUBLIC transcriptPath wrapper (not just the *At seam the rest of this file
// drives) must resolve rollouts under CODEX_HOME/sessions when CODEX_HOME is
// set, so a per-account codex home actually gets its own transcripts read.
func TestCodexSessionsRootHonorsCodexHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("CODEX_HOME", tmp)

	uuid := "codex-home-uuid"
	dst := shardedRolloutPath(filepath.Join(tmp, "sessions"), uuid)
	copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

	got, err := transcriptPath("", uuid)
	if err != nil {
		t.Fatalf("transcriptPath: %v", err)
	}
	if got != dst {
		t.Errorf("transcriptPath = %q, want %q", got, dst)
	}
}

// TestCodexSessionsRootUnsetIsByteIdenticalToToday pins the "CODEX_HOME
// unset" behavior of codexSessionsRoot: it must equal ~/.codex/sessions,
// exactly matching the pre-fix hardcoded resolution (home + codexSessionsDir).
func TestCodexSessionsRootUnsetIsByteIdenticalToToday(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")

	got, err := codexSessionsRoot()
	if err != nil {
		t.Fatalf("codexSessionsRoot: %v", err)
	}
	want := filepath.Join(tmpHome, ".codex", "sessions")
	if got != want {
		t.Errorf("codexSessionsRoot = %q, want %q", got, want)
	}
}

// TestTranscriptPathSharedSessionsResumeReachability locks the BOS-158
// cross-account-resume constraint at the daemon boundary: a per-account
// CODEX_HOME is only resume-reachable when its sessions/ directory is
// seeded (symlinked/shared) from a base home that actually holds the
// rollout. Seeding sessions/ is BOS-162's credmaterialize executor's job,
// NOT this RPC's/helper's — this test only asserts that transcriptPath
// resolves correctly THROUGH a properly-seeded symlink, and fails fast
// (not-found) when a per-account home's sessions/ is empty/unseeded. See
// docs/solutions/account-rotation/spike-cross-account-resume-credential-isolation.md.
func TestTranscriptPathSharedSessionsResumeReachability(t *testing.T) {
	uuid := "shared-sessions-uuid"

	t.Run("properly-seeded per-account home resolves through the symlink", func(t *testing.T) {
		base := t.TempDir()
		dst := shardedRolloutPath(filepath.Join(base, "sessions"), uuid)
		copyFixture(t, "testdata/transcripts/sample.jsonl", dst)

		acct := t.TempDir()
		if err := os.Symlink(filepath.Join(base, "sessions"), filepath.Join(acct, "sessions")); err != nil {
			t.Fatalf("symlink sessions/: %v", err)
		}
		t.Setenv("CODEX_HOME", acct)

		got, err := transcriptPath("", uuid)
		if err != nil {
			t.Fatalf("transcriptPath through symlinked sessions/: %v", err)
		}
		wantSuffix := filepath.Join("sessions", "2026", "05", "08")
		if !strings.Contains(got, wantSuffix) {
			t.Errorf("transcriptPath = %q, want it to resolve through the shared sessions/ shard", got)
		}
	})

	t.Run("unseeded per-account home fails fast instead of silently resuming nothing", func(t *testing.T) {
		acct2 := t.TempDir()
		sessionsDir := filepath.Join(acct2, "sessions")
		if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
			t.Fatalf("mkdir empty sessions/: %v", err)
		}
		t.Setenv("CODEX_HOME", acct2)

		if _, err := transcriptPath("", uuid); err == nil {
			t.Fatal("expected an error for an unseeded per-account sessions/ dir, got nil")
		}
	})
}
