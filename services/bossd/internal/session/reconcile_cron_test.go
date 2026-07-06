package session

import (
	"context"
	"os"
	"path/filepath"
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

// fakeSessionLiveness reports the session running iff the boss session id is in `running`.
type fakeSessionLiveness struct{ running map[string]bool }

func (f fakeSessionLiveness) IsSessionAlive(_ context.Context, sessionID string) bool {
	return f.running[sessionID]
}

func TestCronActivityChecker_RunActive_MissingLogFallsBackToSessionLiveness(t *testing.T) {
	dir := t.TempDir()
	c := NewCronActivityChecker(dir, fakeSessionLiveness{running: map[string]bool{"sess-live": true}})

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

func newSweepLifecycle(t *testing.T, logsDir string) (*Lifecycle, *mockSessionStore, *mockAgentRunner, *recordingCronCompletionNotifier) {
	t.Helper()
	sessions := newMockSessionStore()
	runner := newMockAgentRunner()
	lc := newTestLifecycle(sessions, nil, &mockAgentChatStore{}, nil, nil, runner, nil, nil, zerolog.Nop())
	lc.SetAgentLogsDir(logsDir)
	rec := &recordingCronCompletionNotifier{}
	lc.SetCronCompletionNotifier(rec)
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

	n, err := lc.RecoverStrandedCronSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("routed=%d, want 1", n)
	}
	if got := rec.callsCopy(); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("notifier=%v, want [s1]", got)
	}
}

func TestRecoverStrandedCronSessions_RecentlyActive_Skipped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")
	seedLog(t, dir, "a1", time.Minute) // fresh -> still working

	n, _ := lc.RecoverStrandedCronSessions(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_NoLog_Skipped(t *testing.T) {
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	sessions.sessions["s1"] = strandedCronSession("s1", "a1") // no log written
	n, _ := lc.RecoverStrandedCronSessions(context.Background())
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

	n, _ := lc.RecoverStrandedCronSessions(context.Background())
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

	n, _ := lc.RecoverStrandedCronSessions(context.Background())
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
	s.TmuxUnattended = true
	sessions.sessions["s1"] = s
	seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute) // idle -> run over

	n, err := lc.RecoverStrandedCronSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("routed=%d, want 1 (tmux_unattended is unattended)", n)
	}
	if got := rec.callsCopy(); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("notifier=%v, want [s1]", got)
	}
}

func TestRecoverStrandedCronSessions_NoLog_LivenessDead_Routed(t *testing.T) {
	// A logless cron run (e.g. Codex, or a failed tmux pipe-pane) has no durable
	// idle evidence, but a wired liveness checker confirms the agent is gone — so
	// the sweep reaps it instead of leaving it stranded forever.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // nothing alive
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")              // no log written

	n, err := lc.RecoverStrandedCronSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("routed=%d, want 1", n)
	}
	if got := rec.callsCopy(); len(got) != 1 || got[0] != "s1" {
		t.Fatalf("notifier=%v, want [s1]", got)
	}
}

func TestRecoverStrandedCronSessions_NoLog_LivenessAlive_Skipped(t *testing.T) {
	// Logless but liveness reports the session still alive (e.g. pipe-pane failed
	// while the agent keeps running) -> conservatively left for the next sweep.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{"s1": true}})
	sessions.sessions["s1"] = strandedCronSession("s1", "a1") // no log written

	n, _ := lc.RecoverStrandedCronSessions(context.Background())
	if n != 0 || rec.count() != 0 {
		t.Fatalf("routed=%d notifier=%d, want 0/0 (session still alive)", n, rec.count())
	}
}

func TestRecoverStrandedCronSessions_FreshLog_LivenessDead_Routed(t *testing.T) {
	// Fresh log mtime but the agent is confirmed gone (it died moments before a
	// daemon restart) -> reaped immediately rather than waiting out the idle
	// threshold. This is the restart fast-path.
	dir := t.TempDir()
	lc, sessions, _, rec := newSweepLifecycle(t, dir)
	lc.SetSessionLiveness(fakeSessionLiveness{running: map[string]bool{}}) // agent gone
	sessions.sessions["s1"] = strandedCronSession("s1", "a1")
	seedLog(t, dir, "a1", time.Minute) // fresh -> would otherwise look "active"

	n, err := lc.RecoverStrandedCronSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || rec.count() != 1 {
		t.Fatalf("routed=%d notifier=%d, want 1/1 (agent dead reaps despite fresh log)", n, rec.count())
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

	n, _ := lc.RecoverStrandedCronSessions(context.Background())
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

	n, err := lc.RecoverStrandedCronSessions(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("routed=%d err=%v, want 0/nil", n, err)
	}
}
