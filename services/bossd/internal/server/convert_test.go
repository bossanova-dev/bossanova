package server

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/cron"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	goproto "google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestCanonicalRepoOriginURL pins the contract that bossd writes the
// canonical https form into pb.Session.RepoOriginUrl. Without this,
// bosso's webhook dispatcher routes by canonical URL while bossd
// reports the raw DB OriginURL (often the SSH form), and webhook
// deliveries silently regress to "no_ready_daemon" even when bossd
// has a matching session — caught in #327 and #345.
func TestCanonicalRepoOriginURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https github", "https://github.com/owner/repo", "https://github.com/owner/repo"},
		{"https with .git", "https://github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"ssh shorthand", "git@github.com:owner/repo.git", "https://github.com/owner/repo"},
		{"ssh proto", "ssh://git@github.com/owner/repo.git", "https://github.com/owner/repo"},
		{"GHES ssh", "git@github.company.com:team/svc.git", "https://github.company.com/team/svc"},
		{"empty input", "", ""},
		// Unparseable input: keep the original rather than dropping to "" —
		// empty would silently match every daemon with no repoIDs, so
		// preserving the input is the safer fallback.
		{"unparseable", "not-a-real-url", "not-a-real-url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CanonicalRepoOriginURL(tt.in)
			if got != tt.want {
				t.Errorf("CanonicalRepoOriginURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestConstructPRURL(t *testing.T) {
	tests := []struct {
		name      string
		originURL string
		prNumber  int
		want      string
	}{
		{"SSH format", "git@github.com:owner/repo.git", 42, "https://github.com/owner/repo/pull/42"},
		{"HTTPS format", "https://github.com/owner/repo.git", 7, "https://github.com/owner/repo/pull/7"},
		{"HTTPS no .git suffix", "https://github.com/owner/repo", 1, "https://github.com/owner/repo/pull/1"},
		{"empty URL", "", 1, ""},
		{"bare path no slash", "foobar", 1, ""},
		{"git protocol", "git://github.com/owner/repo.git", 5, "https://github.com/owner/repo/pull/5"},
		{"git protocol no .git", "git://github.com/owner/repo", 3, "https://github.com/owner/repo/pull/3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := constructPRURL(tt.originURL, tt.prNumber)
			if got != tt.want {
				t.Errorf("constructPRURL(%q, %d) = %q, want %q", tt.originURL, tt.prNumber, got, tt.want)
			}
		})
	}
}

func TestSuppressStaleConflictAttention(t *testing.T) {
	p := &pb.Session{
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_FAILING,
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE,
			Summary:        "auto-resolve conflicts disabled, needs human",
		},
	}

	suppressStaleConflictAttention(p)

	if p.AttentionStatus != nil {
		t.Fatalf("AttentionStatus = %+v, want nil for non-conflict display status", p.AttentionStatus)
	}
}

func TestAccountToProto_UsageSnapshotRoundTrip(t *testing.T) {
	reset5h := time.Now().Add(5 * time.Hour).UTC().Truncate(time.Millisecond)
	reset7d := time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Millisecond)
	fetchedAt := time.Now().UTC().Truncate(time.Millisecond)
	account := &models.Account{
		ID:            "acct-usage",
		Provider:      models.AccountProviderClaude,
		Label:         "work",
		Status:        models.AccountStatusActive,
		Health:        models.AccountHealthOK,
		CreatedAt:     fetchedAt,
		UpdatedAt:     fetchedAt,
		LastTestError: "",
		Usage: &models.UsageSnapshot{
			Util5h:    0.25,
			Util7d:    0.75,
			Reset5h:   &reset5h,
			Reset7d:   &reset7d,
			Status:    "warning",
			PlanTier:  "max",
			FetchedAt: &fetchedAt,
		},
	}

	got := accountToProto(account)
	if got.Usage == nil {
		t.Fatal("Usage = nil, want populated")
	}
	wire, err := goproto.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded pb.Account
	if err := goproto.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.GetUsage().GetUtil_5H() != account.Usage.Util5h || decoded.GetUsage().GetUtil_7D() != account.Usage.Util7d {
		t.Errorf("utils = %v/%v, want %v/%v", decoded.GetUsage().GetUtil_5H(), decoded.GetUsage().GetUtil_7D(), account.Usage.Util5h, account.Usage.Util7d)
	}
	if decoded.GetUsage().GetStatus() != account.Usage.Status || decoded.GetUsage().GetPlanTier() != account.Usage.PlanTier {
		t.Errorf("status/plan = %q/%q, want %q/%q", decoded.GetUsage().GetStatus(), decoded.GetUsage().GetPlanTier(), account.Usage.Status, account.Usage.PlanTier)
	}
	if !decoded.GetUsage().GetReset_5H().AsTime().Equal(reset5h) {
		t.Errorf("reset_5h = %v, want %v", decoded.GetUsage().GetReset_5H().AsTime(), reset5h)
	}
	if !decoded.GetUsage().GetReset_7D().AsTime().Equal(reset7d) {
		t.Errorf("reset_7d = %v, want %v", decoded.GetUsage().GetReset_7D().AsTime(), reset7d)
	}
	if !decoded.GetUsage().GetFetchedAt().AsTime().Equal(fetchedAt) {
		t.Errorf("fetched_at = %v, want %v", decoded.GetUsage().GetFetchedAt().AsTime(), fetchedAt)
	}
}

func TestAccountToProto_UsageSnapshotNilSafe(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	got := accountToProto(&models.Account{
		ID:        "acct-no-usage",
		Provider:  models.AccountProviderCodex,
		Label:     "work",
		Status:    models.AccountStatusActive,
		Health:    models.AccountHealthOK,
		CreatedAt: now,
		UpdatedAt: now,
	})
	if got.Usage != nil {
		t.Fatalf("Usage = %#v, want nil", got.Usage)
	}
}

func TestSuppressStaleConflictAttentionKeepsCurrentConflict(t *testing.T) {
	p := &pb.Session{
		DisplayStatus: pb.DisplayStatus_DISPLAY_STATUS_CONFLICT,
		AttentionStatus: &pb.AttentionStatus{
			NeedsAttention: true,
			Reason:         pb.AttentionReason_ATTENTION_REASON_MERGE_CONFLICT_UNRESOLVABLE,
			Summary:        "auto-resolve conflicts disabled, needs human",
		},
	}

	suppressStaleConflictAttention(p)

	if p.AttentionStatus == nil || !p.AttentionStatus.NeedsAttention {
		t.Fatalf("AttentionStatus = %+v, want current conflict warning kept", p.AttentionStatus)
	}
}

func TestRepoToProto(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	script := "make install"

	repo := &models.Repo{
		ID:                              "repo-1",
		DisplayName:                     "my-app",
		LocalPath:                       "/home/user/my-app",
		OriginURL:                       "https://github.com/user/my-app.git",
		DefaultBaseBranch:               "main",
		WorktreeBaseDir:                 "/home/user/.worktrees",
		SetupScript:                     &script,
		CanAutoMerge:                    true,
		CanAutoMergeDependabot:          true,
		CanAutoRepair:                   true,
		ShouldArchiveSessionsAfterMerge: true,
		CreatedAt:                       now,
		UpdatedAt:                       now,
	}

	p := repoToProto(repo)
	if p.Id != "repo-1" {
		t.Errorf("Id = %q, want %q", p.Id, "repo-1")
	}
	if p.DisplayName != "my-app" {
		t.Errorf("DisplayName = %q, want %q", p.DisplayName, "my-app")
	}
	if p.LocalPath != "/home/user/my-app" {
		t.Errorf("LocalPath = %q, want %q", p.LocalPath, "/home/user/my-app")
	}
	if p.OriginUrl != "https://github.com/user/my-app.git" {
		t.Errorf("OriginUrl = %q", p.OriginUrl)
	}
	if p.DefaultBaseBranch != "main" {
		t.Errorf("DefaultBaseBranch = %q", p.DefaultBaseBranch)
	}
	if p.WorktreeBaseDir != "/home/user/.worktrees" {
		t.Errorf("WorktreeBaseDir = %q", p.WorktreeBaseDir)
	}
	if p.SetupScript == nil || *p.SetupScript != "make install" {
		t.Errorf("SetupScript = %v", p.SetupScript)
	}
	if !p.CanAutoMerge {
		t.Error("CanAutoMerge should be true")
	}
	if !p.CanAutoMergeDependabot {
		t.Error("CanAutoMergeDependabot should be true")
	}
	if !p.CanAutoRepair {
		t.Error("CanAutoRepair should be true")
	}
	if !p.ShouldArchiveSessionsAfterMerge {
		t.Error("ShouldArchiveSessionsAfterMerge should be true")
	}
	if p.CreatedAt == nil {
		t.Error("CreatedAt should not be nil")
	}
}

// TestRepoToRepoSettings_ArchiveAfterMerge proves the web-safe settings
// projection carries archive_sessions_after_merge so cloud/web repo settings can
// read and toggle it (mirrors the can_auto_repair projection).
func TestRepoToRepoSettings_ArchiveAfterMerge(t *testing.T) {
	on := repoToRepoSettings(&models.Repo{ID: "r", ShouldArchiveSessionsAfterMerge: true})
	if !on.GetShouldArchiveSessionsAfterMerge() {
		t.Error("ShouldArchiveSessionsAfterMerge should project true")
	}
	off := repoToRepoSettings(&models.Repo{ID: "r", ShouldArchiveSessionsAfterMerge: false})
	if off.GetShouldArchiveSessionsAfterMerge() {
		t.Error("ShouldArchiveSessionsAfterMerge should project false")
	}
}

func TestRepoToProto_NilSetupScript(t *testing.T) {
	repo := &models.Repo{
		ID:          "repo-2",
		DisplayName: "no-script",
	}
	p := repoToProto(repo)
	if p.SetupScript != nil {
		t.Errorf("SetupScript should be nil, got %v", p.SetupScript)
	}
}

func TestSessionToProto(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	agentSessionID := "claude-123"
	prNum := 42
	prURL := "https://github.com/owner/repo/pull/42"
	blocked := "CI failed"
	acct := "a1"

	sess := &models.Session{
		ID:                  "sess-1",
		RepoID:              "repo-1",
		Title:               "Fix bug",
		Plan:                "Fix the thing",
		WorktreePath:        "/tmp/wt",
		BranchName:          "fix-bug",
		BaseBranch:          "main",
		State:               machine.ImplementingPlan,
		AgentSessionID:      &agentSessionID,
		AgentName:           "codex",
		PRNumber:            &prNum,
		PRURL:               &prURL,
		LastCheckState:      machine.CheckStatePassed,
		IsAutomationEnabled: true,
		AttemptCount:        3,
		BlockedReason:       &blocked,
		AccountID:           &acct,
		ArchivedAt:          &now,
		CreatedAt:           now,
		UpdatedAt:           now,
	}

	p := SessionToProto(sess)
	if p.Id != "sess-1" {
		t.Errorf("Id = %q", p.Id)
	}
	if p.RepoId != "repo-1" {
		t.Errorf("RepoId = %q", p.RepoId)
	}
	if p.Title != "Fix bug" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.AgentSessionId == nil || *p.AgentSessionId != "claude-123" {
		t.Errorf("AgentSessionId = %v", p.AgentSessionId)
	}
	if p.AgentName != "codex" {
		t.Errorf("AgentName = %q", p.AgentName)
	}
	if p.PrNumber == nil || *p.PrNumber != 42 {
		t.Errorf("PrNumber = %v", p.PrNumber)
	}
	if p.PrUrl == nil || *p.PrUrl != prURL {
		t.Errorf("PrUrl = %v", p.PrUrl)
	}
	if p.BlockedReason == nil || *p.BlockedReason != "CI failed" {
		t.Errorf("BlockedReason = %v", p.BlockedReason)
	}
	if p.ArchivedAt == nil {
		t.Error("ArchivedAt should not be nil")
	}
	if !p.IsAutomationEnabled {
		t.Error("IsAutomationEnabled should be true")
	}
	if p.AttemptCount != 3 {
		t.Errorf("AttemptCount = %d, want 3", p.AttemptCount)
	}
	if p.AccountId == nil || *p.AccountId != "a1" {
		t.Errorf("AccountId = %v, want a1", p.AccountId)
	}
}

func TestSessionToProto_NilOptionals(t *testing.T) {
	sess := &models.Session{
		ID:     "sess-2",
		RepoID: "repo-1",
		State:  machine.CreatingWorktree,
	}
	p := SessionToProto(sess)
	if p.AgentSessionId != nil {
		t.Errorf("AgentSessionId should be nil")
	}
	if p.PrNumber != nil {
		t.Errorf("PrNumber should be nil")
	}
	if p.PrUrl != nil {
		t.Errorf("PrUrl should be nil")
	}
	if p.BlockedReason != nil {
		t.Errorf("BlockedReason should be nil")
	}
	if p.ArchivedAt != nil {
		t.Errorf("ArchivedAt should be nil")
	}
	if p.AccountId != nil {
		t.Errorf("AccountId should be nil")
	}
}

func TestClaudeChatToProto(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	providerSessionID := "provider-resume-1"
	chat := &models.AgentChat{
		ID:                "chat-1",
		SessionID:         "sess-1",
		AgentSessionID:    "claude-abc",
		ProviderSessionID: &providerSessionID,
		Title:             "Chat title",
		DaemonID:          "daemon-1",
		CreatedAt:         now,
	}

	p := agentChatToProto(chat)
	if p.Id != "chat-1" {
		t.Errorf("Id = %q", p.Id)
	}
	if p.SessionId != "sess-1" {
		t.Errorf("SessionId = %q", p.SessionId)
	}
	if p.AgentSessionId != "claude-abc" {
		t.Errorf("AgentSessionId = %q", p.AgentSessionId)
	}
	if p.Title != "Chat title" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.DaemonId != "daemon-1" {
		t.Errorf("DaemonId = %q", p.DaemonId)
	}
	if p.ProviderSessionId != providerSessionID {
		t.Errorf("ProviderSessionId = %q, want %q", p.ProviderSessionId, providerSessionID)
	}
	if p.CreatedAt == nil {
		t.Error("CreatedAt should not be nil")
	}
}

func TestAgentChatToProtoSanitizesInvalidUTF8(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	startError := "send plan failed: " + string([]byte{0xff, 0xfe}) + " not seen"
	chat := &models.AgentChat{
		ID:             "chat-1",
		SessionID:      "sess-1",
		AgentSessionID: "claude-abc",
		Title:          "Repair chat",
		StartError:     &startError,
		CreatedAt:      now,
	}

	p := agentChatToProto(chat)
	if _, err := goproto.Marshal(p); err != nil {
		t.Fatalf("marshal sanitized chat: %v", err)
	}
	if p.StartError == startError {
		t.Fatal("StartError was not sanitized")
	}
}

func TestSessionToProtoSanitizesRepairErrors(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	sess := &models.Session{
		ID:                    "sess-1",
		RepoID:                "repo-1",
		Title:                 "Repair target",
		CreatedAt:             now,
		UpdatedAt:             now,
		LastRepairRunnerError: string([]byte{'r', 'u', 'n', 0xff}),
	}

	p := SessionToProto(sess)
	if _, err := goproto.Marshal(p); err != nil {
		t.Fatalf("marshal sanitized session: %v", err)
	}
	if p.LastRepairRunnerError == sess.LastRepairRunnerError {
		t.Fatal("LastRepairRunnerError was not sanitized")
	}
}

func TestProtoToTimestamp(t *testing.T) {
	t.Run("nil input", func(t *testing.T) {
		got := protoToTimestamp(nil)
		if got != nil {
			t.Errorf("expected nil, got %v", got)
		}
	})

	t.Run("non-nil input", func(t *testing.T) {
		now := time.Date(2025, 6, 15, 12, 30, 0, 0, time.UTC)
		ts := timestamppb.New(now)
		got := protoToTimestamp(ts)
		if got == nil {
			t.Fatal("expected non-nil")
			return
		}
		if !got.Equal(now) {
			t.Errorf("got %v, want %v", *got, now)
		}
	})
}

func TestAttentionStatusToProto(t *testing.T) {
	now := time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC)

	t.Run("no attention needed returns nil", func(t *testing.T) {
		a := vcs.AttentionStatus{NeedsAttention: false}
		got := attentionStatusToProto(a)
		if got != nil {
			t.Errorf("expected nil, got %+v", got)
		}
	})

	t.Run("blocked max attempts", func(t *testing.T) {
		a := vcs.AttentionStatus{
			NeedsAttention: true,
			Reason:         vcs.AttentionReasonBlockedMaxAttempts,
			Summary:        "fix loop exhausted",
			Since:          now,
		}
		got := attentionStatusToProto(a)
		if got == nil {
			t.Fatal("expected non-nil")
			return
		}
		if !got.NeedsAttention {
			t.Error("NeedsAttention should be true")
		}
		if got.Reason != pb.AttentionReason_ATTENTION_REASON_BLOCKED_MAX_ATTEMPTS {
			t.Errorf("Reason = %v, want BLOCKED_MAX_ATTEMPTS", got.Reason)
		}
		if got.Summary != "fix loop exhausted" {
			t.Errorf("Summary = %q", got.Summary)
		}
		if got.Since == nil {
			t.Error("Since should not be nil")
		}
	})

	t.Run("review requested", func(t *testing.T) {
		a := vcs.AttentionStatus{
			NeedsAttention: true,
			Reason:         vcs.AttentionReasonReviewRequested,
			Summary:        "PR ready for human review",
			Since:          now,
		}
		got := attentionStatusToProto(a)
		if got == nil {
			t.Fatal("expected non-nil")
			return
		}
		if got.Reason != pb.AttentionReason_ATTENTION_REASON_REVIEW_REQUESTED {
			t.Errorf("Reason = %v, want REVIEW_REQUESTED", got.Reason)
		}
	})
}

// fakeSessionStore is a minimal SessionStore used only by cronJobStatus.
// Adapted from the cron package's scheduler_test.go fake.
type fakeSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*models.Session
	getErr   error // force every Get to return this error
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{sessions: map[string]*models.Session{}}
}

func (f *fakeSessionStore) put(sess *models.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[sess.ID] = sess
}

func (f *fakeSessionStore) Get(_ context.Context, id string) (*models.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	s, ok := f.sessions[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return s, nil
}

// Stub out the rest of SessionStore. cronJobStatus only calls Get.
func (f *fakeSessionStore) Create(_ context.Context, _ db.CreateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) List(_ context.Context, _ string) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListActive(_ context.Context, _ string) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListActiveWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListWithRepo(_ context.Context, _ string) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListByRepoAndPR(_ context.Context, _ string, _ int) ([]*db.SessionWithRepo, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	panic("not used")
}

// ListTmuxSessionNames satisfies db.SessionStore (BOS-846). No test in this
// package drives the orphaned-tmux reaper, so an empty whitelist is correct.
func (f *fakeSessionStore) ListTmuxSessionNames(_ context.Context) ([]string, error) {
	return nil, nil
}
func (f *fakeSessionStore) Update(_ context.Context, _ string, _ db.UpdateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) Archive(_ context.Context, _ string) error { panic("not used") }
func (f *fakeSessionStore) ResurrectToState(_ context.Context, _ string, _ int) (bool, error) {
	panic("not used")
}
func (f *fakeSessionStore) RollbackFailedResurrect(_ context.Context, _ string, _ time.Time, _, _ int) (bool, error) {
	panic("not used")
}
func (f *fakeSessionStore) Delete(_ context.Context, _ string) error { panic("not used") }
func (f *fakeSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	panic("not used")
}
func (f *fakeSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListByState(_ context.Context, _ int) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListByStates(_ context.Context, _ []int) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) UpdateStateConditionalFrom(_ context.Context, _ string, _ int, _ []int) (bool, error) {
	panic("not used")
}
func (f *fakeSessionStore) UpdateRepairDiagnostics(_ context.Context, _ db.UpdateRepairDiagnosticsParams) error {
	panic("not used")
}

func (f *fakeSessionStore) UpdateRepairBlocked(_ context.Context, _ string, _ time.Time, _ string) error {
	panic("not used")
}

// fakeActivity is a minimal cron.ActivityChecker for cronJobStatus tests. It
// mirrors the fake in internal/cron/scheduler_test.go: RunActive returns a fixed
// verdict regardless of the session, which is enough because each liveness-path
// case exercises exactly one session.
type fakeActivity struct{ active bool }

func (f fakeActivity) RunActive(_ *models.Session) bool { return f.active }

func TestCronJobStatus(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	sessID := "sess-active"
	archivedSessID := "sess-archived"
	mergedSessID := "sess-merged"
	closedSessID := "sess-closed"
	blockedSessID := "sess-blocked"
	missingSessID := "sess-missing"
	emptySessID := ""

	prFailed := models.CronJobOutcomePRFailed
	chatSpawnFailed := models.CronJobOutcomeChatSpawnFailed
	cleanupFailed := models.CronJobOutcomeCleanupFailed
	fireFailed := models.CronJobOutcomeFireFailed
	prCreated := models.CronJobOutcomePRCreated
	deletedNoChanges := models.CronJobOutcomeDeletedNoChanges
	prSkippedNoGitHub := models.CronJobOutcomePRSkippedNoGitHub
	failedRecovered := models.CronJobOutcomeFailedRecovered
	worktreeGone := models.CronJobOutcomeWorktreeGone
	zeroOutput := models.CronJobOutcomeZeroOutput

	// Seed the fake store with sessions in various lifecycle states.
	store := newFakeSessionStore()
	store.put(&models.Session{ID: sessID, State: machine.ImplementingPlan})
	store.put(&models.Session{ID: archivedSessID, State: machine.ImplementingPlan, ArchivedAt: &now})
	store.put(&models.Session{ID: mergedSessID, State: machine.Merged})
	store.put(&models.Session{ID: closedSessID, State: machine.Closed})
	store.put(&models.Session{ID: blockedSessID, State: machine.Blocked})

	// BOS-332: sessions in post-implement / strand states used to prove the
	// liveness path and the widened nil-activity fallback treat them as NOT
	// running.
	readyID := "sess-ready"
	pushingID := "sess-pushing"
	store.put(&models.Session{ID: readyID, State: machine.ReadyForReview})
	store.put(&models.Session{ID: pushingID, State: machine.PushingBranch})

	// The rest of the states the widened cronStatusInactiveState now excludes, so
	// the nil-activity fallback locks in every arm of the switch (a regression
	// dropping any one would otherwise slip through: the liveness path covers
	// production, but the fallback must stay correct too).
	orphanedID := "sess-orphaned"
	awaitingID := "sess-awaiting"
	fixingID := "sess-fixing"
	greenDraftID := "sess-greendraft"
	store.put(&models.Session{ID: orphanedID, State: machine.Orphaned})
	store.put(&models.Session{ID: awaitingID, State: machine.AwaitingChecks})
	store.put(&models.Session{ID: fixingID, State: machine.FixingChecks})
	store.put(&models.Session{ID: greenDraftID, State: machine.GreenDraft})

	// errStore returns an error from every Get — covers the lookup-error fall-through.
	errStore := newFakeSessionStore()
	errStore.getErr = errors.New("db down")

	tests := []struct {
		name     string
		job      *models.CronJob
		store    db.SessionStore
		activity cron.ActivityChecker // nil => legacy widened-fallback path
		want     pb.CronJobStatus
	}{
		{
			name:  "never run, no outcome -> IDLE",
			job:   &models.CronJob{},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "never run (empty session id), no outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &emptySessID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session active (not archived, not terminal) -> RUNNING",
			job:   &models.CronJob{LastRunSessionID: &sessID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_RUNNING,
		},
		{
			name:  "session active beats stale failure outcome -> RUNNING",
			job:   &models.CronJob{LastRunSessionID: &sessID, LastRunOutcome: &prFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_RUNNING,
		},
		{
			name:  "session archived, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &archivedSessID, LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session archived, success outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &archivedSessID, LastRunOutcome: &prCreated},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session merged, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &mergedSessID, LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session merged, success outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &mergedSessID, LastRunOutcome: &prCreated},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session closed, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &closedSessID, LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session closed, success outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &closedSessID, LastRunOutcome: &prCreated},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session blocked, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID, LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			// pr_failed is housekeeping, not a run failure; the Blocked
			// session carries attention-needed state instead of red cron.
			name:  "session blocked, pr_failed -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID, LastRunOutcome: &prFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session blocked, chat_spawn_failed -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID, LastRunOutcome: &chatSpawnFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session blocked, cleanup_failed -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID, LastRunOutcome: &cleanupFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session blocked, no outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session lookup not-found, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &missingSessID, LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session lookup error, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &sessID, LastRunOutcome: &fireFailed},
			store: errStore,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session lookup error, success outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &sessID, LastRunOutcome: &prCreated},
			store: errStore,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			// chat_spawn_failed: the PR was already created; janitorial, not a failed deliverable.
			name:  "outcome chat_spawn_failed -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &chatSpawnFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			// cleanup_failed: run completed but cleanup had an error; surfaces as attention, not FAILED.
			name:  "outcome cleanup_failed -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &cleanupFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "outcome fire_failed -> FAILED",
			job:   &models.CronJob{LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			// worktree_gone: finalize ran against a removed worktree; benign
			// no-op, not a run failure. Must paint IDLE, never red FAILED.
			name:  "outcome worktree_gone -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &worktreeGone},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "outcome zero_output -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &zeroOutput},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session blocked, worktree_gone -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID, LastRunOutcome: &worktreeGone},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		// chat_spawn_failed and cleanup_failed are post-PR janitorial steps;
		// they surface as session attention, not cron FAILED status.
		{
			name:  "archived, chat_spawn_failed (PR was created) -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &archivedSessID, LastRunOutcome: &chatSpawnFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "archived, cleanup_failed -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &archivedSessID, LastRunOutcome: &cleanupFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		// Each success/idle outcome -> IDLE.
		{
			name:  "outcome pr_created -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &prCreated},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "outcome deleted_no_changes -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &deletedNoChanges},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "outcome pr_skipped_no_github -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &prSkippedNoGitHub},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "outcome failed_recovered -> IDLE",
			job:   &models.CronJob{LastRunOutcome: &failedRecovered},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		// BOS-332 liveness path (activity non-nil): STATUS is RUNNING only when
		// the last-run agent is actively working, not merely because the row is
		// non-terminal.
		{
			name:     "liveness: ReadyForReview + RunActive false -> IDLE (not running)",
			job:      &models.CronJob{LastRunSessionID: &readyID},
			store:    store,
			activity: fakeActivity{active: false},
			want:     pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:     "liveness: PushingBranch strand + RunActive false -> IDLE (not running)",
			job:      &models.CronJob{LastRunSessionID: &pushingID},
			store:    store,
			activity: fakeActivity{active: false},
			want:     pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:     "liveness: ImplementingPlan + RunActive true -> RUNNING",
			job:      &models.CronJob{LastRunSessionID: &sessID},
			store:    store,
			activity: fakeActivity{active: true},
			want:     pb.CronJobStatus_CRON_JOB_STATUS_RUNNING,
		},
		{
			name:     "liveness: ImplementingPlan + RunActive false -> IDLE (not running)",
			job:      &models.CronJob{LastRunSessionID: &sessID},
			store:    store,
			activity: fakeActivity{active: false},
			want:     pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		// BOS-332 nil-activity fallback: widened cronStatusInactiveState excludes
		// ReadyForReview, so a non-terminal-but-not-implementing row is not
		// RUNNING; ImplementingPlan still is.
		{
			name:  "nil activity: ReadyForReview -> IDLE (widened inactive state)",
			job:   &models.CronJob{LastRunSessionID: &readyID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "nil activity: Orphaned -> IDLE (widened inactive state)",
			job:   &models.CronJob{LastRunSessionID: &orphanedID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "nil activity: AwaitingChecks -> IDLE (widened inactive state)",
			job:   &models.CronJob{LastRunSessionID: &awaitingID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "nil activity: FixingChecks -> IDLE (widened inactive state)",
			job:   &models.CronJob{LastRunSessionID: &fixingID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "nil activity: GreenDraft -> IDLE (widened inactive state)",
			job:   &models.CronJob{LastRunSessionID: &greenDraftID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "nil activity: ImplementingPlan -> RUNNING",
			job:   &models.CronJob{LastRunSessionID: &sessID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_RUNNING,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cronJobStatus(context.Background(), tt.job, tt.store, tt.activity)
			if got != tt.want {
				t.Errorf("cronJobStatus = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCronJobToProtoIncludesAgentName(t *testing.T) {
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	job := &models.CronJob{
		ID:        "cron-1",
		RepoID:    "repo-1",
		Name:      "Daily",
		Prompt:    "Run daily checks",
		Schedule:  "@daily",
		AgentName: "codex",
		IsEnabled: true,
		CreatedAt: now,
		UpdatedAt: now,
	}

	got := cronJobToProto(context.Background(), job, newFakeSessionStore(), nil, nil)

	if got.AgentName != "codex" {
		t.Fatalf("agent_name = %q, want codex", got.AgentName)
	}
}

func TestCronJobStatusGated(t *testing.T) {
	gatedOutcome := models.CronJobOutcomeGated
	job := &models.CronJob{LastRunOutcome: &gatedOutcome}
	got := cronJobStatus(context.Background(), job, newFakeSessionStore(), nil)
	if got != pb.CronJobStatus_CRON_JOB_STATUS_GATED {
		t.Fatalf("cronJobStatus with gated outcome = %v, want GATED", got)
	}
}

// TestCronJobStatusGateFailedVsGated is the BOS-881 status split: a gate that
// could not run derives FAILED (red, escalatable) while a gate that ran and
// said no keeps the healthy GATED status. No new CronJobStatus enum member is
// involved — gate_failed reuses CRON_JOB_STATUS_FAILED via isCronFailureOutcome.
func TestCronJobStatusGateFailedVsGated(t *testing.T) {
	tests := []struct {
		name    string
		outcome models.CronJobOutcome
		want    pb.CronJobStatus
	}{
		{"gate could not run", models.CronJobOutcomeGateFailed, pb.CronJobStatus_CRON_JOB_STATUS_FAILED},
		{"gate ran and said no", models.CronJobOutcomeGated, pb.CronJobStatus_CRON_JOB_STATUS_GATED},
		{"fire never reached a session", models.CronJobOutcomeFireFailed, pb.CronJobStatus_CRON_JOB_STATUS_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome := tt.outcome
			job := &models.CronJob{LastRunOutcome: &outcome}
			if got := cronJobStatus(context.Background(), job, newFakeSessionStore(), nil); got != tt.want {
				t.Fatalf("cronJobStatus(%q) = %v, want %v", tt.outcome, got, tt.want)
			}
		})
	}
}

// TestIsCronFailureOutcome pins the exact failure set so a later outcome
// addition has to make a deliberate choice rather than inherit one.
func TestIsCronFailureOutcome(t *testing.T) {
	failures := []models.CronJobOutcome{
		models.CronJobOutcomeFireFailed,
		models.CronJobOutcomeGateFailed,
	}
	notFailures := []models.CronJobOutcome{
		models.CronJobOutcomeGated,
		models.CronJobOutcomePRCreated,
		models.CronJobOutcomePRFailed,
		models.CronJobOutcomePRNoChanges,
		models.CronJobOutcomePRSkippedNoGitHub,
		models.CronJobOutcomeChatSpawnFailed,
		models.CronJobOutcomeCleanupFailed,
		models.CronJobOutcomeDeletedNoChanges,
		models.CronJobOutcomeFailedRecovered,
		models.CronJobOutcomeWorktreeGone,
		models.CronJobOutcomeZeroOutput,
	}
	for _, o := range failures {
		if !isCronFailureOutcome(o) {
			t.Errorf("isCronFailureOutcome(%q) = false, want true", o)
		}
	}
	for _, o := range notFailures {
		if isCronFailureOutcome(o) {
			t.Errorf("isCronFailureOutcome(%q) = true, want false", o)
		}
	}
}

func TestCronJobToProtoGatingOverride(t *testing.T) {
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	job := &models.CronJob{
		ID:        "job-1",
		RepoID:    "repo-1",
		Name:      "Gating",
		Prompt:    "check something",
		Schedule:  "@daily",
		CreatedAt: now,
		UpdatedAt: now,
	}

	// Job ID present in gating map → GATING.
	gating := map[string]struct{}{"job-1": {}}
	got := cronJobToProto(context.Background(), job, newFakeSessionStore(), nil, gating)
	if got.LastRunStatus != pb.CronJobStatus_CRON_JOB_STATUS_GATING {
		t.Fatalf("gating map present: LastRunStatus = %v, want GATING", got.LastRunStatus)
	}
}

func TestCronJobToProtoGatingBeatsStaleGated(t *testing.T) {
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	gatedOutcome := models.CronJobOutcomeGated
	job := &models.CronJob{
		ID:             "job-2",
		RepoID:         "repo-1",
		Name:           "Gating beats stale",
		Prompt:         "check",
		Schedule:       "@daily",
		LastRunOutcome: &gatedOutcome,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	// GATED outcome in DB, but job is currently mid-gate → GATING must win.
	gating := map[string]struct{}{"job-2": {}}
	got := cronJobToProto(context.Background(), job, newFakeSessionStore(), nil, gating)
	if got.LastRunStatus != pb.CronJobStatus_CRON_JOB_STATUS_GATING {
		t.Fatalf("gating beats stale gated: LastRunStatus = %v, want GATING", got.LastRunStatus)
	}
}

func TestCronJobToProtoRunningNotAffectedByGatingSet(t *testing.T) {
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	sessID := "sess-active"
	store := newFakeSessionStore()
	store.put(&models.Session{ID: sessID, State: machine.ImplementingPlan})

	job := &models.CronJob{
		ID:               "job-3",
		RepoID:           "repo-1",
		Name:             "Running",
		Prompt:           "check",
		Schedule:         "@daily",
		LastRunSessionID: &sessID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}

	// Job not in gating map → RUNNING from active session wins.
	gating := map[string]struct{}{"other-job": {}}
	got := cronJobToProto(context.Background(), job, store, nil, gating)
	if got.LastRunStatus != pb.CronJobStatus_CRON_JOB_STATUS_RUNNING {
		t.Fatalf("non-gating job with active session: LastRunStatus = %v, want RUNNING", got.LastRunStatus)
	}
}

func TestCronJobToProtoGateCommandAndRunSetupCommandRoundTrip(t *testing.T) {
	now := time.Date(2026, 6, 4, 10, 0, 0, 0, time.UTC)
	job := &models.CronJob{
		ID:                    "job-4",
		RepoID:                "repo-1",
		Name:                  "Gate fields",
		Prompt:                "check",
		Schedule:              "@daily",
		GateCommand:           "make gate-check",
		ShouldRunSetupCommand: true,
		CreatedAt:             now,
		UpdatedAt:             now,
	}

	got := cronJobToProto(context.Background(), job, newFakeSessionStore(), nil, nil)
	if got.GateCommand != "make gate-check" {
		t.Fatalf("GateCommand = %q, want %q", got.GateCommand, "make gate-check")
	}
	if !got.ShouldRunSetupCommand {
		t.Fatalf("ShouldRunSetupCommand = false, want true")
	}
}

// TestCronJobToProtoZeroOutputRoundTrip proves the model→proto conversion carries
// IsZeroOutput in both directions, so a hardcoded constant on either side cannot
// pass.
func TestCronJobToProtoZeroOutputRoundTrip(t *testing.T) {
	now := time.Date(2026, 7, 26, 10, 0, 0, 0, time.UTC)

	for _, tt := range []struct {
		name string
		want bool
	}{
		{"zero output enabled", true},
		{"zero output disabled", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			job := &models.CronJob{
				ID:           "job-zero-output",
				RepoID:       "repo-1",
				Name:         "Zero output",
				Prompt:       "check",
				Schedule:     "@daily",
				IsZeroOutput: tt.want,
				CreatedAt:    now,
				UpdatedAt:    now,
			}

			got := cronJobToProto(context.Background(), job, newFakeSessionStore(), nil, nil)
			if got.IsZeroOutput != tt.want {
				t.Fatalf("IsZeroOutput = %v, want %v", got.IsZeroOutput, tt.want)
			}
		})
	}
}

func TestIsSubdirOf(t *testing.T) {
	tests := []struct {
		name   string
		child  string
		parent string
		want   bool
	}{
		{"exact match", "/home/user/repo", "/home/user/repo", true},
		{"child directory", "/home/user/repo/src/main.go", "/home/user/repo", true},
		{"sibling", "/home/user/other", "/home/user/repo", false},
		{"unrelated paths", "/tmp/foo", "/home/user/repo", false},
		{"parent is prefix but not boundary", "/home/user/repo-extra", "/home/user/repo", false},
		{"child with trailing slash parent", "/home/user/repo/sub", "/home/user/repo/", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isSubdirOf(tt.child, tt.parent)
			if got != tt.want {
				t.Errorf("isSubdirOf(%q, %q) = %v, want %v", tt.child, tt.parent, got, tt.want)
			}
		})
	}
}

// TestMergeGateProtoEnumLockstep locks the invariant that vcs.MergeGate and the
// generated pb.MergeBlock_Gate enum share byte-identical integer values.
// displayEntryToMergeBlock casts vcs.MergeGate -> pb.MergeBlock_Gate by raw
// value, so reordering or inserting a value in either enum would silently
// misreport the gate on get_session/list_sessions while every Go-only test
// stayed green. This is the compile-adjacent guard the "keep in sync EXACTLY"
// comments only document.
func TestMergeGateProtoEnumLockstep(t *testing.T) {
	cases := []struct {
		vcsGate vcs.MergeGate
		pbGate  pb.MergeBlock_Gate
	}{
		{vcs.MergeGateUnspecified, pb.MergeBlock_GATE_UNSPECIFIED},
		{vcs.MergeGateNone, pb.MergeBlock_GATE_NONE},
		{vcs.MergeGateReview, pb.MergeBlock_GATE_REVIEW},
		{vcs.MergeGateCI, pb.MergeBlock_GATE_CI},
		{vcs.MergeGatePending, pb.MergeBlock_GATE_PENDING},
		{vcs.MergeGateConflict, pb.MergeBlock_GATE_CONFLICT},
		{vcs.MergeGateBaseSync, pb.MergeBlock_GATE_BASE_SYNC},
		{vcs.MergeGateDraft, pb.MergeBlock_GATE_DRAFT},
	}
	for _, c := range cases {
		if int32(c.vcsGate) != int32(c.pbGate) {
			t.Errorf("enum drift: vcs.MergeGate(%d) != pb.MergeBlock_Gate %s(%d)",
				c.vcsGate, c.pbGate, int32(c.pbGate))
		}
	}
}

// TestDisplayStatusProtoEnumLockstep locks the same invariant for the
// DisplayStatus enum, which displayEntryToMergeBlock also casts by raw value.
func TestDisplayStatusProtoEnumLockstep(t *testing.T) {
	cases := []struct {
		vcsStatus vcs.DisplayStatus
		pbStatus  pb.DisplayStatus
	}{
		{vcs.DisplayStatusUnspecified, pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED},
		{vcs.DisplayStatusIdle, pb.DisplayStatus_DISPLAY_STATUS_IDLE},
		{vcs.DisplayStatusChecking, pb.DisplayStatus_DISPLAY_STATUS_CHECKING},
		{vcs.DisplayStatusFailing, pb.DisplayStatus_DISPLAY_STATUS_FAILING},
		{vcs.DisplayStatusConflict, pb.DisplayStatus_DISPLAY_STATUS_CONFLICT},
		{vcs.DisplayStatusRejected, pb.DisplayStatus_DISPLAY_STATUS_REJECTED},
		{vcs.DisplayStatusPassing, pb.DisplayStatus_DISPLAY_STATUS_PASSING},
		{vcs.DisplayStatusMerged, pb.DisplayStatus_DISPLAY_STATUS_MERGED},
		{vcs.DisplayStatusClosed, pb.DisplayStatus_DISPLAY_STATUS_CLOSED},
		{vcs.DisplayStatusDraft, pb.DisplayStatus_DISPLAY_STATUS_DRAFT},
		{vcs.DisplayStatusApproved, pb.DisplayStatus_DISPLAY_STATUS_APPROVED},
		{vcs.DisplayStatusReview, pb.DisplayStatus_DISPLAY_STATUS_REVIEW},
	}
	for _, c := range cases {
		if int32(c.vcsStatus) != int32(c.pbStatus) {
			t.Errorf("enum drift: vcs.DisplayStatus(%d) != pb.DisplayStatus %s(%d)",
				c.vcsStatus, c.pbStatus, int32(c.pbStatus))
		}
	}
}

// TestDisplayEntryToMergeBlock covers the read-path hydration helper: nil entry
// -> nil, a review-blocked entry -> GATE_REVIEW with reviewers and detail, and a
// passing entry -> GATE_NONE.
func TestDisplayEntryToMergeBlock(t *testing.T) {
	if got := displayEntryToMergeBlock(nil); got != nil {
		t.Errorf("nil entry: got %v, want nil", got)
	}

	review := displayEntryToMergeBlock(&status.DisplayEntry{
		Status:              vcs.DisplayStatusRejected,
		HasChangesRequested: true,
		ChangesRequestedBy:  []string{"octocat"},
	})
	if review.GetGate() != pb.MergeBlock_GATE_REVIEW {
		t.Errorf("review entry gate = %v, want GATE_REVIEW", review.GetGate())
	}
	if review.GetDetail() == "" {
		t.Error("review entry detail is empty, want non-empty")
	}
	if got := review.GetBlockingReviewers(); len(got) != 1 || got[0] != "octocat" {
		t.Errorf("review entry blocking_reviewers = %v, want [octocat]", got)
	}
	if review.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_REJECTED {
		t.Errorf("review entry display_status = %v, want REJECTED", review.GetDisplayStatus())
	}

	passing := displayEntryToMergeBlock(&status.DisplayEntry{Status: vcs.DisplayStatusPassing})
	if passing.GetGate() != pb.MergeBlock_GATE_NONE {
		t.Errorf("passing entry gate = %v, want GATE_NONE", passing.GetGate())
	}
}

// TestHydrateDisplayEntry pins the single source of truth for stamping the
// in-memory display-tracker fields onto a Session proto — shared by the local
// GetSession/ListSessions RPCs and the reverse-stream projection that feeds the
// cloud/web read model. The DisplayStatus assertion is the one that keeps the
// web Merge button working: it gates on DISPLAY_STATUS_PASSING, and the web only
// ever sees this field via the reverse stream.
func TestHydrateDisplayEntry(t *testing.T) {
	// Nil guards: neither a nil proto nor a nil entry may panic.
	HydrateDisplayEntry(nil, &status.DisplayEntry{})
	if got := (&pb.Session{}); func() (panicked bool) {
		defer func() { panicked = recover() != nil }()
		HydrateDisplayEntry(got, nil)
		return
	}() {
		t.Fatal("HydrateDisplayEntry(p, nil) panicked, want no-op")
	}
	// A nil entry must leave DisplayStatus untouched (UNSPECIFIED).
	blank := &pb.Session{}
	HydrateDisplayEntry(blank, nil)
	if blank.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED {
		t.Errorf("nil entry: display_status = %v, want UNSPECIFIED", blank.GetDisplayStatus())
	}

	mergeable := true
	p := &pb.Session{}
	HydrateDisplayEntry(p, &status.DisplayEntry{
		Status:              vcs.DisplayStatusPassing,
		HasFailures:         true,
		HasChangesRequested: true,
		IsRepairing:         true,
		SettingUp:           true,
		Merging:             true,
		Archiving:           true,
		Mergeable:           &mergeable,
	})
	if p.GetDisplayStatus() != pb.DisplayStatus_DISPLAY_STATUS_PASSING {
		t.Errorf("display_status = %v, want PASSING", p.GetDisplayStatus())
	}
	if !p.GetDisplayHasFailures() || !p.GetDisplayHasChangesRequested() ||
		!p.GetDisplayIsRepairing() || !p.GetDisplaySettingUp() || !p.GetDisplayMerging() {
		t.Errorf("per-axis flags not all stamped: %+v", p)
	}
	if !p.GetArchivePending() {
		t.Errorf("archive_pending not stamped from entry Archiving=true: %+v", p)
	}
	if p.PrMergeable == nil || !p.GetPrMergeable() {
		t.Errorf("pr_mergeable = %v, want true", p.PrMergeable)
	}
	if p.GetMergeBlock() == nil {
		t.Error("merge_block not derived, want non-nil")
	}

	// An entry with Archiving=false must leave archive_pending false — the
	// steady-state (resurrected merged) case that must NOT show "Archiving…".
	pClear := &pb.Session{}
	HydrateDisplayEntry(pClear, &status.DisplayEntry{
		Status:    vcs.DisplayStatusMerged,
		Archiving: false,
	})
	if pClear.GetArchivePending() {
		t.Errorf("archive_pending = true for Archiving=false entry, want false")
	}
}

// TestClampInt32 is the BOS-413 boundary table-test for the package-local
// gosec-G115 clamp helper: normal values pass through; out-of-range int inputs
// clamp to the int32 extremes instead of wrapping. int32 range is
// [-2147483648, 2147483647].
func TestClampInt32(t *testing.T) {
	const (
		maxI32 = 2147483647
		minI32 = -2147483648
	)
	tests := []struct {
		name string
		in   int
		want int32
	}{
		{"normal", 42, 42},
		{"zero", 0, 0},
		{"max", maxI32, maxI32},
		{"min", minI32, minI32},
		{"clampsHigh", maxI32 + 1, maxI32},
		{"clampsLow", minI32 - 1, minI32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := clampInt32(tt.in); got != tt.want {
				t.Errorf("clampInt32(%d) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
