package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func fixturePath(name string) string {
	return filepath.Join("testdata", "progress", name)
}

// TestReadProgressTailPhaseMapping pins the transcript-tail → phase mapping the
// daemon thresholds on. The trailing-tool_result row is the regression case:
// it reproduces the shape observed on 2026-08-03 (a tool_result followed by
// nothing at all while the spinner kept animating) and must report
// AWAITING_MODEL carrying the tool_result's own timestamp.
func TestReadProgressTailPhaseMapping(t *testing.T) {
	tests := []struct {
		name      string
		fixture   string
		wantPhase bossanovav1.AgentProgressPhase
		wantKnown bool
		wantTS    string
	}{
		{
			name:      "trailing tool_result means the agent owes an assistant message",
			fixture:   "trailing_tool_result.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL,
			wantKnown: true,
			wantTS:    "2026-08-03T15:12:38.921Z",
		},
		{
			name:      "trailing tool_use means a tool is still executing",
			fixture:   "trailing_tool_use.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL,
			wantKnown: true,
			wantTS:    "2026-08-03T15:11:09.507Z",
		},
		{
			name:      "trailing assistant text means the turn is complete",
			fixture:   "trailing_assistant_text.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_IDLE,
			wantKnown: true,
			wantTS:    "2026-08-03T15:11:41.002Z",
		},
		{
			name:      "truncated final line falls back to the last complete record",
			fixture:   "truncated_final_line.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL,
			wantKnown: true,
			wantTS:    "2026-08-03T15:12:38.921Z",
		},
		{
			// The record type here is deliberately one no Claude version emits:
			// an UNRECOGNISED shape must stop the walk, unlike the known
			// bookkeeping types exercised below.
			name:      "unclassifiable tail record is not a confident phase",
			fixture:   "trailing_unclassified.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN,
			wantKnown: false,
		},
		{
			// The recall regression. Real transcripts almost never end on a
			// semantic record — `…assistant, system, last-prompt` is the most
			// common tail by a wide margin — so a walk that stopped at the
			// first bookkeeping line would answer known=false for most live
			// chats and the detector would never fire in production. The phase
			// must still come from the tool_result behind the bookkeeping.
			name:      "bookkeeping tail is walked past to the last semantic record",
			fixture:   "trailing_bookkeeping.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL,
			wantKnown: true,
			// The trailing `system` write is newer than the tool_result and is
			// itself evidence the process is alive, so it carries the liveness
			// timestamp.
			wantTS: "2026-08-03T15:12:39.100Z",
		},
		{
			// last-prompt / mode / permission-mode carry no timestamp at all in
			// real transcripts, so the semantic record's own timestamp has to
			// survive them rather than being lost.
			name:      "untimestamped bookkeeping keeps the semantic timestamp",
			fixture:   "untimestamped_bookkeeping.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL,
			wantKnown: true,
			wantTS:    "2026-08-03T15:11:09.507Z",
		},
		{
			// The false-positive guard. A long auto-compaction emits `system`
			// records for minutes while the last semantic record ages past the
			// AWAITING_MODEL threshold. Reporting the stale semantic timestamp
			// would flag a healthy chat as dead, so the newest write wins.
			name:      "bookkeeping still landing keeps the chat off the stall threshold",
			fixture:   "bookkeeping_still_writing.jsonl",
			wantPhase: bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL,
			wantKnown: true,
			wantTS:    "2026-08-03T15:40:00.000Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readProgressTail(fixturePath(tt.fixture))
			if err != nil {
				t.Fatalf("readProgressTail returned an error for a readable fixture: %v", err)
			}
			if got.known != tt.wantKnown {
				t.Fatalf("known = %v, want %v", got.known, tt.wantKnown)
			}
			if got.phase != tt.wantPhase {
				t.Errorf("phase = %v, want %v", got.phase, tt.wantPhase)
			}
			if !tt.wantKnown {
				if !got.lastProgressAt.IsZero() {
					t.Errorf("lastProgressAt = %v, want zero when known=false", got.lastProgressAt)
				}
				return
			}
			want, err := time.Parse(time.RFC3339Nano, tt.wantTS)
			if err != nil {
				t.Fatalf("bad want timestamp: %v", err)
			}
			if !got.lastProgressAt.Equal(want) {
				t.Errorf("lastProgressAt = %v, want %v", got.lastProgressAt, want)
			}
		})
	}
}

// TestReadProgressTailFailsOpen asserts the open direction by name: every
// unreadable shape must yield known=false so the daemon raises NOTHING. A
// false "your session is dead" banner is worse than a missed detection.
func TestReadProgressTailFailsOpen(t *testing.T) {
	dir := t.TempDir()

	empty := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	blankLines := filepath.Join(dir, "blank.jsonl")
	if err := os.WriteFile(blankLines, []byte("\n\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	noTimestamp := filepath.Join(dir, "no_ts.jsonl")
	if err := os.WriteFile(noTimestamp, []byte(`{"type":"assistant","message":{"role":"assistant","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	onlyTruncated := filepath.Join(dir, "only_truncated.jsonl")
	if err := os.WriteFile(onlyTruncated, []byte(`{"type":"assis`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Bookkeeping records are walked past, but a window containing NOTHING
	// else must not be answered from — skipping them may never turn into
	// inventing a phase.
	onlyBookkeeping := filepath.Join(dir, "only_bookkeeping.jsonl")
	if err := os.WriteFile(onlyBookkeeping, []byte(
		`{"type":"system","timestamp":"2026-08-03T15:12:39.100Z","content":"hook ok"}`+"\n"+
			`{"type":"last-prompt","prompt":"go"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// wantErr separates the two kinds of fail-open answer. A read failure says
	// nothing about the file's CONTENT, so the caller must not memoize it; an
	// unclassifiable-but-read file is a property of the bytes and is cacheable.
	tests := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "absent transcript", path: filepath.Join(dir, "does-not-exist.jsonl"), wantErr: true},
		{name: "empty transcript", path: empty, wantErr: true},
		{name: "blank lines only", path: blankLines},
		{name: "record without a parsable timestamp", path: noTimestamp},
		{name: "every line truncated", path: onlyTruncated},
		{name: "path is a directory", path: dir, wantErr: true},
		{name: "bookkeeping records only", path: onlyBookkeeping},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readProgressTail(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if got.known {
				t.Fatalf("known = true, want false (fail open) for %s", tt.name)
			}
			if got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN {
				t.Errorf("phase = %v, want UNKNOWN", got.phase)
			}
			if !got.lastProgressAt.IsZero() {
				t.Errorf("lastProgressAt = %v, want zero (never fabricate one)", got.lastProgressAt)
			}
		})
	}
}

// TestReadProgressTailUnterminatedFinalRecord covers the mid-write read where
// the partial final record carries no trailing newline at all.
func TestReadProgressTailUnterminatedFinalRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial.jsonl")
	content := `{"type":"user","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}` + "\n" +
		`{"type":"assistant","timestamp":"2026-08-03T15:12:40.0`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readProgressTail(path)
	if err != nil {
		t.Fatalf("readProgressTail: %v", err)
	}
	if !got.known {
		t.Fatal("known = false, want true (last complete record is usable)")
	}
	if got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
		t.Errorf("phase = %v, want AWAITING_MODEL", got.phase)
	}
}

// TestReadProgressTailReadsOnlyTheTail proves the probe never depends on the
// head of the file: a transcript whose leading records exceed the tail window
// is still classified from its final record.
func TestReadProgressTailReadsOnlyTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	filler := `{"type":"assistant","timestamp":"2026-08-03T14:00:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"` +
		"padpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpadpad" + `"}]}}` + "\n"
	for written := 0; written < progressTailSize*2; written += len(filler) {
		if _, err := f.WriteString(filler); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.WriteString(`{"type":"user","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := readProgressTail(path)
	if err != nil {
		t.Fatalf("readProgressTail: %v", err)
	}
	if !got.known {
		t.Fatal("known = false, want true")
	}
	if got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
		t.Errorf("phase = %v, want AWAITING_MODEL", got.phase)
	}
}

// TestProgressProbeMemoizesUnchangedTranscript proves the stat-first memo: the
// probe is called on the daemon's 3 s tick for every chat, so an unchanged
// (size, mtime) must NOT re-read the file. Rewriting the content in place
// while restoring size and mtime makes a re-read observable — a cache miss
// would report the new phase.
func TestProgressProbeMemoizesUnchangedTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memo.jsonl")
	toolResult := `{"type":"user","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"aa"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(toolResult), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	var probe progressProbe
	if got := probe.snapshot(path); got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
		t.Fatalf("first probe phase = %v, want AWAITING_MODEL", got.phase)
	}

	// Same byte length, different trailing record — then restore the mtime.
	// The id is padded purely so this record is byte-for-byte the same length as
	// toolResult above; the guard below fails loudly if that ever drifts.
	toolUse := `{"type":"assistant","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1abc","name":"Bash"}]}}` + "\n"
	if len(toolUse) != len(toolResult) {
		t.Fatalf("fixture lengths differ (%d vs %d); the memo test needs an equal-size rewrite", len(toolUse), len(toolResult))
	}
	if err := os.WriteFile(path, []byte(toolUse), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	if got := probe.snapshot(path); got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
		t.Errorf("phase after in-place rewrite = %v, want the memoized AWAITING_MODEL (the probe re-read an unchanged file)", got.phase)
	}

	// Growing the file must invalidate the memo.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"assistant","timestamp":"2026-08-03T15:13:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Bash"}]}}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := probe.snapshot(path); got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_EXECUTING_TOOL {
		t.Errorf("phase after append = %v, want EXECUTING_TOOL", got.phase)
	}
}

// TestProgressProbeEvictsOneEntryAtTheCap proves the memo degrades gracefully
// at its bound instead of falling off a cliff. Dropping the map wholesale would
// make a daemon sitting just above the cap re-read and re-parse EVERY
// transcript on every 3 s tick — the memo would be worse than useless at
// exactly the scale it exists for. Random single eviction keeps the rest.
func TestProgressProbeEvictsOneEntryAtTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capped.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"aa"}]}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fill the memo to exactly its cap with entries for paths that no longer
	// exist, the way a long-lived daemon accumulates dead chats.
	probe := progressProbe{cache: make(map[string]progressCacheEntry, progressCacheLimit)}
	for i := range progressCacheLimit {
		probe.cache[fmt.Sprintf("/gone/%d.jsonl", i)] = progressCacheEntry{size: 1}
	}

	if got := probe.snapshot(path); got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
		t.Fatalf("phase at the cap = %v, want AWAITING_MODEL", got.phase)
	}
	if got := len(probe.cache); got != progressCacheLimit {
		t.Fatalf("cache size after insert at the cap = %d, want %d (one evicted, one added)", got, progressCacheLimit)
	}
	if _, ok := probe.cache[path]; !ok {
		t.Fatal("the freshly probed transcript is not memoized")
	}
}

// TestProgressProbeDoesNotCacheReadFailures pins the difference between the two
// fail-open answers. A transient read failure (EACCES here; EMFILE and a
// transport hiccup on a networked home are the same shape) says nothing about
// the transcript's CONTENT, so memoizing it against the unchanged (size, mtime)
// would be self-defeating: in the stall this probe exists to detect the file
// never moves again, so a single failed open would suppress the detection for
// the life of the daemon. A content-derived unknown is a property of the bytes
// and must stay cacheable — otherwise every unclassifiable transcript is
// re-read and re-parsed on every 3 s tick.
func TestProgressProbeDoesNotCacheReadFailures(t *testing.T) {
	const classifiable = `{"type":"user","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"aa"}]}}` + "\n"
	// A shape no Claude version emits: read fine, classified as nothing.
	const unclassifiable = `{"type":"mystery-shape","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":"hi"}}` + "\n"

	tests := []struct {
		name string
		// content is the transcript the probe first sees.
		content string
		// breaks makes that first read fail and returns the repair that undoes
		// it WITHOUT touching size or mtime, so the memo key is identical either
		// side of it. nil means nothing is broken.
		breaks func(t *testing.T, path string) (repair func())
		// wantCached is whether the first probe may memoize its answer.
		wantCached bool
		// wantKnownAfterRepair is what the second probe must report once the
		// transient failure has cleared.
		wantKnownAfterRepair bool
	}{
		{
			name:    "transient read failure is not memoized",
			content: classifiable,
			breaks: func(t *testing.T, path string) func() {
				t.Helper()
				if os.Geteuid() == 0 {
					t.Skip("running as root: file permissions do not deny reads")
				}
				if err := os.Chmod(path, 0o000); err != nil {
					t.Fatal(err)
				}
				return func() {
					// chmod moves ctime, never mtime or size — the probe's memo
					// key is byte-identical to the failed read's.
					if err := os.Chmod(path, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			},
			wantCached:           false,
			wantKnownAfterRepair: true,
		},
		{
			name:                 "content-derived unknown stays memoized",
			content:              unclassifiable,
			wantCached:           true,
			wantKnownAfterRepair: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "probe.jsonl")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			repair := func() {}
			if tt.breaks != nil {
				repair = tt.breaks(t, path)
			}

			var probe progressProbe
			if got := probe.snapshot(path); got.known {
				t.Fatalf("first probe known = true, want false (fail open)")
			}
			if _, cached := probe.cache[path]; cached != tt.wantCached {
				t.Errorf("memoized after the first probe = %v, want %v", cached, tt.wantCached)
			}

			repair()

			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("the repair moved the memo key (size %d→%d, mtime %v→%v); the test would prove nothing",
					before.Size(), after.Size(), before.ModTime(), after.ModTime())
			}

			got := probe.snapshot(path)
			if got.known != tt.wantKnownAfterRepair {
				t.Fatalf("second probe known = %v, want %v", got.known, tt.wantKnownAfterRepair)
			}
			if tt.wantKnownAfterRepair && got.phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
				t.Errorf("second probe phase = %v, want AWAITING_MODEL (the real classification)", got.phase)
			}
		})
	}
}

// TestProgressProbeForgetsVanishedTranscript makes sure a deleted transcript
// does not keep serving a stale memoized snapshot.
func TestProgressProbeForgetsVanishedTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","timestamp":"2026-08-03T15:12:38.921Z","message":{"role":"user","content":"hi"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var probe progressProbe
	if got := probe.snapshot(path); !got.known {
		t.Fatal("first probe known = false, want true")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := probe.snapshot(path); got.known {
		t.Error("known = true after the transcript vanished, want false")
	}
}

func TestProbeProgressLiveness_ViaServer(t *testing.T) {
	work := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := filepath.Join(home, ".claude", "projects", pathToProjectKey(work))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(fixturePath("trailing_tool_result.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stalledsess.jsonl"), src, 0o600); err != nil {
		t.Fatal(err)
	}

	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.ProbeProgressLiveness(context.Background(), &bossanovav1.ProbeProgressLivenessRequest{
		WorkDir:        work,
		AgentSessionId: "stalledsess",
	})
	if err != nil {
		t.Fatalf("ProbeProgressLiveness: %v", err)
	}
	if !resp.IsKnown {
		t.Fatal("IsKnown = false, want true")
	}
	if resp.Phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_AWAITING_MODEL {
		t.Errorf("Phase = %v, want AWAITING_MODEL", resp.Phase)
	}
	want, err := time.Parse(time.RFC3339Nano, "2026-08-03T15:12:38.921Z")
	if err != nil {
		t.Fatal(err)
	}
	if resp.LastProgressAt == nil {
		t.Fatal("LastProgressAt = nil, want the tool_result's own timestamp")
	}
	if !resp.LastProgressAt.AsTime().Equal(want) {
		t.Errorf("LastProgressAt = %v, want %v", resp.LastProgressAt.AsTime(), want)
	}
}

// TestProbeProgressLiveness_ViaServerFailsOpen asserts fail-open at the RPC
// boundary: a missing transcript and a traversal-rejected session id both
// return known=false with no error and no timestamp.
func TestProbeProgressLiveness_ViaServerFailsOpen(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	work := t.TempDir()
	srv := &Server{logger: zerolog.Nop()}

	for _, sessionID := range []string{"noexist", "../../etc/passwd"} {
		resp, err := srv.ProbeProgressLiveness(context.Background(), &bossanovav1.ProbeProgressLivenessRequest{
			WorkDir:        work,
			AgentSessionId: sessionID,
		})
		if err != nil {
			t.Fatalf("ProbeProgressLiveness(%q) should not error: %v", sessionID, err)
		}
		if resp.IsKnown {
			t.Errorf("IsKnown = true for %q, want false", sessionID)
		}
		if resp.LastProgressAt != nil {
			t.Errorf("LastProgressAt = %v for %q, want nil", resp.LastProgressAt, sessionID)
		}
		if resp.Phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN {
			t.Errorf("Phase = %v for %q, want UNKNOWN", resp.Phase, sessionID)
		}
	}
}
