package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// fakeProcessInspector is a table-driven stand-in for the real process-tree
// and open-file lister so resolveInteractiveSessionIDByPIDAt can be tested
// without spawning real processes.
type fakeProcessInspector struct {
	descendants map[int][]int
	openFiles   map[int][]string
	// openFilesErr programs a per-pid probe failure, the `lsof` error that used
	// to collapse into an empty file list (BOS-1298). A pid present here fails
	// even when openFiles also names it.
	openFilesErr map[int]error
}

func (f fakeProcessInspector) Descendants(pid int) []int { return f.descendants[pid] }
func (f fakeProcessInspector) OpenFiles(pid int) ([]string, error) {
	if err, ok := f.openFilesErr[pid]; ok {
		return nil, err
	}
	return f.openFiles[pid], nil
}

const (
	uuidA = "019f3a91-3083-7abc-8def-0123456789ab"
	uuidB = "019f3a91-a6bb-7abc-8def-0123456789cd"
)

// errProbeFailed stands in for the `lsof` failure the real inspector reports.
var errProbeFailed = errors.New("lsof -p 200: exit status 1")

func TestRolloutUUIDFromPath(t *testing.T) {
	// The ISO timestamp segment and the UUID both contain dashes, so a naive
	// dash-split is ambiguous; the anchored regex must recover the trailing
	// 36-char UUID.
	path := "/home/u/.codex/sessions/2026/05/08/rollout-2026-05-08T07-45-47-" + uuidA + ".jsonl"
	got, ok := rolloutUUIDFromPath(path)
	if !ok || got != uuidA {
		t.Fatalf("rolloutUUIDFromPath = %q,%v; want %q,true", got, ok, uuidA)
	}
	if _, ok := rolloutUUIDFromPath("/home/u/.codex/sessions/2026/05/08/notes.log"); ok {
		t.Fatal("rolloutUUIDFromPath matched a non-rollout file")
	}
	if _, ok := rolloutUUIDFromPath("/tmp/rollout-2026-05-08T07-45-47-not-a-uuid.jsonl"); ok {
		t.Fatal("rolloutUUIDFromPath matched a rollout without a valid UUID")
	}
}

func TestResolveByPIDSingleOpenRollout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	p := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", time.Now())

	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {p}},
	}
	id, path, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if outcome != fdOutcomeBound {
		t.Fatalf("outcome = %v, want fdOutcomeBound", outcome)
	}
	if id != uuidA {
		t.Errorf("id = %q, want %q", id, uuidA)
	}
	if path != p {
		t.Errorf("path = %q, want %q", path, p)
	}
}

func TestResolveByPIDPrefersCWDMatchOverNewer(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	otherDir := t.TempDir()

	// The cwd-matching rollout is OLDER; the non-matching one is NEWER. A
	// resumed codex holds both open, so cwd-match must win over recency.
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()
	matching := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", older)
	stray := writeSessionMetaRollout(t, root, uuidB, otherDir, "codex-tui", newer)

	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {stray, matching}},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if outcome != fdOutcomeBound {
		t.Fatalf("outcome = %v, want fdOutcomeBound", outcome)
	}
	if id != uuidA {
		t.Errorf("id = %q, want cwd-matching %q", id, uuidA)
	}
}

func TestResolveByPIDPrefersNewestAmongCWDMatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()

	older := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", time.Now().Add(-5*time.Minute))
	newer := writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", time.Now())

	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {older, newer}},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if outcome != fdOutcomeBound {
		t.Fatalf("outcome = %v, want fdOutcomeBound", outcome)
	}
	if id != uuidB {
		t.Errorf("id = %q, want newest %q", id, uuidB)
	}
}

func TestResolveByPIDNoRolloutOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()

	// A descendant exists but holds no codex rollout open.
	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {"/var/log/system.log", filepath.Join(workDir, "main.go")}},
	}
	// The outcome must be the genuine keep-waiting one (the tree WAS enumerated
	// and every probe succeeded) so the caller keeps waiting for this chat's own
	// fd instead of falling back to the time-window scan (BOS-290).
	if _, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100); outcome != fdOutcomeNoRolloutFDOpen {
		t.Fatalf("outcome = %v, want fdOutcomeNoRolloutFDOpen (tree visible, probes fine, no rollout held open yet)", outcome)
	}
}

func TestResolveByPIDNoDescendants(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	insp := fakeProcessInspector{descendants: map[int][]int{}}
	// No descendants ⇒ tree not enumerable, so the caller falls back to the
	// time-window scan (fd inspection unavailable).
	if _, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100); outcome != fdOutcomeTreeNotVisible {
		t.Fatalf("outcome = %v, want fdOutcomeTreeNotVisible (no descendants)", outcome)
	}
}

func TestResolveByPIDNonCodexFilesIgnoredButRolloutFound(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	p := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", time.Now())

	// A rollout-named file OUTSIDE the sessions root must not be mistaken for
	// a codex rollout, and unrelated open files are ignored.
	strayNamed := filepath.Join(t.TempDir(), "rollout-2026-05-08T07-45-47-"+uuidB+".jsonl")
	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {"/dev/null", strayNamed, p}},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if outcome != fdOutcomeBound {
		t.Fatalf("outcome = %v, want fdOutcomeBound", outcome)
	}
	if id != uuidA {
		t.Errorf("id = %q, want %q (rollout under root)", id, uuidA)
	}
}

func TestResolveByPIDIgnoresNonTUIRollout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()

	// A codex-exec rollout (non-interactive) held open under the pane must be
	// ignored; only the codex-tui rollout is a real interactive chat.
	execRollout := writeSessionMetaRollout(t, root, uuidB, workDir, "codex-exec", time.Now())
	tuiRollout := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", time.Now().Add(-time.Minute))

	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {execRollout, tuiRollout}},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if outcome != fdOutcomeBound {
		t.Fatalf("outcome = %v, want fdOutcomeBound", outcome)
	}
	if id != uuidA {
		t.Errorf("id = %q, want codex-tui rollout %q (codex-exec must be ignored)", id, uuidA)
	}
}

func TestResolveByPIDGuardsInvalidPID(t *testing.T) {
	insp := fakeProcessInspector{}
	if _, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, "/root", "/work", 0); outcome != fdOutcomeTreeNotVisible {
		t.Fatalf("outcome = %v for pid 0, want fdOutcomeTreeNotVisible", outcome)
	}
}

// TestResolveByPIDProbeFailureIsNotNoRolloutOpen is the BOS-1298 regression:
// an `lsof` failure used to collapse into an empty file list, which is
// byte-identical to "this process holds no rollout open". The daemon then logged
// the wait-and-it-fixes-itself reason for a fault that never resolves by
// waiting, for the whole discovery budget. Asserted on the returned outcome, not
// on a log line.
func TestResolveByPIDProbeFailureIsNotNoRolloutOpen(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	// A rollout exists on disk; the probe simply cannot be run to see it. That
	// is what makes this fixture able to produce the failure at all — a fixture
	// whose probe succeeds proves nothing about this branch.
	_ = writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", time.Now())

	insp := fakeProcessInspector{
		descendants:  map[int][]int{100: {100, 200}},
		openFilesErr: map[int]error{100: errProbeFailed, 200: errProbeFailed},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if id != "" {
		t.Fatalf("id = %q, want empty: nothing was observed", id)
	}
	if outcome == fdOutcomeNoRolloutFDOpen {
		t.Fatal("outcome = fdOutcomeNoRolloutFDOpen for a tree whose every probe FAILED; a broken probe must not read as a negative observation")
	}
	if outcome != fdOutcomeProbeFailed {
		t.Fatalf("outcome = %v, want fdOutcomeProbeFailed", outcome)
	}
}

// TestResolveByPIDPartialProbeFailureStillBinds pins that a wide tree does not
// lose a real answer to one unreadable process: the act of observing is tracked
// per process, and a rollout found by a process we COULD read still binds.
func TestResolveByPIDPartialProbeFailureStillBinds(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	p := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", time.Now())

	insp := fakeProcessInspector{
		descendants:  map[int][]int{100: {100, 200, 300}},
		openFilesErr: map[int]error{200: errProbeFailed},
		openFiles:    map[int][]string{300: {p}},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if outcome != fdOutcomeBound {
		t.Fatalf("outcome = %v, want fdOutcomeBound: one failed probe must not mask a rollout another process holds open", outcome)
	}
	if id != uuidA {
		t.Errorf("id = %q, want %q", id, uuidA)
	}
}

// TestResolveByPIDPartialProbeFailureWithNoRolloutIsInconclusive pins this
// plan's recorded position: a tree that was only partly observable and turned up
// no rollout is inconclusive, not negative. A negative is what authorises the
// "keep waiting" remedy, and the rollout may sit behind the process we could not
// read.
func TestResolveByPIDPartialProbeFailureWithNoRolloutIsInconclusive(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()

	insp := fakeProcessInspector{
		descendants:  map[int][]int{100: {100, 200}},
		openFilesErr: map[int]error{200: errProbeFailed},
		openFiles:    map[int][]string{100: {"/var/log/system.log"}},
	}
	if _, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100); outcome != fdOutcomeProbeFailed {
		t.Fatalf("outcome = %v, want fdOutcomeProbeFailed for a partially-observed tree with no rollout", outcome)
	}
}

// TestOSProcessInspectorWorksWithoutSystemDirsOnPATH: bossd's service PATH need
// not include /usr/sbin, where macOS keeps lsof. The real inspector must still
// see this process and the file it holds open, or every codex launch probe
// fails and the daemon kills the session.
func TestOSProcessInspectorWorksWithoutSystemDirsOnPATH(t *testing.T) {
	for _, tool := range []string{"ps", "lsof"} {
		if tool == "lsof" && runtime.GOOS == "linux" {
			continue // Linux reads /proc, not lsof.
		}
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed on this host", tool)
		}
	}
	held, err := os.CreateTemp(t.TempDir(), "rollout-held-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	want, err := filepath.EvalSymlinks(held.Name())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	insp := osProcessInspector{}
	pid := os.Getpid()
	if tree := insp.Descendants(pid); !slices.Contains(tree, pid) {
		t.Fatalf("Descendants(%d) = %v, want it to include the process itself", pid, tree)
	}
	files, err := insp.OpenFiles(pid)
	if err != nil {
		t.Fatalf("OpenFiles(%d) with a PATH lacking the system dirs: %v", pid, err)
	}
	for _, f := range files {
		if resolved, err := filepath.EvalSymlinks(f); err == nil && resolved == want {
			return
		}
	}
	t.Fatalf("OpenFiles(%d) = %v, want it to include the held file %s", pid, files, want)
}

// TestOSProcessInspectorExitedPIDHoldsNothing pins the race behind delta's
// "codex process open-file probe failed; rollout fd unreadable": the
// descendant tree is a `ps` snapshot, and a short-lived child that exits
// before its own probe made lsof exit 1 with no output (measured on delta:
// `lsof -p <dead pid> -Fn` prints nothing, rc=1). One such child turned a
// whole tree with no rollout open yet into fdOutcomeProbeFailed. A process
// that no longer exists holds no files, so the probe answers empty, not error.
func TestOSProcessInspectorExitedPIDHoldsNothing(t *testing.T) {
	if runtime.GOOS != "linux" {
		if _, err := exec.LookPath(inspectTool("lsof")); err != nil {
			t.Skip("lsof not installed on this host")
		}
	}
	child := exec.Command("true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	files, err := osProcessInspector{}.OpenFiles(pid)
	if err != nil || len(files) != 0 {
		t.Fatalf("OpenFiles(exited %d) = %v, %v; want no files and no error", pid, files, err)
	}

	root := t.TempDir()
	insp := &fakeProcessInspector{
		descendants: map[int][]int{100: {100, pid}},
		openFiles:   map[int][]string{100: {"/var/log/system.log"}},
	}
	wrapped := exitedChildInspector{fake: insp, exited: pid}
	if _, _, outcome := resolveInteractiveSessionIDByPIDAt(wrapped, root, t.TempDir(), 100); outcome != fdOutcomeNoRolloutFDOpen {
		t.Fatalf("outcome = %v, want fdOutcomeNoRolloutFDOpen: a vanished child is not a probe failure", outcome)
	}
}

// exitedChildInspector serves the fake tree but probes the exited pid with the
// real osProcessInspector, so the classification under test is production's.
type exitedChildInspector struct {
	fake   *fakeProcessInspector
	exited int
}

func (e exitedChildInspector) Descendants(pid int) []int { return e.fake.Descendants(pid) }
func (e exitedChildInspector) OpenFiles(pid int) ([]string, error) {
	if pid == e.exited {
		return osProcessInspector{}.OpenFiles(pid)
	}
	return e.fake.OpenFiles(pid)
}

// TestResolveByPIDNeverBindsAnotherWorktreesRollout pins the cross-repo
// hardening: a pane's process tree holding only a rollout recorded for another
// worktree (another repo's chat) used to bind it as the newest candidate, and
// the chat would then read that repo's transcript. Process ownership does not
// make a foreign conversation this chat's, so the tree reports no rollout of
// its own and a recovery caller falls through to the cwd-filtered scan.
func TestResolveByPIDNeverBindsAnotherWorktreesRollout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	madverts := filepath.Join(t.TempDir(), "madverts-core", "cron-mv-sweep-build-1792103581")
	freshClaim := t.TempDir()
	foreign := writeSessionMetaRollout(t, root, uuidB, freshClaim, "codex-tui", time.Now())

	insp := fakeProcessInspector{
		descendants: map[int][]int{100: {100, 200}},
		openFiles:   map[int][]string{200: {foreign}},
	}
	id, _, outcome := resolveInteractiveSessionIDByPIDAt(insp, root, madverts, 100)
	if outcome != fdOutcomeNoRolloutFDOpen || id != "" {
		t.Fatalf("resolve = %q, %v; want no binding (fdOutcomeNoRolloutFDOpen) for a rollout recorded in %s", id, outcome, freshClaim)
	}
}
