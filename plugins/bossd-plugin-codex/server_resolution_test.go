package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestResolveInteractiveSessionIDRPCPrefersPaneFDResolution(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	// Two sibling codex chats in the SAME worktree launched in the same
	// window. The time-window scan is ambiguous (two codex-tui rollouts), but
	// fd resolution binds THIS chat's pane to its OWN rollout.
	launchedAfter := time.Now().Add(-2 * time.Second)
	own := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", launchedAfter.Add(time.Second))
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {own}},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found {
		t.Fatal("Found = false, want true")
	}
	if resp.SessionId != uuidA {
		t.Errorf("SessionId = %q, want fd-resolved %q", resp.SessionId, uuidA)
	}
	if resp.Ambiguous {
		t.Error("Ambiguous = true, want false (fd resolution is unambiguous)")
	}
}

func TestResolveInteractiveSessionIDRPCFallsBackWhenFDInspectionUnavailable(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	launchedAfter := time.Now().Add(-2 * time.Second)
	path := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	// The process tree can't be enumerated at all (empty descendants ⇒ `ps`
	// failed / pane gone ⇒ fd inspection unavailable on this host). ONLY then do
	// we fall back to the time-window scan, which resolves the single match.
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{},
		openFiles:   map[int][]string{},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found || resp.SessionId != uuidA {
		t.Fatalf("Found=%v SessionId=%q, want true/%q via time-window fallback", resp.Found, resp.SessionId, uuidA)
	}
	if resp.TranscriptPath != path {
		t.Errorf("TranscriptPath = %q, want %q", resp.TranscriptPath, path)
	}
}

// TestResolveInteractiveSessionIDRPCWaitsForOwnFDWhenTreeVisible is the direct
// BOS-290 regression for the eager-fallback hole: when a chat's pane process
// tree IS visible but its own codex hasn't opened its rollout yet, the plugin
// must report Found=false so the daemon keeps polling for THIS chat's own fd —
// it must NOT fall through to the time-window scan and bind a sibling chat's
// already-written rollout (which the -2s window slack would otherwise surface as
// the single unambiguous match).
func TestResolveInteractiveSessionIDRPCWaitsForOwnFDWhenTreeVisible(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	// A sibling chat's rollout already exists in this worktree and sits inside
	// the target chat's time-window (the collision the eager fallback caused).
	launchedAfter := time.Now().Add(-2 * time.Second)
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	// The target chat's pane tree is visible (ps enumerated it) but its codex
	// holds no rollout open yet.
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {}},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if resp.Found {
		t.Fatalf("Found=true SessionId=%q — must NOT bind the sibling's rollout; keep polling for the chat's own fd (BOS-290)", resp.SessionId)
	}
}

func TestResolveInteractiveSessionIDRPCUsesRolloutMetadata(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "") // guard against CODEX_HOME leaking in from the ambient environment
	workDir := t.TempDir()
	launchedAfter := time.Date(2026, 5, 8, 7, 45, 40, 0, time.UTC)
	path := writeSessionMetaRollout(t,
		filepath.Join(tmpHome, codexSessionsDir),
		"rpc-session",
		workDir,
		"codex-tui",
		launchedAfter.Add(time.Second),
	)

	s := newTestServer(t)
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found {
		t.Fatal("Found = false, want true")
	}
	if resp.SessionId != "rpc-session" {
		t.Errorf("SessionId = %q, want rpc-session", resp.SessionId)
	}
	if resp.TranscriptPath != path {
		t.Errorf("TranscriptPath = %q, want %q", resp.TranscriptPath, path)
	}
	if resp.Ambiguous {
		t.Error("Ambiguous = true, want false")
	}
	if resp.Reason != "" {
		t.Errorf("Reason = %q, want empty", resp.Reason)
	}
}

// TestResolveInteractiveSessionIDRPCPaneBindingBeatsAmbiguousScan is the
// BOS-1144 reproduction. Two codex-tui rollouts exist for one worktree — the
// ordinary shape once an operator opens a second chat on a session. Resolved
// the way the launch path used to ask (no pane pid, no launch floor) the scan
// can only report ambiguity, which is why those chats were left with no
// provider_session_id and reopened into a fresh conversation. Resolved with the
// chat's own pane pid, the same corpus binds deterministically to the rollout
// that pane's codex holds open.
func TestResolveInteractiveSessionIDRPCPaneBindingBeatsAmbiguousScan(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	written := time.Now().Add(-time.Second)
	own := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", written)
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", written)

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {own}},
	}

	// (a) The pre-fix request shape: no pane pid, no launch floor.
	scanned, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir: workDir,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID(scan): %v", err)
	}
	if !scanned.Ambiguous {
		t.Fatalf("Ambiguous = false (SessionId %q), want true: two codex-tui rollouts share this worktree", scanned.SessionId)
	}
	if scanned.Found {
		t.Error("Found = true, want false for an ambiguous scan")
	}
	if scanned.Reason != "multiple matching codex-tui rollouts found" {
		t.Errorf("Reason = %q, want %q", scanned.Reason, "multiple matching codex-tui rollouts found")
	}

	// (b) The fixed request shape: the chat's own pane pid resolves it.
	bound, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(written.Add(-time.Second)),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID(pane): %v", err)
	}
	if !bound.Found {
		t.Fatal("Found = false, want true: the pane's codex holds a rollout open")
	}
	if bound.SessionId != uuidA {
		t.Errorf("SessionId = %q, want the pane's own rollout %q", bound.SessionId, uuidA)
	}
	if bound.Ambiguous {
		t.Error("Ambiguous = true, want false: fd resolution is unambiguous")
	}
}

// TestResolveInteractiveSessionIDRPCNamesDistinctMissReasons pins that every
// miss explains itself, and that the two misses are told apart. "The fd is not
// open yet" is a retry-and-it-will-appear; "nothing matched" is a wrong window
// or a chat that never wrote a rollout. Collapsing them into one silent
// Found=false is what made the unbound chats invisible in the daemon log.
func TestResolveInteractiveSessionIDRPCNamesDistinctMissReasons(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	// A sibling's rollout exists, but the target pane's codex has opened none.
	launchedAfter := time.Now().Add(-2 * time.Second)
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", launchedAfter.Add(time.Second))

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {}},
	}
	fdMiss, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       workDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
		PanePid:       4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID(fd miss): %v", err)
	}
	if fdMiss.Found {
		t.Fatal("Found = true, want false while the pane's own fd is not open")
	}
	if fdMiss.Reason == "" {
		t.Fatal("Reason is empty for an fd miss with a visible process tree")
	}
	if fdMiss.Reason != reasonPaneRolloutFDNotOpenYet {
		t.Errorf("Reason = %q, want %q", fdMiss.Reason, reasonPaneRolloutFDNotOpenYet)
	}

	// Nothing at all in the window: the scan's own miss.
	emptyWorkDir := t.TempDir()
	scanMiss, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:       emptyWorkDir,
		LaunchedAfter: timestamppb.New(launchedAfter),
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID(scan miss): %v", err)
	}
	if scanMiss.Found {
		t.Fatal("Found = true, want false for a worktree with no rollouts")
	}
	if scanMiss.Reason == "" {
		t.Fatal("Reason is empty for a scan fall-through miss")
	}
	if scanMiss.Reason != reasonNoRolloutFound {
		t.Errorf("Reason = %q, want %q", scanMiss.Reason, reasonNoRolloutFound)
	}
	if fdMiss.Reason == scanMiss.Reason {
		t.Fatalf("both misses report the same reason %q; they must be distinguishable", fdMiss.Reason)
	}
}

// TestResolveInteractiveSessionIDRPCReasonLadderIsOrderedByRemedy pins the
// BOS-1298 ordering: the two misses that never resolve on their own are reported
// AHEAD of the one that routinely does. "fd not open yet" is routinely true on a
// cold worktree, so leaving it first shadows every real fault behind it — which
// is how a broken probe was reported as a slow codex for eleven hours.
func TestResolveInteractiveSessionIDRPCReasonLadderIsOrderedByRemedy(t *testing.T) {
	newServerWithRoot := func(t *testing.T) (*Server, string, string) {
		t.Helper()
		tmpHome := t.TempDir()
		t.Setenv("HOME", tmpHome)
		t.Setenv("CODEX_HOME", "")
		return newTestServer(t), filepath.Join(tmpHome, codexSessionsDir), t.TempDir()
	}

	// Rung 1: the probe itself failed. A rollout DOES exist under the root, so a
	// ladder that tested cheapness first would answer "fd not open yet" here.
	t.Run("probe failure outranks fd-not-open-yet", func(t *testing.T) {
		s, root, workDir := newServerWithRoot(t)
		_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", time.Now())
		s.inspector = fakeProcessInspector{
			descendants:  map[int][]int{4242: {4242, 5252}},
			openFilesErr: map[int]error{4242: errProbeFailed, 5252: errProbeFailed},
		}
		got, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
			WorkDir:       workDir,
			LaunchedAfter: timestamppb.New(time.Now().Add(-2 * time.Second)),
			PanePid:       4242,
		})
		if err != nil {
			t.Fatalf("ResolveInteractiveSessionID: %v", err)
		}
		if got.Found {
			t.Fatal("Found = true, want false: nothing was observed")
		}
		if got.Reason == reasonPaneRolloutFDNotOpenYet {
			t.Fatal("Reason = the wait-and-it-fixes-itself reason for a FAILED probe; the ladder must report the probe failure, which never resolves by waiting")
		}
		if got.Reason != reasonPaneProbeFailed {
			t.Errorf("Reason = %q, want %q", got.Reason, reasonPaneProbeFailed)
		}
	})

	// Rung 2: probes fine, tree holds nothing, and the sessions root holds no
	// rollout at all — a wrong CODEX_HOME, not a slow codex.
	t.Run("no rollout under the sessions root outranks fd-not-open-yet", func(t *testing.T) {
		s, _, workDir := newServerWithRoot(t)
		s.inspector = fakeProcessInspector{
			descendants: map[int][]int{4242: {4242, 5252}},
			openFiles:   map[int][]string{5252: {"/var/log/system.log"}},
		}
		got, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
			WorkDir:       workDir,
			LaunchedAfter: timestamppb.New(time.Now().Add(-2 * time.Second)),
			PanePid:       4242,
		})
		if err != nil {
			t.Fatalf("ResolveInteractiveSessionID: %v", err)
		}
		if got.Found {
			t.Fatal("Found = true, want false")
		}
		if got.Reason != reasonNoRolloutFound {
			t.Errorf("Reason = %q, want %q for an empty sessions root", got.Reason, reasonNoRolloutFound)
		}
	})

	// Rung 3: the genuine wait. Rollouts exist, this tree holds none of them.
	t.Run("rollouts exist but none held by this tree is the wait case", func(t *testing.T) {
		s, root, workDir := newServerWithRoot(t)
		_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", time.Now())
		s.inspector = fakeProcessInspector{
			descendants: map[int][]int{4242: {4242, 5252}},
			openFiles:   map[int][]string{5252: {"/var/log/system.log"}},
		}
		got, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
			WorkDir:       workDir,
			LaunchedAfter: timestamppb.New(time.Now().Add(-2 * time.Second)),
			PanePid:       4242,
		})
		if err != nil {
			t.Fatalf("ResolveInteractiveSessionID: %v", err)
		}
		if got.Found {
			t.Fatal("Found = true, want false")
		}
		if got.Reason != reasonPaneRolloutFDNotOpenYet {
			t.Errorf("Reason = %q, want %q", got.Reason, reasonPaneRolloutFDNotOpenYet)
		}
	})
}

// TestResolveMissReasonConstantsArePinned keeps each rung's wire value stable.
// The daemon logs these verbatim and operators grep for them, so a rename must
// show up as a diff on this test rather than silently changing what an incident
// report matches on.
func TestResolveMissReasonConstantsArePinned(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{reasonPaneProbeFailed, "codex process open-file probe failed; rollout fd unreadable"},
		{reasonNoRolloutFound, "no matching codex-tui rollout found"},
		{reasonPaneRolloutFDNotOpenYet, "codex process tree visible but no rollout fd open yet"},
	} {
		if tc.got != tc.want {
			t.Errorf("reason constant = %q, want %q", tc.got, tc.want)
		}
	}
}

// TestResolveInteractiveSessionIDRPCUsesFDEvenOnLegacyBackfill pins BOS-1298's
// gate change. Recovery callers (wake, RecordChat resume, the host-service
// lookup) send AllowLegacyBackfill because the time-window scan is their
// fallback — but when they ALSO know the chat's live pane, fd resolution is the
// authoritative answer and must not be skipped. The fixture makes the
// time-window scan ambiguous, so a bind here cannot have come from it.
func TestResolveInteractiveSessionIDRPCUsesFDEvenOnLegacyBackfill(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	chatCreatedAt := time.Now().Add(-time.Minute)
	// Two codex-tui rollouts share this worktree inside the legacy window, so
	// the scan is ambiguous and can bind nothing.
	held := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", chatCreatedAt)
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", chatCreatedAt.Add(time.Second))

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {held}},
	}

	// (a) Without a pane pid the recovery caller gets the ambiguous scan — the
	// pre-fix behaviour, kept for chats with no live process.
	scanned, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:             workDir,
		ChatCreatedAt:       timestamppb.New(chatCreatedAt),
		AllowLegacyBackfill: true,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID(no pane): %v", err)
	}
	if !scanned.Ambiguous || scanned.Found {
		t.Fatalf("legacy scan Found=%v Ambiguous=%v, want false/true — otherwise (b) below proves nothing", scanned.Found, scanned.Ambiguous)
	}

	// (b) With the chat's live pane, fd resolution runs and binds.
	bound, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:             workDir,
		ChatCreatedAt:       timestamppb.New(chatCreatedAt),
		AllowLegacyBackfill: true,
		PanePid:             4242,
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID(pane): %v", err)
	}
	if !bound.Found {
		t.Fatal("Found = false: a recovery caller with a live pane must still get fd resolution")
	}
	if bound.SessionId != uuidA {
		t.Errorf("SessionId = %q, want the rollout the pane's process holds open %q", bound.SessionId, uuidA)
	}
	if bound.Ambiguous {
		t.Error("Ambiguous = true, want false: fd resolution is unambiguous")
	}
}

// TestResolveInteractiveSessionIDRPCRecoveryFallsThroughOnFDMiss is the
// BOS-1298 round-2 regression. Widening the fd gate to "pane pid alone" put the
// three recovery callers (wake, RecordChat resume, the host-service lookup)
// inside a block whose every rung returned early, so a chat whose codex had
// exited — while tmux `remain-on-exit` still reported a live pane pid — could
// never be bound by anything. A recovery caller must fall THROUGH a non-binding
// fd outcome to the time-window scan; only a launch poll stops at the ladder.
func TestResolveInteractiveSessionIDRPCRecoveryFallsThroughOnFDMiss(t *testing.T) {
	cases := []struct {
		name string
		insp fakeProcessInspector
	}{
		{
			// The pane pid is live (remain-on-exit zombie) but its codex is gone,
			// so every open-files probe errors: fdOutcomeProbeFailed.
			name: "probe failed",
			insp: fakeProcessInspector{
				descendants:  map[int][]int{4242: {4242}},
				openFilesErr: map[int]error{4242: errProbeFailed},
			},
		},
		{
			// The tree reads cleanly and simply holds no rollout open:
			// fdOutcomeNoRolloutFDOpen.
			name: "no rollout fd open",
			insp: fakeProcessInspector{
				descendants: map[int][]int{4242: {4242, 5252}},
				openFiles:   map[int][]string{5252: {}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			t.Setenv("CODEX_HOME", "")
			workDir := t.TempDir()
			root := filepath.Join(tmpHome, codexSessionsDir)

			createdAt := time.Now().Add(-time.Minute)
			path := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", createdAt.Add(time.Second))

			s := newTestServer(t)
			s.inspector = tc.insp
			resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
				WorkDir:             workDir,
				PanePid:             4242,
				AllowLegacyBackfill: true,
				ChatCreatedAt:       timestamppb.New(createdAt),
			})
			if err != nil {
				t.Fatalf("ResolveInteractiveSessionID: %v", err)
			}
			if !resp.Found {
				t.Fatalf("Found=false Reason=%q — a recovery caller must fall through to the time-window scan (BOS-1298)", resp.Reason)
			}
			if resp.SessionId != uuidA {
				t.Errorf("SessionId = %q, want %q via the time-window scan", resp.SessionId, uuidA)
			}
			if resp.TranscriptPath != path {
				t.Errorf("TranscriptPath = %q, want %q", resp.TranscriptPath, path)
			}
		})
	}
}

// TestResolveInteractiveSessionIDRPCLaunchDoesNotFallThroughOnFDMiss is the
// BOS-290 guard on the other side of the same branch: a LAUNCH poll with the
// identical fd miss must still stop at the reason ladder rather than accept the
// racy time-window scan, which could bind a sibling chat's rollout. It must stay
// red if the recovery fall-through is ever widened to the launch path.
func TestResolveInteractiveSessionIDRPCLaunchDoesNotFallThroughOnFDMiss(t *testing.T) {
	cases := []struct {
		name       string
		insp       fakeProcessInspector
		wantReason string
	}{
		{
			name: "probe failed",
			insp: fakeProcessInspector{
				descendants:  map[int][]int{4242: {4242}},
				openFilesErr: map[int]error{4242: errProbeFailed},
			},
			wantReason: reasonPaneProbeFailed,
		},
		{
			name: "no rollout fd open",
			insp: fakeProcessInspector{
				descendants: map[int][]int{4242: {4242, 5252}},
				openFiles:   map[int][]string{5252: {}},
			},
			wantReason: reasonPaneRolloutFDNotOpenYet,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpHome := t.TempDir()
			t.Setenv("HOME", tmpHome)
			t.Setenv("CODEX_HOME", "")
			workDir := t.TempDir()
			root := filepath.Join(tmpHome, codexSessionsDir)

			// A sibling's rollout sits squarely inside this chat's window; the
			// scan would bind it if the launch path ever fell through.
			launchedAfter := time.Now().Add(-2 * time.Second)
			_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", launchedAfter.Add(time.Second))

			s := newTestServer(t)
			s.inspector = tc.insp
			resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
				WorkDir:       workDir,
				PanePid:       4242,
				LaunchedAfter: timestamppb.New(launchedAfter),
			})
			if err != nil {
				t.Fatalf("ResolveInteractiveSessionID: %v", err)
			}
			if resp.Found {
				t.Fatalf("Found=true SessionId=%q — a launch poll must not fall through to the time-window scan (BOS-290)", resp.SessionId)
			}
			if resp.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", resp.Reason, tc.wantReason)
			}
		})
	}
}

// TestResolveInteractiveSessionIDRPCRecoveryStillPrefersFD pins that the
// fall-through above did not demote fd resolution for recovery callers: when the
// pane's codex DOES hold its rollout open, the recovery call binds through the
// fd, not through the scan — even with an ambiguous sibling in the window that
// the scan could only report as ambiguous.
func TestResolveInteractiveSessionIDRPCRecoveryStillPrefersFD(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()
	root := filepath.Join(tmpHome, codexSessionsDir)

	createdAt := time.Now().Add(-time.Minute)
	own := writeSessionMetaRollout(t, root, uuidA, workDir, "codex-tui", createdAt.Add(time.Second))
	_ = writeSessionMetaRollout(t, root, uuidB, workDir, "codex-tui", createdAt.Add(2*time.Second))

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants: map[int][]int{4242: {4242, 5252}},
		openFiles:   map[int][]string{5252: {own}},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:             workDir,
		PanePid:             4242,
		AllowLegacyBackfill: true,
		ChatCreatedAt:       timestamppb.New(createdAt),
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if !resp.Found || resp.SessionId != uuidA {
		t.Fatalf("Found=%v SessionId=%q, want true/%q bound via the pane's own fd", resp.Found, resp.SessionId, uuidA)
	}
	if resp.Ambiguous {
		t.Error("Ambiguous = true, want false: fd resolution is unambiguous even where the scan is not")
	}
}

// TestResolveInteractiveSessionIDRPCRecoveryMissNamesItself pins that a recovery
// call whose fall-through scan ALSO misses still reports a non-empty Reason. An
// unexplained miss is invisible in the daemon's discovery warn line (BOS-1144).
func TestResolveInteractiveSessionIDRPCRecoveryMissNamesItself(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("CODEX_HOME", "")
	workDir := t.TempDir()

	s := newTestServer(t)
	s.inspector = fakeProcessInspector{
		descendants:  map[int][]int{4242: {4242}},
		openFilesErr: map[int]error{4242: errProbeFailed},
	}
	resp, err := s.ResolveInteractiveSessionID(context.Background(), &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:             workDir,
		PanePid:             4242,
		AllowLegacyBackfill: true,
		ChatCreatedAt:       timestamppb.New(time.Now().Add(-time.Minute)),
	})
	if err != nil {
		t.Fatalf("ResolveInteractiveSessionID: %v", err)
	}
	if resp.Found {
		t.Fatalf("Found = true, want false: nothing to bind in this worktree")
	}
	if resp.Reason == "" {
		t.Fatal("Reason is empty for a recovery fall-through miss")
	}
}
