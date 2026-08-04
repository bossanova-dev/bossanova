package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/recurser/bossalib/daemonstate"
	"github.com/recurser/bossd/internal/tccprobe"
	"github.com/rs/zerolog"
)

func TestProtectedRootsFor(t *testing.T) {
	t.Parallel()

	home := filepath.Join(string(filepath.Separator), "Users", "alice")
	documents := filepath.Join(home, "Documents")
	desktop := filepath.Join(home, "Desktop")
	downloads := filepath.Join(home, "Downloads")

	tests := []struct {
		name  string
		paths []string
		want  []string
	}{
		{
			name:  "path under Documents selects Documents",
			paths: []string{filepath.Join(documents, "projects", "bossanova")},
			want:  []string{documents},
		},
		{
			name:  "paths under Desktop and Downloads select both roots",
			paths: []string{filepath.Join(downloads, "repo"), filepath.Join(desktop, "worktrees")},
			want:  []string{desktop, downloads},
		},
		{
			name:  "unrelated path selects no roots",
			paths: []string{filepath.Join(home, "Code", "bossanova")},
		},
		{
			name:  "path equal to root selects it",
			paths: []string{documents},
			want:  []string{documents},
		},
		{
			name: "duplicate paths under one root collapse",
			paths: []string{
				filepath.Join(documents, "repo"),
				filepath.Join(documents, "repo", "worktrees"),
				documents,
			},
			want: []string{documents},
		},
		{
			name:  "similarly prefixed sibling is excluded",
			paths: []string{filepath.Join(home, "Documents-old", "repo")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := protectedRootsFor(home, tt.paths); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("protectedRootsFor(%q, %q) = %q, want %q", home, tt.paths, got, tt.want)
			}
		})
	}
}

func TestProtectedRootsForResolvedFollowsSymlinkedCandidates(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp home: %v", err)
	}
	outside := t.TempDir()

	tests := []struct {
		name          string
		protectedRoot string
		candidateLeaf string
	}{
		{name: "repository path reaches Documents", protectedRoot: "Documents", candidateLeaf: "repo"},
		{name: "worktree base reaches Documents", protectedRoot: "Documents", candidateLeaf: "worktrees"},
		{name: "repository path reaches Desktop", protectedRoot: "Desktop", candidateLeaf: "repo"},
		{name: "worktree base reaches Desktop", protectedRoot: "Desktop", candidateLeaf: "worktrees"},
		{name: "repository path reaches Downloads", protectedRoot: "Downloads", candidateLeaf: "repo"},
		{name: "worktree base reaches Downloads", protectedRoot: "Downloads", candidateLeaf: "worktrees"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			protectedRoot := filepath.Join(home, tt.protectedRoot)
			if err := os.MkdirAll(filepath.Join(protectedRoot, tt.candidateLeaf), 0o755); err != nil {
				t.Fatalf("create protected target: %v", err)
			}
			link := filepath.Join(outside, tt.protectedRoot+"-"+tt.candidateLeaf+"-link")
			if err := os.Symlink(protectedRoot, link); err != nil {
				t.Skipf("create symlink: %v", err)
			}

			roots, blocked := protectedRootsForResolved(
				home,
				[]string{filepath.Join(link, tt.candidateLeaf)},
				time.Second,
				filepath.EvalSymlinks,
			)
			if want := []string{protectedRoot}; !reflect.DeepEqual(roots, want) {
				t.Fatalf("protectedRootsForResolved() roots = %q, want %q", roots, want)
			}
			if len(blocked) != 0 {
				t.Fatalf("protectedRootsForResolved() blocked = %#v, want none", blocked)
			}
		})
	}
}

func TestProtectedRootsForResolvedRecordsResolverErrors(t *testing.T) {
	home := t.TempDir()
	candidate := filepath.Join(t.TempDir(), "broken-link", "repo")

	roots, diagnostics := protectedRootsForResolved(home, []string{candidate}, time.Second, func(string) (string, error) {
		return "", errors.New("broken symlink")
	})
	if len(roots) != 0 {
		t.Fatalf("protectedRootsForResolved() roots = %q, want none", roots)
	}
	if len(diagnostics) != 1 || diagnostics[0].Path != candidate || diagnostics[0].Status != tccprobe.StatusError || diagnostics[0].Err == nil {
		t.Fatalf("protectedRootsForResolved() diagnostics = %#v, want error result for %q", diagnostics, candidate)
	}
}

func TestProtectedRootsForResolvedTracksTimedOutWorkers(t *testing.T) {
	home := t.TempDir()
	candidate := filepath.Join(t.TempDir(), "symlinked", "repo")
	release := make(chan struct{})
	var workerDone []<-chan struct{}

	_, diagnostics := protectedRootsForResolvedWithTracker(home, []string{candidate}, 20*time.Millisecond, func(string) (string, error) {
		<-release
		return "", nil
	}, func(done <-chan struct{}) {
		workerDone = append(workerDone, done)
	})
	if len(diagnostics) != 1 || diagnostics[0].Status != tccprobe.StatusBlocked {
		t.Fatalf("protectedRootsForResolvedWithTracker() diagnostics = %#v, want blocked result", diagnostics)
	}
	if len(workerDone) != 1 {
		t.Fatalf("tracked worker handles = %d, want 1", len(workerDone))
	}

	close(release)
	select {
	case <-workerDone[0]:
	case <-time.After(time.Second):
		t.Fatal("timed-out resolver worker completion was not tracked")
	}
}

func TestProtectedRootsForResolvedPersistsBlockedTimeout(t *testing.T) {
	home := t.TempDir()
	candidate := filepath.Join(t.TempDir(), "symlinked", "repo")
	release := make(chan struct{})
	timeout := 20 * time.Millisecond

	started := time.Now()
	roots, blocked := protectedRootsForResolved(home, []string{candidate}, timeout, func(string) (string, error) {
		<-release
		return "", nil
	})
	elapsed := time.Since(started)
	close(release)

	if elapsed > 2*timeout+20*time.Millisecond {
		t.Fatalf("protectedRootsForResolved() took %s, want roughly within twice %s", elapsed, timeout)
	}
	if len(roots) != 0 {
		t.Fatalf("protectedRootsForResolved() roots = %q, want none", roots)
	}
	if len(blocked) != 1 {
		t.Fatalf("protectedRootsForResolved() blocked = %#v, want one result", blocked)
	}
	if got := blocked[0]; got.Path != candidate || got.Status != tccprobe.StatusBlocked || got.Err == nil || !strings.Contains(got.Err.Error(), "symlink resolution timed out") {
		t.Fatalf("blocked result = %#v, want timeout diagnostic for %q", got, candidate)
	}

	var persisted daemonstate.Metadata
	persistTCCProbeResults(zerolog.Nop(), t.TempDir(), daemonstate.Metadata{}, blocked, func(_ string, metadata daemonstate.Metadata) error {
		persisted = metadata
		return nil
	})
	if len(persisted.TCCProbeResults) != 1 || persisted.TCCProbeResults[0].Status != daemonstate.TCCProbeStatusBlocked || persisted.TCCProbeResults[0].Path != candidate || !strings.Contains(persisted.TCCProbeResults[0].Diagnostic, "symlink resolution timed out") {
		t.Fatalf("persisted blocked diagnostic = %#v, want timeout result for %q", persisted.TCCProbeResults, candidate)
	}
}

func TestProtectedRootsForResolvedSelectsLexicalAndResolvedRoots(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp home: %v", err)
	}
	documents := filepath.Join(home, "Documents")
	desktop := filepath.Join(home, "Desktop")
	if err := os.MkdirAll(filepath.Join(desktop, "repo"), 0o755); err != nil {
		t.Fatalf("create Desktop repo: %v", err)
	}
	if err := os.Symlink(desktop, documents); err != nil {
		t.Skipf("create Documents symlink: %v", err)
	}

	roots, blocked := protectedRootsForResolved(home, []string{filepath.Join(documents, "repo")}, time.Second, filepath.EvalSymlinks)
	if want := []string{documents, desktop}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("protectedRootsForResolved() roots = %q, want %q", roots, want)
	}
	if len(blocked) != 0 {
		t.Fatalf("protectedRootsForResolved() diagnostics = %#v, want none", blocked)
	}
}

func TestProtectedRootsForResolvedBoundsBlockedBatch(t *testing.T) {
	home := t.TempDir()
	const candidateCount = 6
	candidates := make([]string, candidateCount)
	for i := range candidates {
		candidates[i] = filepath.Join(t.TempDir(), "candidate", string(rune('a'+i)))
	}

	release := make(chan struct{})
	timeout := 40 * time.Millisecond
	var mu sync.Mutex
	inFlight, maxInFlight, calls := 0, 0, 0
	resolver := func(string) (string, error) {
		mu.Lock()
		calls++
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
		return "", nil
	}

	started := time.Now()
	roots, blocked := protectedRootsForResolved(home, candidates, timeout, resolver)
	elapsed := time.Since(started)
	close(release)

	if elapsed > 2*timeout+30*time.Millisecond {
		t.Fatalf("protectedRootsForResolved() took %s, want roughly within twice %s", elapsed, timeout)
	}
	if len(roots) != 0 {
		t.Fatalf("protectedRootsForResolved() roots = %q, want none", roots)
	}
	if len(blocked) != len(candidates) {
		t.Fatalf("protectedRootsForResolved() blocked = %#v, want one result per candidate", blocked)
	}
	for i, result := range blocked {
		if result.Path != candidates[i] || result.Status != tccprobe.StatusBlocked || result.Err == nil || !strings.Contains(result.Err.Error(), "symlink resolution timed out") {
			t.Fatalf("blocked[%d] = %#v, want timeout diagnostic for %q", i, result, candidates[i])
		}
	}
	mu.Lock()
	gotCalls, gotMaxInFlight := calls, maxInFlight
	mu.Unlock()
	if gotMaxInFlight > 3 {
		t.Fatalf("max simultaneous resolver calls = %d, want <= 3", gotMaxInFlight)
	}
	if gotCalls > 3 {
		t.Fatalf("resolver calls before deadline = %d, want <= 3", gotCalls)
	}
}

func TestProtectedRootsForResolvedMixIsDeterministic(t *testing.T) {
	home := t.TempDir()
	documents := filepath.Join(home, "Documents")
	desktop := filepath.Join(home, "Desktop")
	lexical := filepath.Join(documents, "repo")
	viaSymlink := filepath.Join(t.TempDir(), "desktop-link", "repo")
	broken := filepath.Join(t.TempDir(), "broken-link", "repo")
	paths := []string{lexical, viaSymlink, broken, lexical}

	roots, blocked := protectedRootsForResolved(home, paths, time.Second, func(candidate string) (string, error) {
		switch candidate {
		case viaSymlink:
			return filepath.Join(desktop, "repo"), nil
		case broken:
			return "", errors.New("broken symlink")
		default:
			return candidate, nil
		}
	})
	if want := []string{documents, desktop}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("protectedRootsForResolved() roots = %q, want %q", roots, want)
	}
	if len(blocked) != 1 || blocked[0].Path != broken || blocked[0].Status != tccprobe.StatusError || blocked[0].Err == nil {
		t.Fatalf("protectedRootsForResolved() diagnostics = %#v, want error result for %q", blocked, broken)
	}
}

func TestProtectedRootsForResolvedAtPreventsQueuedLaunchAfterDeadline(t *testing.T) {
	home := t.TempDir()
	paths := []string{
		filepath.Join(t.TempDir(), "one"),
		filepath.Join(t.TempDir(), "two"),
		filepath.Join(t.TempDir(), "three"),
		filepath.Join(t.TempDir(), "queued"),
	}
	deadline := time.Now().Add(time.Second)
	var launchChecks atomic.Int32
	var calls atomic.Int32
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	type selection struct {
		blocked []tccprobe.Result
	}
	selected := make(chan selection, 1)

	go func() {
		_, blocked := protectedRootsForResolvedAt(
			home,
			paths,
			time.Second,
			deadline,
			func() time.Time {
				if launchChecks.Add(1) > 3 {
					return deadline
				}
				return deadline.Add(-time.Second)
			},
			func(candidate string) (string, error) {
				calls.Add(1)
				started <- struct{}{}
				<-release
				return candidate, nil
			},
		)
		selected <- selection{blocked: blocked}
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("initial resolver workers did not start")
		}
	}
	close(release)
	blocked := (<-selected).blocked
	if got := calls.Load(); got != 3 {
		t.Fatalf("resolver calls = %d, want only the three initial workers", got)
	}
	queuedBlocked := false
	for _, result := range blocked {
		if result.Path == paths[3] && result.Status == tccprobe.StatusBlocked {
			queuedBlocked = true
			break
		}
	}
	if !queuedBlocked {
		t.Fatalf("blocked results = %#v, want queued candidate %q", blocked, paths[3])
	}
}

func TestDeadlineDrainRetainsBufferedCompletionAsResolvedRoot(t *testing.T) {
	home := t.TempDir()
	documents := filepath.Join(home, "Documents")
	desktop := filepath.Join(home, "Desktop")
	completed := make(chan symlinkResolutionResult, 1)
	completed <- symlinkResolutionResult{index: 0, path: filepath.Join(desktop, "repo")}
	results := make([]symlinkResolutionResult, 1)
	finished := make([]bool, 1)

	drainCompletedResolutions(completed, results, finished)
	roots := mergeResolvedRoots(home, []string{documents}, results, finished)
	if want := []string{documents, desktop}; !reflect.DeepEqual(roots, want) {
		t.Fatalf("roots after deadline drain = %q, want %q", roots, want)
	}
}

func TestProtectedRootsForResolvedFindsMissingLeafBelowSymlinkedAncestor(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp home: %v", err)
	}
	code := t.TempDir()

	for _, rootName := range []string{"Documents", "Desktop", "Downloads"} {
		t.Run(rootName, func(t *testing.T) {
			protectedRoot := filepath.Join(home, rootName)
			if err := os.MkdirAll(protectedRoot, 0o755); err != nil {
				t.Fatalf("create protected root: %v", err)
			}
			link := filepath.Join(code, rootName+"-code")
			if err := os.Symlink(protectedRoot, link); err != nil {
				t.Skipf("create Code symlink: %v", err)
			}

			roots, diagnostics := protectedRootsForResolved(home, []string{filepath.Join(link, "worktrees")}, time.Second, filepath.EvalSymlinks)
			if want := []string{protectedRoot}; !reflect.DeepEqual(roots, want) {
				t.Fatalf("protectedRootsForResolved() roots = %q, want %q", roots, want)
			}
			if len(diagnostics) != 0 {
				t.Fatalf("protectedRootsForResolved() diagnostics = %#v, want none", diagnostics)
			}
		})
	}
}

func TestProtectedRootsForResolvedPersistsImmediatePermissionDiagnostic(t *testing.T) {
	home := t.TempDir()
	candidate := filepath.Join(t.TempDir(), "Code", "worktrees")

	_, diagnostics := protectedRootsForResolved(home, []string{candidate}, time.Second, func(string) (string, error) {
		return "", os.ErrPermission
	})
	if len(diagnostics) != 1 || diagnostics[0].Path != candidate || diagnostics[0].Status != tccprobe.StatusDenied || !errors.Is(diagnostics[0].Err, os.ErrPermission) {
		t.Fatalf("resolution diagnostics = %#v, want denied result for %q", diagnostics, candidate)
	}

	var persisted daemonstate.Metadata
	persistTCCProbeResults(zerolog.Nop(), t.TempDir(), daemonstate.Metadata{}, diagnostics, func(_ string, metadata daemonstate.Metadata) error {
		persisted = metadata
		return nil
	})
	if len(persisted.TCCProbeResults) != 1 || persisted.TCCProbeResults[0].Path != candidate || persisted.TCCProbeResults[0].Status != daemonstate.TCCProbeStatusDenied || !strings.Contains(persisted.TCCProbeResults[0].Diagnostic, os.ErrPermission.Error()) {
		t.Fatalf("persisted diagnostics = %#v, want denied result for %q", persisted.TCCProbeResults, candidate)
	}
}

func TestPersistTCCProbeResultsConvertsEveryStatus(t *testing.T) {
	t.Parallel()

	results := []tccprobe.Result{
		{Path: "/Documents", Status: tccprobe.StatusOK},
		{Path: "/Desktop", Status: tccprobe.StatusDenied, Err: os.ErrPermission},
		{Path: "/Downloads", Status: tccprobe.StatusBlocked, Err: errors.New("probe timed out")},
		{Path: "/Missing", Status: tccprobe.StatusAbsent, Err: os.ErrNotExist},
		{Path: "/Unexpected", Status: tccprobe.StatusError, Err: errors.New("too many open files")},
	}
	base := daemonstate.Metadata{PID: 42, ExecutablePath: "/stable/bossd"}
	var persisted daemonstate.Metadata

	got := persistTCCProbeResults(zerolog.Nop(), "/state", base, results, func(_ string, metadata daemonstate.Metadata) error {
		persisted = metadata
		return nil
	})

	want := []daemonstate.TCCProbeResult{
		{Path: "/Documents", Status: daemonstate.TCCProbeStatusOK},
		{Path: "/Desktop", Status: daemonstate.TCCProbeStatusDenied, Diagnostic: os.ErrPermission.Error()},
		{Path: "/Downloads", Status: daemonstate.TCCProbeStatusBlocked, Diagnostic: "probe timed out"},
		{Path: "/Missing", Status: daemonstate.TCCProbeStatusAbsent, Diagnostic: os.ErrNotExist.Error()},
		{Path: "/Unexpected", Status: daemonstate.TCCProbeStatusError, Diagnostic: "too many open files"},
	}
	if !reflect.DeepEqual(got.TCCProbeResults, want) {
		t.Fatalf("persistTCCProbeResults() results = %#v, want %#v", got.TCCProbeResults, want)
	}
	if !reflect.DeepEqual(persisted, got) {
		t.Fatalf("persisted metadata = %#v, want %#v", persisted, got)
	}
	if got.PID != base.PID || got.ExecutablePath != base.ExecutablePath {
		t.Fatalf("persistTCCProbeResults() lost daemon identity: %#v", got)
	}
	if !got.TCCProbeCompleted {
		t.Fatal("persistTCCProbeResults() did not mark the probe complete")
	}
}

func TestPersistTCCProbeResultsMarksZeroRootProbeComplete(t *testing.T) {
	t.Parallel()

	var persisted daemonstate.Metadata
	got := persistTCCProbeResults(zerolog.Nop(), "/state", daemonstate.Metadata{PID: 42}, nil, func(_ string, metadata daemonstate.Metadata) error {
		persisted = metadata
		return nil
	})
	if !got.TCCProbeCompleted || !persisted.TCCProbeCompleted {
		t.Fatal("zero-root probe completion was not persisted")
	}
	if len(got.TCCProbeResults) != 0 || len(persisted.TCCProbeResults) != 0 {
		t.Fatalf("zero-root probe results = %#v persisted %#v, want empty", got.TCCProbeResults, persisted.TCCProbeResults)
	}
}

func TestPersistTCCProbeResultsWriteFailureIsObservational(t *testing.T) {
	t.Parallel()

	wrote := false
	got := persistTCCProbeResults(
		zerolog.Nop(),
		"/state",
		daemonstate.Metadata{PID: 42},
		[]tccprobe.Result{{Path: "/Documents", Status: tccprobe.StatusDenied}},
		func(string, daemonstate.Metadata) error {
			wrote = true
			return errors.New("disk full")
		},
	)
	if !wrote {
		t.Fatal("persistTCCProbeResults() did not attempt state write")
	}
	if len(got.TCCProbeResults) != 1 || got.TCCProbeResults[0].Status != daemonstate.TCCProbeStatusDenied {
		t.Fatalf("persistTCCProbeResults() = %#v, want retained denied result", got)
	}
}

func TestStagedBinaryStale(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	staged := writeExecutableFile(t, dir, "staged-bossd", "current")
	same := writeExecutableFile(t, dir, "same-bossd", "current")
	different := writeExecutableFile(t, dir, "different-bossd", "newer")
	notStaged := writeExecutableFile(t, dir, "running-elsewhere", "current")

	tests := []struct {
		name    string
		running string
		staged  string
		source  string
		want    bool
	}{
		{
			name:    "same bytes are current",
			running: staged,
			staged:  staged,
			source:  same,
		},
		{
			name:    "different bytes are stale",
			running: staged,
			staged:  staged,
			source:  different,
			want:    true,
		},
		{
			name:    "non-staged running executable is ignored",
			running: notStaged,
			staged:  staged,
			source:  different,
		},
		{
			name:    "non-staged running executable does not require staged file",
			running: notStaged,
			staged:  filepath.Join(dir, "missing-staged-bossd"),
			source:  different,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := stagedBinaryStale(tt.running, tt.staged, tt.source)
			if err != nil {
				t.Fatalf("stagedBinaryStale() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("stagedBinaryStale() = %v, want %v", got, tt.want)
			}
		})
	}
}

func writeExecutableFile(t *testing.T, dir, name, contents string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatalf("write executable %s: %v", name, err)
	}
	return path
}
