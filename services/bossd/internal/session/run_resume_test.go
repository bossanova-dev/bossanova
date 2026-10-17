package session

import (
	"context"
	"database/sql"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/tmux"
)

func TestRunResumeCompletionGatePreservesLiveNonBuildCompletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dead     bool
		over     bool
		finalize bool
	}{
		{name: "live idle non-build prompt", over: true, finalize: true},
		{name: "live busy prompt"},
		{name: "dead dropped prompt", dead: true, over: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t)
			row := f.sessions.sessions[f.sessionID]
			row.IsTmuxUnattended = true
			pane := "unattended-pane"
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{AgentSessionID: "agent-old", AgentName: "claude", TmuxSessionName: &pane}}}}
			f.lc.SetRunResumeStore(&runResumeLedger{})
			// Non-build prompts have neither a boss/build receipt nor terminal telemetry.
			f.lc.agentRuns = &runResumeTelemetry{runs: []db.AgentRun{{AgentSessionID: "agent-old"}}}
			f.lc.tmux = tmux.NewClient(tmux.WithCommandFactory(func(ctx context.Context, _ string, args ...string) *exec.Cmd {
				if args[0] == "display-message" && tc.dead {
					return exec.CommandContext(ctx, "printf", "1")
				}
				return exec.CommandContext(ctx, "true")
			}))
			gate := NewCronCompletionGate(CronCompletionGateDeps{
				Sessions: f.sessions, Finalizer: &recordingCronFinalizer{},
				RunCompletionEvidence: func(*models.Session) (bool, string) { return tc.over, "agent idle evidence" },
				DeferDroppedRun:       f.lc.DeferDroppedRunFinalization,
			})
			got := gate.checkAndFinalize(context.Background(), f.sessionID)
			if finalize := got == cronCompletionGateCheckFinalize; finalize != tc.finalize {
				t.Fatalf("finalize=%v want %v", finalize, tc.finalize)
			}
		})
	}
}

type runResumeLedger struct {
	db.ResumeAttemptStore
	rows   []*models.ResumeAttempt
	onList func()
}

func (s *runResumeLedger) ListForRun(_ context.Context, _, key string) ([]*models.ResumeAttempt, error) {
	if s.onList != nil {
		s.onList()
	}
	var out []*models.ResumeAttempt
	for _, a := range s.rows {
		if a.RunKey == key {
			out = append(out, a)
		}
	}
	return out, nil
}
func (s *runResumeLedger) SetFailure(_ context.Context, id, class string) error {
	for _, a := range s.rows {
		if a.ID == id {
			a.FailureClass = class
		}
	}
	return nil
}
func (s *runResumeLedger) Record(_ context.Context, p db.RecordResumeAttemptParams) (*models.ResumeAttempt, error) {
	a := &models.ResumeAttempt{SessionID: p.SessionID, RunKey: p.RunKey, AttemptedAt: p.AttemptedAt, FailureClass: p.FailureClass, AttemptNumber: len(s.rows) + 1}
	s.rows = append(s.rows, a)
	return a, nil
}
func (s *runResumeLedger) DeleteForRun(context.Context, string, string) (int64, error) {
	n := len(s.rows)
	s.rows = nil
	return int64(n), nil
}

func TestRunResumeSchedule(t *testing.T) {
	f := newRotationFixture(t)
	ctx := context.Background()
	death := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := death
	row := f.sessions.sessions[f.sessionID]
	row.IsTmuxUnattended = true
	row.UpdatedAt = death.Add(-time.Hour)
	f.lc.sessions = &runResumeObservedStore{mockSessionStore: f.sessions, observations: map[string]time.Time{}}
	f.lc.agentChats = &runResumeChats{mockAgentChatStore: &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old", Title: "Run build", CreatedAt: death}}}}, now: func() time.Time { return now }}
	ledger := &runResumeLedger{}
	f.lc.SetRunResumeStore(ledger)
	f.lc.SetClockForTest(func() time.Time { return now })
	for i, mins := range []int{0, 1, 2, 3, 10, 11, 12} {
		now = death.Add(time.Duration(mins) * time.Minute)
		f.runner.running = map[string]bool{}
		f.runner.nextID = now.Format("1504")
		f.lc.ResumeDroppedRuns(ctx)
		want := []int{0, 1, 1, 2, 2, 3, 3}[i]
		if len(f.runner.started) != want {
			t.Fatalf("+%dm: launches=%d want=%d", mins, len(f.runner.started), want)
		}
	}
	if row.State != machine.Blocked {
		t.Fatalf("state=%v want Blocked", row.State)
	}
	if len(ledger.rows) != 3 {
		t.Fatalf("attempts=%d", len(ledger.rows))
	}
	for _, call := range f.runner.started {
		if !strings.Contains(call.plan, "ORIGINAL PLAN BODY") || call.resume != nil {
			t.Fatalf("fresh resume prompt=%q resume=%v", call.plan, call.resume)
		}
	}
}

func TestRunResumeManualNonBlocked(t *testing.T) {
	for _, state := range []machine.State{machine.Blocked, machine.FixingChecks, machine.ImplementingPlan} {
		t.Run(state.String(), func(t *testing.T) {
			f := newRotationFixture(t)
			row := f.sessions.sessions[f.sessionID]
			row.State = state
			ledger := &runResumeLedger{rows: []*models.ResumeAttempt{{RunKey: "agent-old"}}}
			f.lc.SetRunResumeStore(ledger)
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old"}}}}
			if _, err := f.lc.ResumeSessionRun(context.Background(), f.sessionID); err != nil {
				t.Fatal(err)
			}
			if len(f.runner.started) != 1 || len(ledger.rows) != 0 {
				t.Fatalf("launches=%d ledger=%d", len(f.runner.started), len(ledger.rows))
			}
		})
	}
}

type runResumeTelemetry struct {
	db.AgentRunStore
	runs []db.AgentRun
}

func (s *runResumeTelemetry) List(context.Context, db.AgentRunFilter) ([]db.AgentRun, error) {
	return s.runs, nil
}

type runResumeProvider struct{ *mockVCSProvider }

func (p *runResumeProvider) GetCheckSet(context.Context, string, int) (vcs.CheckSet, error) {
	return vcs.CheckSet{HasBuildReceipt: true}, nil
}

func TestRunResumeExclusions(t *testing.T) {
	for _, kind := range []string{"archived", "interactive", "finished", "receipt", "disabled", "human-active", "parked"} {
		t.Run(kind, func(t *testing.T) {
			f := newRotationFixture(t)
			row := f.sessions.sessions[f.sessionID]
			row.IsTmuxUnattended = true
			row.UpdatedAt = time.Now().Add(-time.Hour)
			chats := &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old"}}}}
			f.lc.agentChats = chats
			f.lc.SetRunResumeStore(&runResumeLedger{})
			switch kind {
			case "parked":
				f.lc.SetSessionLiveness(fakeSessionLiveness{parked: map[string]bool{f.sessionID: true}})
			case "archived":
				at := time.Now()
				row.ArchivedAt = &at
			case "interactive":
				row.IsTmuxUnattended = false
			case "finished":
				f.lc.agentRuns = &runResumeTelemetry{runs: []db.AgentRun{{AgentSessionID: "agent-old", TerminalState: db.AgentRunTerminalReviewReady}}}
			case "receipt":
				n := 1
				row.PRNumber = &n
				f.lc.provider = &runResumeProvider{mockVCSProvider: &mockVCSProvider{}}
			case "disabled":
				off := false
				f.lc.SetRotationConfig(config.ManagedAccountsConfig{AutoResumeDroppedRuns: &off})
			case "human-active":
				chats.seedChat(f.sessionID, &models.AgentChat{ID: "human", SessionID: f.sessionID, AgentSessionID: "human", AgentName: "claude"})
				f.runner.running["human"] = true
			}
			if n := f.lc.ResumeDroppedRuns(context.Background()); n != 0 || len(f.runner.started) != 0 {
				t.Fatalf("resumed=%d launches=%d", n, len(f.runner.started))
			}
		})
	}
}

// TestRunResumeManualRefusesARevivalOntoATakenTarget pins the human-initiated
// half of note 1 of the #3192 verifier: resuming a Blocked or Orphaned run
// whose PR or branch another live session now holds is refused, naming that
// session, before anything is stopped or spawned.
func TestRunResumeManualRefusesARevivalOntoATakenTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state machine.State
		rival models.Session
		want  string
	}{
		{
			name:  "blocked, rival on the PR",
			state: machine.Blocked,
			rival: models.Session{State: machine.AwaitingChecks, PRNumber: intPtr(42), BranchName: "other/branch"},
			want:  `PR #42 is now held by session rival ("Rival"); archive that session before resuming this one`,
		},
		{
			name:  "orphaned, rival on the branch",
			state: machine.Orphaned,
			rival: models.Session{State: machine.ImplementingPlan, BranchName: "fix/pr-head"},
			want:  `branch "fix/pr-head" is now held by session rival ("Rival"); archive that session before resuming this one`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRotationFixture(t)
			row := f.sessions.sessions[f.sessionID]
			row.State, row.PRNumber, row.BranchName = tc.state, intPtr(42), "fix/pr-head"
			f.lc.SetRunResumeStore(&runResumeLedger{})
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old"}}}}
			rival := tc.rival
			rival.ID, rival.RepoID, rival.Title = "rival", row.RepoID, "Rival"
			f.sessions.sessions["rival"] = &rival

			_, err := f.lc.ResumeSessionRun(context.Background(), f.sessionID)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("ResumeSessionRun error = %v, want %q", err, tc.want)
			}
			if !IsDuplicateActiveSessionError(err) {
				t.Fatalf("error %T does not unwrap to a duplicate-session error", err)
			}
			if len(f.runner.started) != 0 || len(f.runner.stopped) != 0 {
				t.Fatalf("refused resume started %d and stopped %d runs, want none", len(f.runner.started), len(f.runner.stopped))
			}
			if row.State != tc.state {
				t.Fatalf("state = %v, want %v kept", row.State, tc.state)
			}
		})
	}
}

func TestRunResumeManualRefusesActiveHuman(t *testing.T) {
	f := newRotationFixture(t)
	f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{AgentSessionID: "agent-old"}, {AgentSessionID: "human", AgentName: "claude"}}}}
	f.runner.running["human"] = true
	if _, err := f.lc.ResumeSessionRun(context.Background(), f.sessionID); err == nil {
		t.Fatal("resumed beside active human chat")
	}
}

func TestRunResumeEarlyStopAndRestart(t *testing.T) {
	for _, classes := range [][]string{{"auth_expired", "auth_expired"}, {"crash", "auth_expired"}} {
		t.Run(strings.Join(classes, "-"), func(t *testing.T) {
			f := newRotationFixture(t)
			ctx := context.Background()
			row := f.sessions.sessions[f.sessionID]
			row.IsTmuxUnattended = true
			death := time.Now().Add(-time.Hour)
			row.UpdatedAt = death
			reason := "API Error: 401"
			row.BlockedReason = &reason
			ledger := &runResumeLedger{rows: []*models.ResumeAttempt{{ID: "first", RunKey: "agent-old", AttemptedAt: death, FailureClass: classes[0]}, {ID: "second", RunKey: "agent-old", AttemptedAt: death.Add(2 * time.Minute), FailureClass: classes[1]}}}
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old"}}}}
			f.lc.SetRunResumeStore(ledger)
			// New Lifecycle, same persisted stores: the due attempt survives restart.
			restarted := &Lifecycle{sessions: f.lc.sessions, repos: f.lc.repos, agentChats: f.lc.agentChats, agentRunner: f.lc.agentRunner, logger: f.lc.logger}
			restarted.SetRunResumeStore(ledger)
			n := restarted.ResumeDroppedRuns(ctx)
			if classes[0] == classes[1] {
				if n != 0 || row.State != machine.Blocked {
					t.Fatalf("resumed=%d state=%v", n, row.State)
				}
			} else if n != 1 {
				t.Fatalf("resumed=%d want1", n)
			}
		})
	}
}

type runResumeChats struct {
	*mockAgentChatStore
	now func() time.Time
}

func (s *runResumeChats) Create(ctx context.Context, p db.CreateAgentChatParams) (*models.AgentChat, error) {
	c, err := s.mockAgentChatStore.Create(ctx, p)
	if err == nil {
		c.CreatedAt = s.now()
		c.AgentName = p.AgentName
		c.AccountID = p.AccountID
		c.Model = p.Model
		s.seedChat(p.SessionID, c)
	}
	return c, err
}

func (s *mockSessionStore) CommitRunResume(ctx context.Context, id string, expected int, prior *string, next string) (*models.Session, error) {
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if current.ArchivedAt != nil || int(current.State) != expected || current.AgentSessionID == nil || prior == nil || *current.AgentSessionID != *prior {
		return nil, sql.ErrNoRows
	}
	working := int(machine.ImplementingPlan)
	var clear *string
	enabled := true
	return s.Update(ctx, id, db.UpdateSessionParams{State: &working, AgentSessionID: stringPtrPtr(next), BlockedReason: &clear, IsAutomationEnabled: &enabled})
}

func (s *mockSessionStore) ObserveRunDeath(ctx context.Context, id, _ string, _ int, at time.Time) (time.Time, error) {
	row, err := s.Get(ctx, id)
	if err != nil {
		return time.Time{}, err
	}
	if !row.UpdatedAt.IsZero() {
		return row.UpdatedAt, nil
	}
	return at, nil
}

type runResumeObservedStore struct {
	*mockSessionStore
	observations map[string]time.Time
}

func (s *runResumeObservedStore) ObserveRunDeath(_ context.Context, id, agentID string, _ int, at time.Time) (time.Time, error) {
	key := id + ":" + agentID
	if first, ok := s.observations[key]; ok {
		return first, nil
	}
	s.observations[key] = at
	return at, nil
}

func TestRunResumeFailedTmuxLaunchKeepsRetrying(t *testing.T) {
	h := newStartTmuxChatHarness(t)
	ctx := context.Background()
	row := h.sessions.sessions["sess-1"]
	row.IsTmuxUnattended = true
	prior := "old-run"
	pane := "old-pane"
	row.AgentSessionID = &prior
	death := time.Date(2026, 10, 15, 14, 0, 0, 0, time.UTC)
	row.UpdatedAt = death
	now := death.Add(time.Minute)
	h.lc.SetClockForTest(func() time.Time { return now })
	old := &models.AgentChat{ID: "original", SessionID: row.ID, AgentSessionID: prior, AgentName: "claude", TmuxSessionName: &pane, CreatedAt: death}
	h.chats.chatsBySession = map[string][]*models.AgentChat{row.ID: {old}}
	h.tmuxFake.failSubcommand["has-session"] = true
	h.tmuxFake.failStderr["has-session"] = "can't find session"
	h.tmuxFake.failSubcommand["new-session"] = true
	ledger := &runResumeLedger{}
	h.lc.SetRunResumeStore(ledger)
	h.lc.ResumeDroppedRuns(ctx)
	now = death.Add(3 * time.Minute)
	h.lc.ResumeDroppedRuns(ctx)
	if len(ledger.rows) != 2 {
		t.Fatalf("launch failures stopped retrying after %d attempts", len(ledger.rows))
	}
	if old.TmuxSessionName == nil {
		t.Fatal("crashed run was relabelled deliberately parked")
	}
}

func (s *mockSessionStore) GiveUpRunResume(ctx context.Context, id string, expected int, prior *string, reason string) error {
	current, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.ArchivedAt != nil || int(current.State) != expected || current.AgentSessionID == nil || prior == nil || *current.AgentSessionID != *prior {
		return sql.ErrNoRows
	}
	blocked := int(machine.Blocked)
	_, err = s.Update(ctx, id, db.UpdateSessionParams{State: &blocked, BlockedReason: stringPtrPtr(reason)})
	return err
}

type runResumeSnapshotStore struct{ *mockSessionStore }

func (s *runResumeSnapshotStore) Get(ctx context.Context, id string) (*models.Session, error) {
	row, err := s.mockSessionStore.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	snapshot := *row
	return &snapshot, nil
}

func TestRunResumeGiveUpPreservesConcurrentOwner(t *testing.T) {
	for _, kind := range []string{"completion", "finalization", "replacement", "archive"} {
		t.Run(kind, func(t *testing.T) {
			f := newRotationFixture(t)
			row := f.sessions.sessions[f.sessionID]
			row.IsTmuxUnattended = true
			death := time.Now().Add(-time.Hour)
			row.UpdatedAt = death
			f.lc.sessions = &runResumeSnapshotStore{mockSessionStore: f.sessions}
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old"}}}}
			// Interleave a competing write after the session/telemetry snapshot,
			// before the exhausted ledger is acted on.
			ledger := &runResumeLedger{onList: func() {
				switch kind {
				case "completion":
					row.State = machine.Merged
				case "finalization":
					row.State = machine.Finalizing
				case "replacement":
					next := "replacement"
					row.AgentSessionID = &next
				case "archive":
					at := time.Now()
					row.ArchivedAt = &at
				}
			}}
			for i := range 3 {
				ledger.rows = append(ledger.rows, &models.ResumeAttempt{RunKey: "agent-old", AttemptedAt: death.Add(time.Duration(i) * time.Minute), FailureClass: "unknown"})
			}
			f.lc.SetRunResumeStore(ledger)
			if n := f.lc.ResumeDroppedRuns(context.Background()); n != 0 || len(f.runner.started) != 0 {
				t.Fatalf("resumed=%d launches=%d", n, len(f.runner.started))
			}
			if row.State == machine.Blocked || row.BlockedReason != nil {
				t.Fatalf("stale give-up blocked newer owner: %+v", row)
			}
		})
	}
}

func TestRunResumeKeySurvivesSameMillisecondReplacement(t *testing.T) {
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	original := &models.AgentChat{ID: "b", Seq: 1, AgentSessionID: "old-run", CreatedAt: at}
	replacement := &models.AgentChat{ID: "a", Seq: 2, AgentSessionID: "new-run-1", CreatedAt: at}
	for name, chats := range map[string][]*models.AgentChat{"newest first": {replacement, original}, "oldest first": {original, replacement}} {
		if key := RunResumeKey(&models.Session{}, chats); key != "old-run" {
			t.Fatalf("%s: key=%q, want old-run", name, key)
		}
	}
}

// TestRunResumeSkipsASessionAnotherResumeHolds pins the per-session resume
// gate: while one resume holds a session, the automatic resume leaves that
// session for a later pass instead of relaunching it a second time.
func TestRunResumeSkipsASessionAnotherResumeHolds(t *testing.T) {
	prev := RevivalLockTimeout
	RevivalLockTimeout = 20 * time.Millisecond
	t.Cleanup(func() { RevivalLockTimeout = prev })
	f := newRotationFixture(t)
	ctx := context.Background()
	death := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	now := death
	row := f.sessions.sessions[f.sessionID]
	row.IsTmuxUnattended = true
	row.UpdatedAt = death.Add(-time.Hour)
	f.lc.sessions = &runResumeObservedStore{mockSessionStore: f.sessions, observations: map[string]time.Time{}}
	f.lc.agentChats = &runResumeChats{mockAgentChatStore: &mockAgentChatStore{chatsBySession: map[string][]*models.AgentChat{f.sessionID: {{ID: "original", SessionID: f.sessionID, AgentSessionID: "agent-old", Title: "Run build", CreatedAt: death}}}}, now: func() time.Time { return now }}
	f.lc.SetRunResumeStore(&runResumeLedger{})
	f.lc.SetClockForTest(func() time.Time { return now })
	f.lc.ResumeDroppedRuns(ctx)
	now = death.Add(time.Minute)

	release, err := f.lc.lockRunResume(ctx, f.sessionID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if n := f.lc.ResumeDroppedRuns(ctx); n != 0 || len(f.runner.started) != 0 {
		t.Fatalf("resumed=%d launches=%d while another resume held the session, want 0 and 0", n, len(f.runner.started))
	}
	release()
	if n := f.lc.ResumeDroppedRuns(ctx); n != 1 || len(f.runner.started) != 1 {
		t.Fatalf("resumed=%d launches=%d once the gate was free, want 1 and 1", n, len(f.runner.started))
	}
}

// TestRunResumeGateErrors pins finding 4 of the #3204 verifier: a wait that
// times out names the gate, and a caller that gives up while waiting gets its
// own context's error rather than ErrRunResumeBusy.
func TestRunResumeGateErrors(t *testing.T) {
	lc := newTestLifecycle(newMockSessionStore(), newMockRepoStore(), nil, &recordingCronJobStore{}, &mockWorktreeManager{}, newMockAgentRunner(), nil, nil, zerolog.Nop())
	ctx := context.Background()
	release, err := lc.lockRunResume(ctx, "sess-1", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	_, err = lc.lockRunResume(ctx, "sess-1", 10*time.Millisecond)
	want := `another resume of this session is in progress: acquire run-resume lock "sess-1": context deadline exceeded`
	if !errors.Is(err, ErrRunResumeBusy) || err.Error() != want {
		t.Fatalf("timed-out wait = %v, want %q", err, want)
	}

	giveUp, cancel := context.WithCancel(ctx)
	time.AfterFunc(10*time.Millisecond, cancel)
	_, err = lc.lockRunResume(giveUp, "sess-1", time.Minute)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrRunResumeBusy) {
		t.Fatalf("abandoned wait = %v, want context.Canceled and not ErrRunResumeBusy", err)
	}
}
