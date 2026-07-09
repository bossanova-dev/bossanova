package main

import (
	"path/filepath"
	"testing"
	"time"
)

// fakeProcessInspector is a table-driven stand-in for the real process-tree
// and open-file lister so resolveInteractiveSessionIDByPIDAt can be tested
// without spawning real processes.
type fakeProcessInspector struct {
	descendants map[int][]int
	openFiles   map[int][]string
}

func (f fakeProcessInspector) Descendants(pid int) []int  { return f.descendants[pid] }
func (f fakeProcessInspector) OpenFiles(pid int) []string { return f.openFiles[pid] }

const (
	uuidA = "019f3a91-3083-7abc-8def-0123456789ab"
	uuidB = "019f3a91-a6bb-7abc-8def-0123456789cd"
)

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
	id, path, ok, treeVisible := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if !treeVisible {
		t.Error("treeVisible = false, want true (descendants enumerated)")
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
	id, _, ok, _ := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if !ok {
		t.Fatal("ok = false, want true")
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
	id, _, ok, _ := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if !ok {
		t.Fatal("ok = false, want true")
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
	// treeVisible must be true (the tree WAS enumerated) so the caller keeps
	// waiting for this chat's own fd instead of falling back to the time-window
	// scan (BOS-290).
	if _, _, ok, treeVisible := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100); ok || !treeVisible {
		t.Fatalf("ok=%v treeVisible=%v, want false/true (tree visible, no rollout held open yet)", ok, treeVisible)
	}
}

func TestResolveByPIDNoDescendants(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	workDir := t.TempDir()
	insp := fakeProcessInspector{descendants: map[int][]int{}}
	// No descendants ⇒ tree not enumerable ⇒ treeVisible false, so the caller
	// falls back to the time-window scan (fd inspection unavailable).
	if _, _, ok, treeVisible := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100); ok || treeVisible {
		t.Fatalf("ok=%v treeVisible=%v, want false/false (no descendants)", ok, treeVisible)
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
	id, _, ok, _ := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if !ok {
		t.Fatal("ok = false, want true")
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
	id, _, ok, _ := resolveInteractiveSessionIDByPIDAt(insp, root, workDir, 100)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if id != uuidA {
		t.Errorf("id = %q, want codex-tui rollout %q (codex-exec must be ignored)", id, uuidA)
	}
}

func TestResolveByPIDGuardsInvalidPID(t *testing.T) {
	insp := fakeProcessInspector{}
	if _, _, ok, treeVisible := resolveInteractiveSessionIDByPIDAt(insp, "/root", "/work", 0); ok || treeVisible {
		t.Fatalf("ok=%v treeVisible=%v for pid 0, want false/false", ok, treeVisible)
	}
}
