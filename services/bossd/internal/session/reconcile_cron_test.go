package session

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/rs/zerolog"
)

func TestAgentLogIdleFor(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0).UTC()

	// Missing log -> unknown.
	if _, known := agentLogIdleFor(dir, "missing", now); known {
		t.Fatal("missing log: want known=false")
	}
	// Empty id / empty dir -> unknown.
	if _, known := agentLogIdleFor(dir, "", now); known {
		t.Fatal("empty id: want known=false")
	}
	if _, known := agentLogIdleFor("", "x", now); known {
		t.Fatal("empty dir: want known=false")
	}
	// Existing log with mtime 30m ago -> idle≈30m, known.
	logPath := filepath.Join(dir, "sess-1.log")
	if err := os.WriteFile(logPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-30 * time.Minute)
	if err := os.Chtimes(logPath, old, old); err != nil {
		t.Fatal(err)
	}
	idle, known := agentLogIdleFor(dir, "sess-1", now)
	if !known {
		t.Fatal("want known=true")
	}
	if idle < 29*time.Minute || idle > 31*time.Minute {
		t.Fatalf("idle=%s, want ~30m", idle)
	}
}

// seedLog writes an agent log for agentSessionID with mtime `idle` in the past.
func seedLog(t *testing.T, dir, agentSessionID string, idle time.Duration) {
	t.Helper()
	p := agentLogPathFor(dir, agentSessionID)
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-idle)
	if err := os.Chtimes(p, ts, ts); err != nil {
		t.Fatal(err)
	}
}

func TestCronActivityChecker_RunActive(t *testing.T) {
	dir := t.TempDir()
	c := NewCronActivityChecker(dir, nil)

	// No agent id -> not active (don't block the next fire).
	if c.RunActive(&models.Session{}) {
		t.Fatal("no agent id: want not active")
	}
	// Fresh log -> active.
	seedLog(t, dir, "a1", time.Minute)
	if !c.RunActive(&models.Session{State: machine.ImplementingPlan, AgentSessionID: ptr("a1")}) {
		t.Fatal("fresh log: want active")
	}
	// Idle log -> not active.
	seedLog(t, dir, "a2", cronAgentIdleThreshold+time.Minute)
	if c.RunActive(&models.Session{State: machine.ImplementingPlan, AgentSessionID: ptr("a2")}) {
		t.Fatal("idle log: want not active")
	}
	// Fresh log on a completed session -> not active (agent work is over).
	seedLog(t, dir, "a3", time.Minute)
	if c.RunActive(&models.Session{State: machine.ReadyForReview, AgentSessionID: ptr("a3")}) {
		t.Fatal("fresh log, completed session: want not active")
	}
	// Missing log, no liveness checker -> fail open (not active).
	if c.RunActive(&models.Session{AgentSessionID: ptr("nolog")}) {
		t.Fatal("missing log, nil liveness: want not active")
	}
}

// fakeSessionLiveness reports the session alive iff the boss session id is in
// `running`, parked iff it is in `parked`, and dead otherwise.
type fakeSessionLiveness struct {
	running map[string]bool
	parked  map[string]bool
}

func (f fakeSessionLiveness) SessionLiveness(_ context.Context, sessionID string) Liveness {
	switch {
	case f.running[sessionID]:
		return LivenessAlive
	case f.parked[sessionID]:
		return LivenessParked
	default:
		return LivenessDead
	}
}

func TestCronActivityChecker_RunActive_MissingLogFallsBackToSessionLiveness(t *testing.T) {
	dir := t.TempDir()
	c := NewCronActivityChecker(dir, fakeSessionLiveness{running: map[string]bool{"sess-live": true}})
	recent := time.Now().Add(-time.Minute)
	stale := time.Now().Add(-preAgentReapGrace - time.Minute)

	// A pre-agent bootstrap has no AgentSessionID yet, and process liveness can
	// be unavailable before the pane exists. Live or recent rows still suppress
	// overlap conservatively.
	if !c.RunActive(&models.Session{ID: "sess-live", State: machine.CreatingWorktree, AgentName: "claude", CreatedAt: stale}) {
		t.Fatal("pre-agent session: want active")
	}
	if !c.RunActive(&models.Session{ID: "sess-recent", State: machine.StartingAgent, AgentName: "claude", CreatedAt: recent}) {
		t.Fatal("recent pre-agent session with dead liveness: want active")
	}
	if c.RunActive(&models.Session{ID: "sess-stale", State: machine.StartingAgent, AgentName: "claude", CreatedAt: stale}) {
		t.Fatal("stale pre-agent session with dead liveness: want not active")
	}

	// Missing log + session still live through chat/tmux liveness (e.g.
	// pipe-pane failed at start) -> active, so overlap suppression stays in
	// effect (fail closed).
	if !c.RunActive(&models.Session{ID: "sess-live", State: machine.ImplementingPlan, AgentSessionID: ptr("agent-live"), AgentName: "claude"}) {
		t.Fatal("missing log, session live: want active")
	}
	// Missing log + agent gone -> not active, so the next fire proceeds.
	if c.RunActive(&models.Session{ID: "sess-dead", AgentSessionID: ptr("agent-dead"), AgentName: "claude"}) {
		t.Fatal("missing log, session dead: want not active")
	}
	// Missing log + completed session -> not active even if generic task
	// liveness reports true for post-agent states awaiting PR/check events.
	if c.RunActive(&models.Session{ID: "sess-live", State: machine.ReadyForReview, AgentSessionID: ptr("agent-live"), AgentName: "claude"}) {
		t.Fatal("missing log, completed session: want not active")
	}
	// A fresh log still wins over liveness (log mtime drives the decision).
	seedLog(t, dir, "logged", time.Minute)
	if !c.RunActive(&models.Session{ID: "sess-logged", State: machine.ImplementingPlan, AgentSessionID: ptr("logged"), AgentName: "claude"}) {
		t.Fatal("fresh log: want active")
	}
}

// recordingReapFinalizer captures the (sessionID, expectedStates) each reap
// routes through the broadened finalize entry, standing in for the real
// finalizeSessionFrom so sweep-selection tests don't wire the full finalize
// pipeline. It always reports a PR-created success.
type recordingReapFinalizer struct {
	mu    sync.Mutex
	calls []reapFinalizeCall
}

type reapFinalizeCall struct {
	sessionID      string
	expectedStates []int
}

func (r *recordingReapFinalizer) finalize(_ context.Context, sessionID string, expectedStates []int) (*FinalizeResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, reapFinalizeCall{sessionID: sessionID, expectedStates: expectedStates})
	return &FinalizeResult{Outcome: models.CronJobOutcomePRCreated}, nil
}

func (r *recordingReapFinalizer) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingReapFinalizer) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.calls))
	for i, c := range r.calls {
		out[i] = c.sessionID
	}
	return out
}

func newSweepLifecycle(t *testing.T, logsDir string) (*Lifecycle, *mockSessionStore, *mockAgentRunner, *recordingReapFinalizer) {
	t.Helper()
	sessions := newMockSessionStore()
	runner := newMockAgentRunner()
	lc := newTestLifecycle(sessions, nil, &mockAgentChatStore{}, nil, nil, runner, nil, nil, zerolog.Nop())
	lc.SetAgentLogsDir(logsDir)
	// A wired notifier keeps the partial-wiring early-return satisfied; the
	// sweep now routes reaps through reapFinalizer, not the notifier.
	lc.SetCronCompletionNotifier(&recordingCronCompletionNotifier{})
	rec := &recordingReapFinalizer{}
	lc.reapFinalizer = rec.finalize
	return lc, sessions, runner, rec
}

func strandedCronSession(id, agentSessionID string) *models.Session {
	return &models.Session{
		ID:             id,
		State:          machine.ImplementingPlan,
		AgentName:      "claude",
		AgentSessionID: ptr(agentSessionID),
		CronJobID:      ptr("cron-" + id),
		UpdatedAt:      time.Now().Add(-time.Hour),
	}
}

func TestRecoverStrandedCronSessions_IdleRun_Routed(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute)

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("routed=%d, want 1", n)
	}
	if got := rec.ids(); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("reaped=%v, want [s1]", got)
	}
	// The periodic reap passes the periodic (post-agent) set as expectedStates.
	if got := rec.calls[0].expectedStates; !equalIntSet(got, periodicReapStateInts()) {
		t.Fatalf("expectedStates=%v, want %v", got, periodicReapStateInts())
	}
}

// equalIntSet reports set equality (order-independent) for two int slices.
func equalIntSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[int]int, len(a))
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
	}
	for _, c := range m {
		if c != 0 {
			return false
		}
	}
	return true
}

func TestRecoverStrandedCronSessions_RecentlyActive_Skipped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")
	seedLog(t, dir, "a1", time.Minute) // fresh -> still working

	n, _ := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_NoLog_Skipped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	sessions.sessions["s1"] = strandedCronSession("s1", "a1") // no log written
	n, _ := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0 (unknown liveness=alive)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_RunnerRunning_Skipped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, runner, rec := newSweepLifecycle(t, dir)
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute)
	runner.running["a1"] = true // headless run still alive

	n, _ := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d, want 0 (runner running)", n)
	}
}

func TestRecoverStrandedCronSessions_NonCron_Skipped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	s := strandedCronSession("s1", "a1")
	s.CronJobID = nil
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute)

	n, _ := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d, want 0 (non-cron)", n)
	}
}

func TestRecoverStrandedCronSessions_TmuxUnattended_Routed(t *testing.T) {
	// A tmux_unattended session (e.g. /boss-epic) has no CronJobID but is still
	// unattended, so the completion gate defers it to this sweep. It must be
	// recovered on the same terms as a cron session, not skipped forever.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	s := strandedCronSession("s1", "a1")
	s.CronJobID = nil
	s.IsTmuxUnattended = true
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute) // idle -> run over

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("routed=%d, want 1 (tmux_unattended is unattended)", n)
	}
	if got := rec.ids(); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("reaped=%v, want [s1]", got)
	}
}

func TestRecoverStrandedCronSessions_NoLog_LivenessDead_LiveTmuxPane_Skipped(t *testing.T) {
	// A cron run is tmux-hosted even when it is not marked tmux_unattended. A
	// confirmed live pane overrides false aggregate liveness when pipe-pane never
	// wrote a log.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // nothing alive
	lc.tmux = stubTmuxForSweep(true, "")
	tmuxName := "boss-s1-a1"
	lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		"s1": {{SessionID: "s1", AgentSessionID: "a1", TmuxSessionName: &tmuxName}},
	}}
	s := strandedCronSession("s1", "a1")
	s.IsTmuxUnattended = false
	sessions.sessions["s1"] = s // no log written

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0 (live tmux pane defers despite false liveness)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_NoLog_LivenessAlive_Skipped(t *testing.T) {
	// Logless but liveness reports the session still alive (e.g. pipe-pane failed
	// while the agent keeps running) -> conservatively left for the next sweep.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{"s1": true}})
	sessions.sessions["s1"] = strandedCronSession("s1", "a1") // no log written

	n, _ := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0 (session still alive)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_FreshLog_LivenessDead_Skipped(t *testing.T) {
	// A fresh cron log is durable evidence that the tmux-hosted run may still be
	// active. False liveness cannot override it.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // agent gone
	s := strandedCronSession("s1", "a1")
	s.IsTmuxUnattended = false
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", time.Minute) // fresh -> would otherwise look "active"

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0 (fresh cron log defers despite false liveness)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_TmuxHostedUnattendedFalseLiveness_Skipped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*models.Session)
		seed      func(t *testing.T, dir string)
	}{
		{
			name: "tmux_unattended_fresh_log",
			configure: func(s *models.Session) {
				s.CronJobID = nil
				s.IsTmuxUnattended = true
			},
			seed: func(t *testing.T, dir string) {
				seedLog(t, dir, "a1", time.Minute)
			},
		},
		{
			name:      "cron_fresh_log",
			configure: func(*models.Session) {},
			seed: func(t *testing.T, dir string) {
				seedLog(t, dir, "a1", time.Minute)
			},
		},
		{
			name: "detached_fresh_log",
			configure: func(s *models.Session) {
				s.CronJobID = nil
				s.Detach = true
			},
			seed: func(t *testing.T, dir string) {
				seedLog(t, dir, "a1", time.Minute)
			},
		},
		{
			name: "tmux_unattended_unknown_log",
			configure: func(s *models.Session) {
				s.CronJobID = nil
				s.IsTmuxUnattended = true
			},
			seed: func(*testing.T, string) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			lc, sessions, _, rec := newSweepLifecycle(t, dir)
			lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // false liveness
			s := strandedCronSession("s1", "a1")
			tc.configure(s)
			sessions.sessions["s1"] = s
			tc.seed(t, dir)

			n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n != 0 || rec.count() != 0 {
				t.Fatalf("routed=%d notifier=%d, want 0/0 (tmux run lacks durable completion evidence)", n, rec.count())
			}
		})
	}
}

func TestRecoverStrandedCronSessions_TmuxUnattendedWithoutAgentIDFalseLiveness_StartupRouted(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // false liveness
	sessions.sessions["s1"] = &models.Session{
		ID:               "s1",
		State:            machine.CreatingWorktree,
		IsTmuxUnattended: true,
	}

	n, err := lc.RecoverStrandedCronSessionsAtStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || rec.count() != 1 {
		t.Fatalf("routed=%d notifier=%d, want 1/1 (restart-stranded pre-agent run)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_FreshLog_LivenessAlive_Skipped(t *testing.T) {
	// Fresh log + a live (idle-in-tmux) agent -> not over yet; the durable
	// log-idle threshold still guards against reaping a paused-but-live run.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{"s1": true}})
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")
	seedLog(t, dir, "a1", time.Minute) // fresh

	n, _ := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0 (live agent, fresh log)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_NoNotifier_NoOp(t *testing.T) {
	dir := t.TempDir()
	sessions := newMockSessionStore()
	lc := newTestLifecycle(sessions, nil, &mockAgentChatStore{}, nil, nil, newMockAgentRunner(), nil, nil, zerolog.Nop())
	lc.SetAgentLogsDir(dir)
	sessions.sessions["s2"] = strandedCronSession("s2", "a2")
	seedLog(t, dir, "a2", cronAgentIdleThreshold+time.Minute)

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("routed=%d err=%v, want 0/nil", n, err)
	}
}

func TestStrandedRunIsDead(t *testing.T) {
	dir := t.TempDir()

	// Post-agent: defers to cronRunIsOver. Idle log + no liveness => over.
	seedLog(t, dir, "over", cronAgentIdleThreshold+time.Minute)
	seedLog(t, dir, "fresh", time.Minute)

	cases := []struct {
		name     string
		sess     *models.Session
		liveness sessionLiveness
		wantDead bool
	}{
		{
			name:     "post_agent_idle_log_is_dead",
			sess:     &models.Session{ID: "s", State: machine.ImplementingPlan, AgentName: "claude", AgentSessionID: ptr("over")},
			wantDead: true,
		},
		{
			name:     "post_agent_fresh_log_not_dead",
			sess:     &models.Session{ID: "s", State: machine.PushingBranch, AgentName: "claude", AgentSessionID: ptr("fresh")},
			wantDead: false,
		},
		{
			name:     "post_agent_fresh_log_liveness_dead_is_dead",
			sess:     &models.Session{ID: "s", State: machine.OpeningDraftPR, AgentName: "claude", AgentSessionID: ptr("fresh")},
			liveness: fakeSessionLiveness{running: map[string]bool{}},
			wantDead: true,
		},
		{
			name:     "pre_agent_nil_id_liveness_dead_is_dead",
			sess:     &models.Session{ID: "s", State: machine.CreatingWorktree, AgentName: "claude"},
			liveness: fakeSessionLiveness{running: map[string]bool{}},
			wantDead: true,
		},
		{
			name:     "pre_agent_nil_id_liveness_alive_not_dead",
			sess:     &models.Session{ID: "s", State: machine.StartingAgent, AgentName: "claude"},
			liveness: fakeSessionLiveness{running: map[string]bool{"s": true}},
			wantDead: false,
		},
		{
			name:     "pre_agent_nil_id_liveness_unwired_not_dead",
			sess:     &models.Session{ID: "s", State: machine.CreatingWorktree, AgentName: "claude"},
			liveness: nil,
			wantDead: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lc := newTestLifecycle(newMockSessionStore(), nil, &mockAgentChatStore{}, nil, nil, newMockAgentRunner(), nil, nil, zerolog.Nop())
			lc.SetAgentLogsDir(dir)
			if tc.liveness != nil {
				lc.SetSessionLiveness(tc.liveness)
			}
			if got := lc.strandedRunIsDead(tc.sess); got != tc.wantDead {
				t.Fatalf("strandedRunIsDead=%v, want %v", got, tc.wantDead)
			}
		})
	}
}

func TestRecoverStrandedCronSessions_PostAgentPushingBranch_Routed(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	s := strandedCronSession("s1", "a1")
	s.State = machine.PushingBranch
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute) // idle -> run over

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("routed=%d, want 1", n)
	}
	if got := rec.ids(); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("reaped=%v, want [s1]", got)
	}
}

func TestRecoverStrandedCronSessions_PreAgentCreatingWorktree_Startup_Routed(t *testing.T) {
	// A restart can strand a cron session after persisting CronJobID and
	// CreatingWorktree, but before creating a pane or agent ID. There is no
	// in-flight creator after restart, so dead liveness recovers this row.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // nothing alive
	s := &models.Session{
		ID:        "s1",
		State:     machine.CreatingWorktree,
		AgentName: "claude",
		CronJobID: ptr("cron-s1"), // no AgentSessionID (pre-agent)
		UpdatedAt: time.Now().Add(-time.Hour),
	}
	sessions.sessions["s1"] = s

	n, err := lc.RecoverStrandedCronSessionsAtStartup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || rec.count() != 1 {
		t.Fatalf("routed=%d notifier=%d, want 1/1 (restart-stranded pre-agent cron run)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_LoglessTmuxExited_Routed(t *testing.T) {
	// pipe-pane is best effort. When it never creates a log, a confirmed absent
	// tmux session is independent completion evidence and must eventually reap
	// the stranded run; false liveness alone remains insufficient.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}})
	lc.tmux = stubTmuxForSweep(false, "")
	tmuxName := "boss-s1-a1"
	lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		"s1": {{SessionID: "s1", AgentSessionID: "a1", TmuxSessionName: &tmuxName}},
	}}
	sessions.sessions["s1"] = strandedCronSession("s1", "a1") // no log written

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || rec.count() != 1 {
		t.Fatalf("routed=%d notifier=%d, want 1/1 (confirmed missing tmux pane)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_LoglessTmuxReplacementChatExited_Routed(t *testing.T) {
	// A non-resumable account switch clears the original chat's tmux name and
	// starts a replacement under a fresh agent-session ID without changing the
	// parent session's persisted ID. The replacement pane is still the durable
	// completion evidence for this tmux-hosted unattended run.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}})
	lc.tmux = stubTmuxForSweep(false, "")
	replacementTmuxName := "boss-s1-a2"
	lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{
		"s1": {
			{SessionID: "s1", AgentSessionID: "a1"}, // switched-from chat; pane name cleared
			{SessionID: "s1", AgentSessionID: "a2", TmuxSessionName: &replacementTmuxName},
		},
	}}
	sessions.sessions["s1"] = strandedCronSession("s1", "a1") // no log written

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || rec.count() != 1 {
		t.Fatalf("routed=%d notifier=%d, want 1/1 (confirmed missing replacement tmux pane)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_PreAgentCreatingWorktree_Periodic_NotRouted(t *testing.T) {
	// The SAME pre-agent CreatingWorktree strand (nil AgentSessionID, liveness
	// dead) must NOT be reaped by the PERIODIC sweep: a running daemon owns that
	// state through the live create path, so reaping it would delete the row out
	// from under an in-flight `start session`. This is the primary BOS-426
	// regression guard.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // nothing alive
	s := &models.Session{
		ID:        "s1",
		State:     machine.CreatingWorktree,
		AgentName: "claude",
		CronJobID: ptr("cron-s1"), // no AgentSessionID (pre-agent, mid-creation)
		UpdatedAt: time.Now().Add(-time.Hour),
		CreatedAt: time.Now().Add(-time.Hour),
	}
	sessions.sessions["s1"] = s

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d reaped=%d, want 0/0 (periodic must not reap pre-agent)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_PreAgentStartingAgent_Periodic_NotRouted(t *testing.T) {
	// StartingAgent is the other pre-agent state; the periodic sweep must leave
	// it alone too (BOS-426).
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // nothing alive
	s := &models.Session{
		ID:        "s1",
		State:     machine.StartingAgent,
		AgentName: "claude",
		CronJobID: ptr("cron-s1"), // no AgentSessionID (pre-agent, mid-creation)
		UpdatedAt: time.Now().Add(-time.Hour),
		CreatedAt: time.Now().Add(-time.Hour),
	}
	sessions.sessions["s1"] = s

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d reaped=%d, want 0/0 (periodic must not reap pre-agent)", n, rec.count())
	}
}

func TestPreAgentReapAllowed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()

	// Pre-agent state younger than the grace window -> deny (protect a live
	// creation).
	if preAgentReapAllowed(machine.CreatingWorktree, now.Add(-time.Minute), now) {
		t.Fatal("young pre-agent state: want not allowed")
	}
	// Pre-agent state older than the grace window -> allow (restart-frozen).
	if !preAgentReapAllowed(machine.CreatingWorktree, now.Add(-10*time.Minute), now) {
		t.Fatal("old pre-agent state: want allowed")
	}
	// Post-agent state -> allowed regardless of age (age 0 here).
	if !preAgentReapAllowed(machine.ImplementingPlan, now, now) {
		t.Fatal("post-agent state: want allowed regardless of age")
	}
}

func TestRecoverStrandedCronSessions_Orphaned_NeverReaped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // agent gone
	s := strandedCronSession("s1", "a1")
	s.State = machine.Orphaned // terminal; must never be reaped
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute)

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d reaped=%d, want 0/0 (Orphaned excluded)", n, rec.count())
	}
	for _, c := range rec.calls {
		if c.sessionID == "s1" {
			t.Fatal("Orphaned session must never be finalized")
		}
	}
}

func TestRecoverStrandedCronSessions_LiveAndAttended_Untouched(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	// A live cron run (fresh log, liveness alive).
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{"live": true}})
	live := strandedCronSession("live", "a-live")
	live.State = machine.PushingBranch
	sessions.sessions["live"] = live
	seedLog(t, dir, "a-live", time.Minute)
	// An attended (non-unattended) session that is idle/dead — must be skipped.
	attended := strandedCronSession("attended", "a-att")
	attended.State = machine.PushingBranch
	attended.CronJobID = nil
	attended.IsTmuxUnattended = false
	sessions.sessions["attended"] = attended
	seedLog(t, dir, "a-att", cronAgentIdleThreshold+time.Minute)

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d reaped=%d, want 0/0 (live + attended both skipped)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_Archived_Skipped(t *testing.T) {
	// An archived session's worktree was already removed by ArchiveSession,
	// which leaves the row in an implementing state. ListByStates returns
	// archived rows regardless of archived status, so finalizing one here would
	// run `git status` against a gone path and surface the exact spurious
	// pr_failed BOS-384 kills. The reaper must skip archived sessions entirely.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	s := strandedCronSession("s1", "a1")
	archivedAt := time.Now().Add(-time.Minute)
	s.ArchivedAt = &archivedAt
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute) // idle -> would otherwise route

	n, err := lc.RecoverStrandedCronSessionsPeriodic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d reaped=%d, want 0/0 (archived session skipped)", n, rec.count())
	}
	for _, c := range rec.calls {
		if c.sessionID == "s1" {
			t.Fatal("archived session must never be finalized")
		}
	}
}
