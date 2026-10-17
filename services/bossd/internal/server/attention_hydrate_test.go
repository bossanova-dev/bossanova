package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	libevidence "github.com/recurser/bossalib/evidence"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sessionreason"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/evidence"
	"github.com/recurser/bossd/internal/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestAttentionListParkCases serves a sign-off park as a REVIEW whose merge
// records the approval and a blocked park as a FIX that never offers a merge,
// with the open checklist items its park note recorded.
func TestAttentionListParkCases(t *testing.T) {
	review, fix := pb.AttentionKind_ATTENTION_KIND_REVIEW, pb.AttentionKind_ATTENTION_KIND_FIX
	for _, tc := range []struct {
		id, code, label, sentence string
		kind                      pb.AttentionKind
		openItems                 []string
	}{
		{"2979", "human-review:ui", "", "UI change · evidence video (2:40)", review, nil},
		{"2983", "human-review:security", "outbound requests", "Security change: outbound requests", review, nil},
		{"2987", "human-review:database", "", "Database migration", review, nil},
		{"3189", "human-review:unproven", "", "Some checks have no evidence", review, nil},
		{"no-receipt", "no-receipt", "", "No review of the latest commit", review, nil},
		{"mixed", "human-review:ui,ledger-open", "", "The PR checklist has 1 open item", fix, []string{"Open human follow-up: watch the rollout"}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			n := 1
			row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, UpdatedAt: time.Now()}
			tracker := status.NewDisplayTracker()
			tracker.Set(row.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, VerifyReason: tc.code, VerifyLabel: tc.label, HeadSHA: timelineHead})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
			s.evidence = openItemsEvidence{
				stubEvidence: stubEvidence{"s": evidence.Entry{Summary: libevidence.Summary{Head: timelineHead, VideoURL: "https://proof.example/video.mp4", VideoDuration: 160 * time.Second, Proven: 3, Total: 4}}},
				head:         timelineHead,
				items:        []string{"Open human follow-up: watch the rollout"},
			}
			resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			got := onlySession(t, resp.Msg.Sessions).GetAttention()
			if got.GetKind() != tc.kind || got.GetSentence() != tc.sentence {
				t.Fatalf("attention = %v", got)
			}
			if merges := slices.Contains(got.GetActions(), pb.AttentionAction_ATTENTION_ACTION_MERGE); merges != (tc.kind == review) {
				t.Fatalf("actions = %v; MERGE offered = %v on a %v", got.GetActions(), merges, tc.kind)
			}
			if !slices.Equal(got.GetOpenItems(), tc.openItems) {
				t.Fatalf("open items = %q, want %q", got.GetOpenItems(), tc.openItems)
			}
		})
	}
}

// openItemsEvidence serves items as the park note's open items on head.
type openItemsEvidence struct {
	stubEvidence
	head  string
	items []string
}

func (e openItemsEvidence) OpenItems(_, head string) []string {
	if head != e.head {
		return nil
	}
	return e.items
}

type attentionCronStore struct {
	db.CronJobStore
	jobs  []*models.CronJob
	reads int
}

func (f *attentionCronStore) ListByRepo(context.Context, string) ([]*models.CronJob, error) {
	f.reads++
	return f.jobs, nil
}

type attentionSessionStore struct {
	db.SessionStore
	row *models.Session
}

func (f *attentionSessionStore) Get(context.Context, string) (*models.Session, error) {
	return f.row, nil
}

type attentionResumeStore struct {
	db.ResumeAttemptStore
	rows []*models.ResumeAttempt
	key  string
}

func (f *attentionResumeStore) ListForRun(_ context.Context, _, key string) ([]*models.ResumeAttempt, error) {
	f.key = key
	return f.rows, nil
}

// TestAttentionFactoryMerge reads the repo's verify jobs from its cron jobs,
// once a minute per repo, and answers a green, mergeable PR nothing verified
// by them: in progress when a verify job is enabled, since verify will reach
// the head, and otherwise an unverified MERGE, a disabled verify job included.
// A job counts as verify by its prompt or by running the boss-verify cron gate.
func TestAttentionFactoryMerge(t *testing.T) {
	const verifyGate = `node "$HOME/.claude/skills/boss-verify/toolbox/cron-gates/boss-verify.mjs"`
	for _, tc := range []struct {
		name string
		jobs []*models.CronJob
		kind pb.AttentionKind
	}{
		{"absent", nil, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"unrelated", []*models.CronJob{{Prompt: "/boss-build", IsEnabled: true}}, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"disabled", []*models.CronJob{{Prompt: "/boss-verify", IsEnabled: false}}, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"disabled gate", []*models.CronJob{{Prompt: "/gi-verify", GateCommand: verifyGate, IsEnabled: false}}, pb.AttentionKind_ATTENTION_KIND_MERGE},
		{"enabled", []*models.CronJob{{Prompt: "$boss-verify headless", IsEnabled: true}}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{"enabled gate", []*models.CronJob{{Prompt: "/gi-verify", GateCommand: verifyGate, IsEnabled: true}}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
		{"one of two enabled", []*models.CronJob{{Prompt: "/boss-verify", IsEnabled: false}, {Prompt: "/boss-verify", IsEnabled: true}}, pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			mergeable := true
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview}
			tr := status.NewDisplayTracker()
			tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			crons := &attentionCronStore{jobs: tc.jobs}
			s.cronJobs = crons
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: []db.CheckSnapshot{{HeadSHA: timelineHead, RawJSON: checksJSON(t, greenCI(2, ""))}}}
			// Use ordinary checks only: the fixture's pending verify status is not CI.
			for range 2 {
				resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
				if err != nil {
					t.Fatal(err)
				}
				got := onlySession(t, resp.Msg.Sessions).GetAttention()
				if got.GetKind() != tc.kind {
					t.Fatalf("attention = %v, want %v", got, tc.kind)
				}
				if tc.kind != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS && !got.GetIsUnverified() {
					t.Fatalf("merge-ready attention %v is not marked unverified", got)
				}
			}
			if crons.reads != 1 {
				t.Fatalf("cron reads=%d, want 1 (cached)", crons.reads)
			}
		})
	}
}

// TestAttentionVerifiedMerge reads "verified" from the persisted check
// snapshot of the current head: a successful boss/verify status there serves a
// verified MERGE, and no verdict there serves the same MERGE marked
// is_unverified. A snapshot of an older head is not read at all, so its
// verdict never makes the current head merge-ready.
func TestAttentionVerifiedMerge(t *testing.T) {
	const oldHead = "1111111111111111111111111111111111111111"
	success := vcs.CheckConclusionSuccess
	verified := append(greenCI(2, ""), vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success, Description: "verified"})
	mergeActions := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_MERGE, pb.AttentionAction_ATTENTION_ACTION_GITHUB}
	for _, tc := range []struct {
		name       string
		snaps      []db.CheckSnapshot
		kind       pb.AttentionKind
		sentence   string
		actions    []pb.AttentionAction
		unverified bool
	}{
		{
			name:     "verified head",
			snaps:    []db.CheckSnapshot{{HeadSHA: timelineHead, RawJSON: checksJSON(t, verified)}},
			kind:     pb.AttentionKind_ATTENTION_KIND_MERGE,
			sentence: "Ready to merge",
			actions:  mergeActions,
		},
		{
			name:       "no verdict on the head",
			snaps:      []db.CheckSnapshot{{HeadSHA: timelineHead, RawJSON: checksJSON(t, greenCI(2, ""))}},
			kind:       pb.AttentionKind_ATTENTION_KIND_MERGE,
			sentence:   "Ready to merge",
			actions:    mergeActions,
			unverified: true,
		},
		{
			name:  "verdict on an older head",
			snaps: []db.CheckSnapshot{{HeadSHA: oldHead, RawJSON: checksJSON(t, verified)}},
			kind:  pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := 1
			mergeable := true
			row := &models.Session{ID: "s", RepoID: "r", PRNumber: &n, State: machine.ReadyForReview}
			tr := status.NewDisplayTracker()
			tr.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusPassing, HeadSHA: timelineHead, Mergeable: &mergeable})
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tr, nil)
			s.checkSnapshots = &fakeCheckSnapshotStore{snaps: tc.snaps}
			resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			got := onlySession(t, resp.Msg.Sessions).GetAttention()
			if got.GetKind() != tc.kind || got.GetSentence() != tc.sentence || !slices.Equal(got.GetActions(), tc.actions) {
				t.Fatalf("attention = %v, want %v %q %v", got, tc.kind, tc.sentence, tc.actions)
			}
			if got.GetIsUnverified() != tc.unverified {
				t.Fatalf("is_unverified = %v, want %v", got.GetIsUnverified(), tc.unverified)
			}
			if len(got.GetParkCodes()) != 0 {
				t.Fatalf("park codes = %v, want none", got.GetParkCodes())
			}
		})
	}
}

func TestAttentionQuestionCache(t *testing.T) {
	row := &models.Session{ID: "s", RepoID: "r"}
	chat := &models.AgentChat{ID: "c", SessionID: "s", AgentSessionID: "a", AgentName: "claude"}
	tracker := status.NewTracker()
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_QUESTION, at(10))
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, map[string][]*models.AgentChat{"s": {chat}}, nil, tracker)
	s.sessions = &attentionSessionStore{row: row}
	client := &transcriptAgentClient{resp: &pb.ReadTranscriptResponse{Exists: true, FinalAssistantText: "Which option?"}}
	s.agentClients = map[string]agent.AgentRunnerClient{"claude": client}
	// Transcript resolution uses the chat row, just as the actual RPC does.
	s.agentChats = &chatStoreFake{chat: chat}
	for range 2 {
		p := SessionToProto(row)
		s.hydrateAttention(context.Background(), p, attentionDeps{row: row, chats: []*models.AgentChat{chat}})
		if got := p.GetAttention(); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_QUESTION || got.GetSentence() != "“Which option?”" || got.GetTargetChatId() != "a" {
			t.Fatalf("attention=%v", got)
		}
	}
	if client.calls != 1 {
		t.Fatalf("reads=%d", client.calls)
	}
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_WORKING, at(11))
	tracker.Update("a", pb.ChatStatus_CHAT_STATUS_QUESTION, at(12))
	client.resp.FinalAssistantText = "A new question?"
	p := SessionToProto(row)
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row, chats: []*models.AgentChat{chat}})
	if p.GetAttention().GetSentence() != "“A new question?”" || client.calls != 2 {
		t.Fatalf("new question=%v reads=%d", p.Attention, client.calls)
	}
}

func TestAttentionResumeDismissalAndRecovery(t *testing.T) {
	chatID := "run-a"
	row := &models.Session{ID: "s", RepoID: "r", AgentSessionID: &chatID, State: machine.Blocked}
	ledger := &attentionResumeStore{rows: []*models.ResumeAttempt{{AttemptedAt: at(10), FailureClass: "auth_expired"}, {AttemptedAt: at(12), FailureClass: "auth_expired"}}}
	s := &Server{resumeAttempts: ledger}
	p := SessionToProto(row)
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX || !strings.Contains(p.GetAttention().GetSentence(), "same 401") || ledger.key != chatID {
		t.Fatalf("attention=%v key=%s", p.Attention, ledger.key)
	}
	row.DismissedAlertKey = "resume:auth_expired"
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatal("dismissed alert still visible")
	}
	row.DismissedAlertKey = ""
	row.State = machine.ImplementingPlan
	p.LastAgentActivityAt = timestamppb.New(at(15))
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatal("recovered run still needs fix")
	}
}

func TestAttention2986NoStaleAlert(t *testing.T) {
	n := 2986
	reason := "old repair failed"
	row := &models.Session{ID: "s", RepoID: "r", State: machine.ReadyForReview, PRNumber: &n, BlockedReason: &reason, LastRepairAttemptCount: 5, LastRepairHeadSHA: "old", LastRepairStartedAt: timePtr(at(10))}
	tracker := status.NewDisplayTracker()
	tracker.Set("s", vcs.DisplayInfo{Status: vcs.DisplayStatusNeedsHuman, HeadSHA: timelineHead, VerifyReason: "human-review:security"})
	s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, nil, tracker, nil)
	chatID := "productive"
	chats := map[string][]*models.AgentChat{"s": {{SessionID: "s", AgentSessionID: chatID}}}
	s.agentChats = &listSessionsAgentChatStoreFake{chats: chats}
	s.chatStatus = status.NewTracker()
	s.chatStatus.Update(chatID, pb.ChatStatus_CHAT_STATUS_WORKING, at(20))
	s.rotationEvents = fakeRotationEventStore{events: []db.RotationEvent{{SessionID: "s", CreatedAt: at(16), Trigger: pb.RotationTrigger_ROTATION_TRIGGER_AUTH_INVALIDATED.String(), Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED.String()}}}
	oldHead := "old"
	row.LastAttemptHeadSHA = &oldHead
	for _, state := range []machine.State{machine.ReadyForReview, machine.Blocked} {
		row.State = state
		resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
		if err != nil {
			t.Fatal(err)
		}
		if got := onlySession(t, resp.Msg.Sessions).GetAttention(); got.GetKind() != pb.AttentionKind_ATTENTION_KIND_REVIEW {
			t.Fatalf("state=%v attention=%v", state, got)
		}
	}
}

func TestAttentionResumeActionsMatchResumableStates(t *testing.T) {
	for _, kind := range []string{"blocked", "repair-exhausted", "auth-wedge", "archived", "no-prompt"} {
		t.Run(kind, func(t *testing.T) {
			id := "run"
			row := &models.Session{ID: "s", State: machine.Blocked, AgentSessionID: &id, Plan: "/boss-build TEST-1", WorktreePath: "/tmp/work"}
			p := SessionToProto(row)
			switch kind {
			case "repair-exhausted":
				row.State = machine.FixingChecks
				row.LastRepairAttemptCount = 5
				n := 1
				row.PRNumber = &n
			case "auth-wedge":
				row.State = machine.ImplementingPlan
				p.RotationEvents = []*pb.RotationEvent{{Outcome: pb.RotationOutcome_ROTATION_OUTCOME_RESPAWN_CAP_EXHAUSTED, CreatedAt: timestamppb.New(time.Now())}}
			case "archived":
				now := time.Now()
				row.ArchivedAt = &now
			case "no-prompt":
				row.Plan = ""
			}
			s := &Server{}
			s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
			if p.GetAttention().GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
				t.Fatalf("attention=%v", p.Attention)
			}
			offered := slices.Contains(p.Attention.Actions, pb.AttentionAction_ATTENTION_ACTION_RESUME)
			want := kind != "archived" && kind != "no-prompt"
			if offered != want {
				t.Fatalf("offered=%v want=%v", offered, want)
			}
		})
	}
}

func TestAttentionResumeLedgerKeepsOriginalRunKey(t *testing.T) {
	original := "first"
	latest := "third"
	cron := "cron-job"
	reason := "automatic resume ledger: attempt budget exhausted"
	row := &models.Session{ID: "s", State: machine.Blocked, AgentSessionID: &latest, CronJobID: &cron, Plan: "prompt", WorktreePath: "/tmp/work", BlockedReason: &reason}
	ledger := &attentionResumeStore{rows: []*models.ResumeAttempt{{AttemptedAt: at(10), FailureClass: "auth_expired"}, {AttemptedAt: at(12), FailureClass: "auth_expired"}}}
	s := &Server{resumeAttempts: ledger}
	p := SessionToProto(row)
	p.LastAgentActivityAt = timestamppb.New(at(13))
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row, chats: []*models.AgentChat{{ID: "first", AgentSessionID: original, CreatedAt: at(1)}, {ID: "third", AgentSessionID: latest, CreatedAt: at(12)}}})
	if ledger.key != "cron-job:first" || !strings.Contains(p.Attention.Sentence, "same 401") {
		t.Fatalf("key=%q attention=%v", ledger.key, p.Attention)
	}
}

func TestAttentionResumeDoesNotExhaustLiveReplacement(t *testing.T) {
	id := "third"
	row := &models.Session{ID: "s", State: machine.ImplementingPlan, AgentSessionID: &id, Plan: "prompt", WorktreePath: "/tmp/work"}
	ledger := &attentionResumeStore{rows: []*models.ResumeAttempt{{AttemptedAt: at(10), FailureClass: "crash"}, {AttemptedAt: at(12), FailureClass: "crash"}, {AttemptedAt: at(20), FailureClass: "unknown"}}}
	s := &Server{resumeAttempts: ledger}
	p := SessionToProto(row)
	s.hydrateAttention(context.Background(), p, attentionDeps{row: row})
	if p.GetAttention().GetKind() == pb.AttentionKind_ATTENTION_KIND_FIX {
		t.Fatalf("third replacement has not failed: %v", p.Attention)
	}
}

// TestAttentionDeadBootstrapOffersOnlyDismiss pins the 2026-10-16 madverts
// row: a cron session the stranded-bootstrap reaper blocked after a daemon
// restart has no agent chat, no tmux pane and no worktree. Offering OPEN_CHAT
// minted a chat that failed on "worktree directory missing", and "open the
// chat, then resume" named two impossible steps.
func TestAttentionDeadBootstrapOffersOnlyDismiss(t *testing.T) {
	reason := sessionreason.BootstrapStranded()
	cronJob := "cron-mv-sweep-build"
	for name, cron := range map[string]*string{"cron": &cronJob, "interactive": nil} {
		t.Run(name, func(t *testing.T) {
			row := &models.Session{ID: "s", RepoID: "r", State: machine.Blocked, CronJobID: cron, Plan: "/boss-build --assignee me",
				WorktreePath: "/Users/dave/.bossanova/worktrees/madverts-core/cron-mv-sweep-build-1792103581", BlockedReason: &reason}
			p := SessionToProto(row)
			(&Server{}).hydrateAttention(context.Background(), p, attentionDeps{row: row})
			a := p.GetAttention()
			if a.GetKind() != pb.AttentionKind_ATTENTION_KIND_FIX {
				t.Fatalf("kind = %v, want FIX", a.GetKind())
			}
			if want := []pb.AttentionAction{pb.AttentionAction_ATTENTION_ACTION_DISMISS}; !slices.Equal(a.GetActions(), want) {
				t.Fatalf("actions = %v, want %v", a.GetActions(), want)
			}
			for _, advice := range []string{"open the chat", "resume", "archive it"} {
				if strings.Contains(a.GetSentence(), advice) {
					t.Fatalf("sentence = %q advises %q; it must state only what happened", a.GetSentence(), advice)
				}
			}
			if a.GetTargetChatId() != "" {
				t.Fatalf("target chat = %q, want none", a.GetTargetChatId())
			}
			want := "Session died before setup finished and left no worktree or chat"
			if cron != nil {
				want = "Run died before setup finished and left no worktree or chat"
			}
			if a.GetSentence() != want {
				t.Fatalf("sentence = %q, want %q", a.GetSentence(), want)
			}
		})
	}
}

// TestAttentionProgressLabelMatchesChatState serves a quick chat (no PR) in each
// chat state and checks the in-progress label names that state, as the display
// label does: a stopped chat reads "stopped", never "idle".
func TestAttentionProgressLabelMatchesChatState(t *testing.T) {
	for _, tc := range []struct {
		chat pb.ChatStatus
		want string
	}{
		{pb.ChatStatus_CHAT_STATUS_STOPPED, "stopped"},
		{pb.ChatStatus_CHAT_STATUS_IDLE, "idle"},
		{pb.ChatStatus_CHAT_STATUS_WORKING, "working"},
	} {
		t.Run(tc.want, func(t *testing.T) {
			chatID := "chat-" + tc.want
			row := &models.Session{ID: "s", RepoID: "r", State: machine.ImplementingPlan, AgentSessionID: &chatID, CreatedAt: time.Now(), UpdatedAt: time.Now()}
			chats := map[string][]*models.AgentChat{"s": {{ID: "c", SessionID: "s", AgentSessionID: chatID}}}
			tracker := status.NewTracker()
			tracker.Update(chatID, tc.chat, time.Now())
			s := newListSessionsDisplayStatusTestServer([]*models.Session{row}, chats, status.NewDisplayTracker(), tracker)
			resp, err := s.ListSessions(context.Background(), connect.NewRequest(&pb.ListSessionsRequest{}))
			if err != nil {
				t.Fatal(err)
			}
			att := onlySession(t, resp.Msg.Sessions).GetAttention()
			if att.GetKind() != pb.AttentionKind_ATTENTION_KIND_IN_PROGRESS || att.GetProgressLabel() != tc.want {
				t.Fatalf("attention = %v %q, want IN_PROGRESS %q", att.GetKind(), att.GetProgressLabel(), tc.want)
			}
		})
	}
}
