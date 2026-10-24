package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossd/internal/account"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type retrySubmitError struct{}

func (retrySubmitError) Error() string                       { return "line was not submitted" }
func (retrySubmitError) SubmitOutcome() domain.SubmitOutcome { return domain.OutcomeNotSubmitted }

func TestTransientAgentStart(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"not submitted", retrySubmitError{}, true},
		{"wrapped not submitted", fmt.Errorf("launch: %w", retrySubmitError{}), true},
		{"deadline", status.Error(codes.DeadlineExceeded, "probe"), true},
		{"wrapped unavailable", fmt.Errorf("plugin: %w", status.Error(codes.Unavailable, "busy")), true},
		{"substep deadline", context.DeadlineExceeded, true},
		{"capability refusal", status.Error(codes.FailedPrecondition, "missing"), false},
		{"not loaded", agent.ErrAgentNotLoaded, false},
		{"profile unsupported", agent.ErrHeadlessCapabilityProfileUnsupported, false},
		{"wrapped deadline", fmt.Errorf("probe: %w", status.Error(codes.DeadlineExceeded, "slow")), true},
		{"credential refusal", redactedInjection(status.Error(codes.Unavailable, "credential")), false},
		{"account refusal", &account.InjectionError{}, false},
		{"joined unavailable", errors.Join(errors.New("outer"), status.Error(codes.Unavailable, "busy")), true},
		{"joined refusal wins", errors.Join(status.Error(codes.Unavailable, "busy"), status.Error(codes.FailedPrecondition, "refused")), false},
		{"plain error", errors.New("unrecognized"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientAgentStartError(tc.err); got != tc.want {
				t.Fatalf("transient = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestStartRetry(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  int
		err       error
		budget    time.Duration
		wantCalls int
		wantErr   bool
	}{
		{"second succeeds", 1, retrySubmitError{}, 10 * time.Minute, 2, false},
		{"preflight twice", 2, status.Error(codes.DeadlineExceeded, "probe"), 10 * time.Minute, 3, false},
		{"refusal", 3, status.Error(codes.FailedPrecondition, "missing"), 10 * time.Minute, 1, true},
		{"not loaded", 3, agent.ErrAgentNotLoaded, 10 * time.Minute, 1, true},
		{"credential", 3, redactedInjection(context.DeadlineExceeded), 10 * time.Minute, 1, true},
		{"exhausted", 3, retrySubmitError{}, 10 * time.Minute, 3, true},
		{"little time", 1, retrySubmitError{}, time.Minute, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), tc.budget)
			defer cancel()
			var progress bytes.Buffer
			l := &Lifecycle{logger: zerolog.Nop()}
			retry := agentStartRetry{backoffs: []time.Duration{time.Millisecond, time.Millisecond}}
			calls := 0
			id, err := l.startAgentWithRetry(ctx, "sess", &progress, &retry, func() (string, error) {
				calls++
				if calls <= tc.failures {
					return "", tc.err
				}
				return "live", nil
			})
			if calls != tc.wantCalls || (err != nil) != tc.wantErr {
				t.Fatalf("calls=%d id=%q err=%v", calls, id, err)
			}
			if !tc.wantErr && id != "live" {
				t.Fatalf("id=%q", id)
			}
			if got := strings.Count(progress.String(), "retrying in"); got != calls-1 {
				t.Fatalf("progress=%q, want %d retry lines", progress.String(), calls-1)
			}
			if err != nil {
				if !strings.Contains(err.Error(), fmt.Sprintf("after %d attempts", calls)) {
					t.Fatalf("missing count: %v", err)
				}
				if !errors.Is(err, tc.err) {
					t.Fatalf("cause lost: %v", err)
				}
				if domain.OutcomeOf(tc.err) == domain.OutcomeNotSubmitted && domain.OutcomeOf(err) != domain.OutcomeNotSubmitted {
					t.Fatalf("outcome lost: %v", err)
				}
			}
		})
	}
}

func TestStartRetryCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	l := &Lifecycle{logger: zerolog.Nop()}
	retry := agentStartRetry{backoffs: []time.Duration{time.Hour, time.Hour}}
	calls := 0
	_, err := l.startAgentWithRetry(ctx, "sess", nil, &retry, func() (string, error) { calls++; cancel(); return "", context.DeadlineExceeded })
	if calls != 1 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

// Use a stateful chat store to assert the visible list, rather than only the
// number of cleanup calls. Other existing launch fixtures record writes only.
type retryChatStore struct {
	*mockAgentChatStore
	rows map[string]*domain.AgentChat
}

func (s *retryChatStore) Create(ctx context.Context, p db.CreateAgentChatParams) (*domain.AgentChat, error) {
	row, err := s.mockAgentChatStore.Create(ctx, p)
	if err == nil {
		s.rows[p.AgentSessionID] = row
	}
	return row, err
}
func (s *retryChatStore) DeleteByAgentSessionID(ctx context.Context, id string) error {
	if err := s.mockAgentChatStore.DeleteByAgentSessionID(ctx, id); err != nil {
		return err
	}
	delete(s.rows, id)
	return nil
}

type retryPreflightRunner struct {
	*mockAgentRunner
	failures int
}

func (r *retryPreflightRunner) PreflightByAgentWithHeadlessCapabilityProfile(ctx context.Context, a, w, m, e string, env map[string]string, p pb.HeadlessCapabilityProfile, trust pb.ProjectTrustDecision) error {
	err := r.mockAgentRunner.PreflightByAgentWithHeadlessCapabilityProfile(ctx, a, w, m, e, env, p, trust)
	if r.preflightCalls <= r.failures {
		return status.Error(codes.DeadlineExceeded, "plugin busy")
	}
	return err
}

func retryIntegrationHarness(t *testing.T) *startTmuxChatHarness {
	t.Helper()
	h := newStartTmuxChatHarness(t, tmux.WithSessionStartReadyDeadline(30*time.Millisecond))
	h.sessions.sessions["sess-1"].State = machine.CreatingWorktree
	h.sessions.sessions["sess-1"].WorktreePath = ""
	h.wt.worktreePath = t.TempDir()
	setupScript := "run setup"
	h.repos.repos["repo-abcdef12"].SetupScript = &setupScript
	previous := agentStartRetryBackoffs
	agentStartRetryBackoffs = []time.Duration{time.Millisecond, time.Millisecond}
	t.Cleanup(func() { agentStartRetryBackoffs = previous })
	return h
}

func TestStartRetrySessionNotSubmitted(t *testing.T) {
	h := retryIntegrationHarness(t)
	rows := &retryChatStore{mockAgentChatStore: h.chats, rows: map[string]*domain.AgentChat{}}
	h.lc.agentChats = rows
	ids := 0
	h.lc.newTmuxChatAgentSessionID = func() string { ids++; return fmt.Sprintf("agent-%08d", ids) }
	h.tmuxFake.capturePaneFunc = func(calls []recordedTmuxCall) string {
		launches := 0
		for _, c := range calls {
			if c.subcommand == "new-session" {
				launches++
			}
		}
		// First pane never draws the command. The second behaves normally.
		if launches == 1 {
			return "Welcome to Claude\n❯\n"
		}
		return drawTypedLine("Welcome to Claude\n❯\n", calls)
	}
	h.sessions.sessions["sess-1"].Plan = "/boss-build"
	setupCalls := 0
	h.wt.onSetupScript = func() { setupCalls++ }
	var output bytes.Buffer
	err := h.lc.StartSession(context.Background(), "sess-1", StartSessionOpts{IsTmuxUnattended: true, DeferPR: true, SetupOutput: &output})
	if err != nil {
		t.Fatal(err)
	}
	if ids != 2 || len(rows.rows) != 1 || rows.rows["agent-00000002"] == nil {
		t.Fatalf("ids=%d rows=%v", ids, rows.rows)
	}
	if len(h.wt.archived) != 0 || len(h.wt.created) != 1 || setupCalls != 1 {
		t.Fatalf("archived=%v created=%d setup=%d", h.wt.archived, len(h.wt.created), setupCalls)
	}
	if strings.Count(output.String(), "retrying in") != 1 {
		t.Fatalf("progress=%q", output.String())
	}
	if h.sessions.sessions["sess-1"].State != machine.ImplementingPlan {
		t.Fatal("session did not reach implementing state")
	}
}

func TestStartRetrySessionPreflight(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failures  int
		refusal   bool
		wantErr   bool
		wantCalls int
	}{
		{"transient twice", 2, false, false, 4},
		{"non transient", 0, true, true, 1},
		{"transient exhausted", 3, false, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := retryIntegrationHarness(t)
			runner := &retryPreflightRunner{mockAgentRunner: h.agentRun, failures: tc.failures}
			if tc.refusal {
				runner.preflightErr = status.Error(codes.FailedPrecondition, "capability missing")
			}
			h.lc.agentRunner = runner
			h.sessions.sessions["sess-1"].Plan = ""
			setupCalls := 0
			h.wt.onSetupScript = func() { setupCalls++ }
			err := h.lc.StartSession(context.Background(), "sess-1", StartSessionOpts{IsTmuxUnattended: true, DeferPR: true, HeadlessCapabilityProfile: pb.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_TRACKER_PLAN_ATTACHMENT_V1})
			if (err != nil) != tc.wantErr || runner.preflightCalls != tc.wantCalls {
				t.Fatalf("err=%v calls=%d", err, runner.preflightCalls)
			}
			wantArchived := 0
			if tc.wantErr {
				wantArchived = 1
			}
			if len(h.wt.archived) != wantArchived || len(h.wt.created) != 1 || setupCalls != 1 {
				t.Fatalf("archived=%v created=%d setup=%d", h.wt.archived, len(h.wt.created), setupCalls)
			}
		})
	}
}

func TestStartRetrySharedBudget(t *testing.T) {
	l := &Lifecycle{logger: zerolog.Nop()}
	retry := agentStartRetry{backoffs: []time.Duration{time.Millisecond, time.Millisecond}}
	calls := 0
	_, err := l.startAgentWithRetry(context.Background(), "sess", nil, &retry, func() (string, error) {
		calls++
		if calls < 3 {
			return "", context.DeadlineExceeded
		}
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	calls = 0
	_, err = l.startAgentWithRetry(context.Background(), "sess", nil, &retry, func() (string, error) { calls++; return "", retrySubmitError{} })
	if calls != 1 || domain.OutcomeOf(err) != domain.OutcomeNotSubmitted || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestStartRetryCancelledDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l := &Lifecycle{logger: zerolog.Nop()}
	retry := agentStartRetry{backoffs: []time.Duration{time.Hour, time.Hour}}
	calls := 0
	_, err := l.startAgentWithRetry(ctx, "sess", nil, &retry, func() (string, error) {
		calls++
		time.AfterFunc(time.Millisecond, cancel)
		return "", retrySubmitError{}
	})
	if calls != 1 || domain.OutcomeOf(err) != domain.OutcomeNotSubmitted {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}
