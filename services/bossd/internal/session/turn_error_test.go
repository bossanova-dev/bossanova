package session

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func setTurnErrorRunner(l *Lifecycle, sessionID, agentID string, response *bossanovav1.ReadTranscriptResponse, rpcErr error) *fakeAgentForLifecycle {
	client := &fakeAgentForLifecycle{ReadTranscriptFunc: func(*bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
		return response, rpcErr
	}}
	l.SetAgents(map[string]agent.RunnerClient{"codex": client})
	l.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{sessionID: {{AgentSessionID: agentID, AgentName: "codex"}}}}
	return client
}

func TestProviderErrorStrandedSweep_Sessions_AC135(t *testing.T) {
	for _, tc := range []struct {
		name                                                string
		enabled, finished, receipt, exists, rpcError, fresh bool
		message                                             string
		routed                                              int
		blocked                                             bool
	}{
		{name: "disabled blocks", exists: true, message: "capacity", blocked: true},
		{name: "enabled defers", enabled: true, exists: true, message: "capacity"},
		{name: "terminal finishes", finished: true, exists: true, message: "capacity", routed: 1},
		{name: "receipt finishes", receipt: true, exists: true, message: "capacity", routed: 1},
		{name: "clean finishes", exists: true, routed: 1},
		{name: "missing finishes", message: "capacity", routed: 1},
		{name: "rpc failure finishes", rpcError: true, exists: true, message: "capacity", routed: 1},
		{name: "busy avoids transcript", fresh: true, exists: true, message: "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			l, store, _, finalizer := newSweepLifecycle(t, dir)
			var logs bytes.Buffer
			l.logger = zerolog.New(&logs)
			s := strandedCronSession("s1", "a1")
			s.Plan = "build"
			s.WorktreePath = dir
			store.sessions[s.ID] = s
			var rpcErr error
			if tc.rpcError {
				rpcErr = errors.New("unavailable")
			}
			client := setTurnErrorRunner(l, s.ID, "a1", &bossanovav1.ReadTranscriptResponse{Exists: tc.exists, LastTurnErrorMessage: tc.message}, rpcErr)
			if tc.enabled {
				l.SetRunResumeStore(&runResumeLedger{})
				pane := "pane"
				l.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{s.ID: {{AgentSessionID: "a1", AgentName: "codex", TmuxSessionName: &pane}}}}
				l.tmux = stubTmuxForSweep(true, "")
			}
			if tc.finished {
				l.agentRuns = &runResumeTelemetry{runs: []domain.AgentRun{{AgentSessionID: "a1", TerminalState: "review_ready"}}}
			}
			if tc.receipt {
				s.PRNumber = ptr(1)
				l.provider = &runResumeProvider{mockVCSProvider: &mockVCSProvider{}}
			}
			age := cronAgentIdleThreshold + time.Minute
			if tc.fresh {
				age = 0
			}
			seedLog(t, dir, "a1", age)
			n, err := l.RecoverStrandedCronSessionsPeriodic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if n != tc.routed || finalizer.count() != tc.routed {
				t.Fatalf("routed=%d finalized=%d want %d", n, finalizer.count(), tc.routed)
			}
			if blocked := s.State == machine.Blocked; blocked != tc.blocked {
				t.Fatalf("blocked=%v want %v", blocked, tc.blocked)
			}
			if tc.blocked && !sessionreason.IsAgentProviderError(s.BlockedReason) {
				t.Fatalf("reason=%v", s.BlockedReason)
			}
			if tc.blocked && !strings.Contains(logs.String(), "last turn ended on a provider error") {
				t.Fatalf("missing warning: %s", logs.String())
			}
			if tc.fresh && client.readTranscriptCalls != 0 {
				t.Fatal("busy run read transcript")
			}
		})
	}
}

func TestProviderErrorBlocksEveryReapState_Sessions_AC136(t *testing.T) {
	for _, startup := range []bool{false, true} {
		states := zeroOutputReapStates
		if startup {
			states = strandedReapStates
		}
		for _, state := range states {
			t.Run(state.String(), func(t *testing.T) {
				dir := t.TempDir()
				l, store, _, finalizer := newSweepLifecycle(t, dir)
				s := strandedCronSession("s1", "a1")
				s.State = state
				s.IsQuickChat = !startup
				store.sessions[s.ID] = s
				setTurnErrorRunner(l, s.ID, "a1", &bossanovav1.ReadTranscriptResponse{Exists: true, LastTurnErrorMessage: "capacity"}, nil)
				seedLog(t, dir, "a1", cronAgentIdleThreshold+time.Minute)
				sweep := l.RecoverStrandedCronSessionsPeriodic
				if startup {
					sweep = l.RecoverStrandedCronSessionsAtStartup
				}
				_, err := sweep(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if finalizer.count() != 0 || s.State != machine.Blocked || !sessionreason.IsAgentProviderError(s.BlockedReason) {
					t.Fatalf("state=%s reason=%v finalized=%d", s.State, s.BlockedReason, finalizer.count())
				}
			})
		}
	}
}

func TestCompletionGateProviderError(t *testing.T) {
	for _, kind := range []string{"error", "clean", "nil", "busy"} {
		t.Run(kind, func(t *testing.T) {
			store := newGateSessionStore()
			store.sessions["s"] = &models.Session{ID: "s", CronJobID: ptr("cron"), AgentSessionID: ptr("a")}
			calls := 0
			var probe func(*models.Session) bool
			if kind != "nil" {
				probe = func(*models.Session) bool { calls++; return kind == "error" }
			}
			gate := NewCronCompletionGate(CronCompletionGateDeps{Sessions: store, Finalizer: &recordingCronFinalizer{}, Logger: zerolog.Nop(), RunCompletionEvidence: func(*models.Session) (bool, string) { return kind != "busy", "idle" }, ChatTurnErrored: probe})
			got := gate.checkAndFinalize(context.Background(), "s")
			want := cronCompletionGateCheckFinalize
			if kind == "error" || kind == "busy" {
				want = cronCompletionGateCheckDone
			}
			if got != want {
				t.Fatalf("got %d want %d", got, want)
			}
			if kind == "busy" && calls != 0 {
				t.Fatal("busy gate read transcript")
			}
		})
	}
}

func TestSessionTurnErrorProviderIDFallback(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "provider authoritative", true: "agent fallback"}[missing], func(t *testing.T) {
			f := newRotationFixture(t)
			s := f.sessions.sessions[f.sessionID]
			chat := &domain.AgentChat{AgentSessionID: *s.AgentSessionID, AgentName: "codex", ProviderSessionID: ptr("provider")}
			f.lc.agentChats = &mockAgentChatStore{chatsBySession: map[string][]*domain.AgentChat{s.ID: {chat}}}
			var ids []string
			f.lc.SetAgents(map[string]agent.RunnerClient{"codex": &fakeAgentForLifecycle{ReadTranscriptFunc: func(req *bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
				ids = append(ids, req.AgentSessionId)
				if req.MaxMessages != 1 {
					t.Fatal("unbounded message request")
				}
				if req.AgentSessionId == "provider" {
					return &bossanovav1.ReadTranscriptResponse{Exists: !missing}, nil
				}
				return &bossanovav1.ReadTranscriptResponse{Exists: true, LastTurnErrorMessage: " capacity "}, nil
			}}})
			msg, got := f.lc.sessionTurnErrored(context.Background(), s)
			if got != missing {
				t.Fatalf("error=%v want %v ids=%v", got, missing, ids)
			}
			if ids[0] != "provider" || (missing && (len(ids) != 2 || msg != "capacity")) {
				t.Fatalf("ids=%v message=%q", ids, msg)
			}
		})
	}
}

func TestRunResumeProviderErrorLivePane_Sessions_AC137(t *testing.T) {
	for _, kind := range []string{"error", "clean", "busy", "exhausted"} {
		t.Run(kind, func(t *testing.T) {
			h := newStartTmuxChatHarness(t)
			s := h.sessions.sessions["sess-1"]
			s.IsTmuxUnattended = true
			s.AgentSessionID = ptr("old-run")
			s.WorktreePath = t.TempDir()
			death := time.Now().Add(-time.Hour)
			now := time.Now()
			s.UpdatedAt = death
			h.lc.SetClockForTest(func() time.Time { return now })
			h.chats.chatsBySession = map[string][]*domain.AgentChat{s.ID: {{ID: "original", SessionID: s.ID, AgentSessionID: "old-run", AgentName: "claude", TmuxSessionName: ptr("old-pane"), CreatedAt: death}}}
			h.lc.agentRuns = &runResumeTelemetry{runs: []domain.AgentRun{{AgentSessionID: "old-run", StoppedAt: &death}}}
			h.agentFake.ReadTranscriptFunc = func(*bossanovav1.ReadTranscriptRequest) (*bossanovav1.ReadTranscriptResponse, error) {
				message := "capacity"
				if kind == "clean" {
					message = ""
				}
				return &bossanovav1.ReadTranscriptResponse{Exists: true, LastTurnErrorMessage: message}, nil
			}
			ledger := &runResumeLedger{}
			h.lc.SetRunResumeStore(ledger)
			if kind == "exhausted" {
				for i := 1; i <= 3; i++ {
					ledger.rows = append(ledger.rows, &domain.ResumeAttempt{BudgetKey: "old-run", AttemptNumber: i, AttemptedAt: death})
				}
			}
			age := cronAgentIdleThreshold + time.Minute
			if kind == "busy" {
				age = 0
			}
			seedLog(t, h.logsDir, "old-run", age)
			resumed := h.lc.ResumeDroppedRuns(context.Background())
			if kind == "error" {
				if resumed != 1 || len(ledger.rows) != 1 || h.findCall("new-session") == nil {
					t.Fatalf("resumed=%d ledger=%d new-session=%v", resumed, len(ledger.rows), h.findCall("new-session"))
				}
			} else if resumed != 0 || h.findCall("new-session") != nil {
				t.Fatalf("unexpected launch: resumed=%d", resumed)
			}
			if kind == "exhausted" && s.State != machine.Blocked {
				t.Fatalf("exhausted state=%s", s.State)
			}
			if kind == "busy" && h.agentFake.readTranscriptCalls != 0 {
				t.Fatal("busy resume read transcript")
			}
		})
	}
}
