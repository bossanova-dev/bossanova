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
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

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

func TestRepoToProto(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	script := "make install"

	repo := &models.Repo{
		ID:                      "repo-1",
		DisplayName:             "my-app",
		LocalPath:               "/home/user/my-app",
		OriginURL:               "https://github.com/user/my-app.git",
		DefaultBaseBranch:       "main",
		WorktreeBaseDir:         "/home/user/.worktrees",
		SetupScript:             &script,
		CanAutoMerge:            true,
		CanAutoMergeDependabot:  true,
		CanAutoAddressReviews:   false,
		CanAutoResolveConflicts: true,
		CreatedAt:               now,
		UpdatedAt:               now,
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
	if p.CanAutoAddressReviews {
		t.Error("CanAutoAddressReviews should be false")
	}
	if !p.CanAutoResolveConflicts {
		t.Error("CanAutoResolveConflicts should be true")
	}
	if p.CreatedAt == nil {
		t.Error("CreatedAt should not be nil")
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
	claudeID := "claude-123"
	prNum := 42
	prURL := "https://github.com/owner/repo/pull/42"
	blocked := "CI failed"

	sess := &models.Session{
		ID:                "sess-1",
		RepoID:            "repo-1",
		Title:             "Fix bug",
		Plan:              "Fix the thing",
		WorktreePath:      "/tmp/wt",
		BranchName:        "fix-bug",
		BaseBranch:        "main",
		State:             machine.ImplementingPlan,
		ClaudeSessionID:   &claudeID,
		PRNumber:          &prNum,
		PRURL:             &prURL,
		LastCheckState:    machine.CheckStatePassed,
		AutomationEnabled: true,
		AttemptCount:      3,
		BlockedReason:     &blocked,
		ArchivedAt:        &now,
		CreatedAt:         now,
		UpdatedAt:         now,
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
	if p.ClaudeSessionId == nil || *p.ClaudeSessionId != "claude-123" {
		t.Errorf("ClaudeSessionId = %v", p.ClaudeSessionId)
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
	if !p.AutomationEnabled {
		t.Error("AutomationEnabled should be true")
	}
	if p.AttemptCount != 3 {
		t.Errorf("AttemptCount = %d, want 3", p.AttemptCount)
	}
}

func TestSessionToProto_NilOptionals(t *testing.T) {
	sess := &models.Session{
		ID:     "sess-2",
		RepoID: "repo-1",
		State:  machine.CreatingWorktree,
	}
	p := SessionToProto(sess)
	if p.ClaudeSessionId != nil {
		t.Errorf("ClaudeSessionId should be nil")
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
}

func TestClaudeChatToProto(t *testing.T) {
	now := time.Date(2025, 1, 15, 12, 0, 0, 0, time.UTC)
	chat := &models.ClaudeChat{
		ID:        "chat-1",
		SessionID: "sess-1",
		ClaudeID:  "claude-abc",
		Title:     "Chat title",
		DaemonID:  "daemon-1",
		CreatedAt: now,
	}

	p := claudeChatToProto(chat)
	if p.Id != "chat-1" {
		t.Errorf("Id = %q", p.Id)
	}
	if p.SessionId != "sess-1" {
		t.Errorf("SessionId = %q", p.SessionId)
	}
	if p.ClaudeId != "claude-abc" {
		t.Errorf("ClaudeId = %q", p.ClaudeId)
	}
	if p.Title != "Chat title" {
		t.Errorf("Title = %q", p.Title)
	}
	if p.DaemonId != "daemon-1" {
		t.Errorf("DaemonId = %q", p.DaemonId)
	}
	if p.CreatedAt == nil {
		t.Error("CreatedAt should not be nil")
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
func (f *fakeSessionStore) ListArchived(_ context.Context, _ string) ([]*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) Update(_ context.Context, _ string, _ db.UpdateSessionParams) (*models.Session, error) {
	panic("not used")
}
func (f *fakeSessionStore) Archive(_ context.Context, _ string) error   { panic("not used") }
func (f *fakeSessionStore) Resurrect(_ context.Context, _ string) error { panic("not used") }
func (f *fakeSessionStore) Delete(_ context.Context, _ string) error    { panic("not used") }
func (f *fakeSessionStore) AdvanceOrphanedSessions(_ context.Context) (int64, error) {
	panic("not used")
}
func (f *fakeSessionStore) UpdateStateConditional(_ context.Context, _ string, _, _ int) (bool, error) {
	panic("not used")
}
func (f *fakeSessionStore) ListByState(_ context.Context, _ int) ([]*models.Session, error) {
	panic("not used")
}

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

	// Seed the fake store with sessions in various lifecycle states.
	store := newFakeSessionStore()
	store.put(&models.Session{ID: sessID, State: machine.ImplementingPlan})
	store.put(&models.Session{ID: archivedSessID, State: machine.ImplementingPlan, ArchivedAt: &now})
	store.put(&models.Session{ID: mergedSessID, State: machine.Merged})
	store.put(&models.Session{ID: closedSessID, State: machine.Closed})
	store.put(&models.Session{ID: blockedSessID, State: machine.Blocked})

	// errStore returns an error from every Get — covers the lookup-error fall-through.
	errStore := newFakeSessionStore()
	errStore.getErr = errors.New("db down")

	tests := []struct {
		name  string
		job   *models.CronJob
		store db.SessionStore
		want  pb.CronJobStatus
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
			job:   &models.CronJob{LastRunSessionID: &archivedSessID, LastRunOutcome: &prFailed},
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
			job:   &models.CronJob{LastRunSessionID: &mergedSessID, LastRunOutcome: &prFailed},
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
			job:   &models.CronJob{LastRunSessionID: &closedSessID, LastRunOutcome: &prFailed},
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
			// Blocked is the state FinalizeSession transitions a session to
			// after a failure outcome. STATUS must surface that as FAILED so
			// the user sees the failure rather than a frozen RUNNING row.
			name:  "session blocked, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID, LastRunOutcome: &chatSpawnFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session blocked, no outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &blockedSessID},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		{
			name:  "session lookup not-found, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &missingSessID, LastRunOutcome: &prFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session lookup error, failure outcome -> FAILED",
			job:   &models.CronJob{LastRunSessionID: &sessID, LastRunOutcome: &prFailed},
			store: errStore,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "session lookup error, success outcome -> IDLE",
			job:   &models.CronJob{LastRunSessionID: &sessID, LastRunOutcome: &prCreated},
			store: errStore,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_IDLE,
		},
		// Each failure outcome -> FAILED (when no active session).
		{
			name:  "outcome pr_failed -> FAILED",
			job:   &models.CronJob{LastRunOutcome: &prFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "outcome chat_spawn_failed -> FAILED",
			job:   &models.CronJob{LastRunOutcome: &chatSpawnFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "outcome cleanup_failed -> FAILED",
			job:   &models.CronJob{LastRunOutcome: &cleanupFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
		},
		{
			name:  "outcome fire_failed -> FAILED",
			job:   &models.CronJob{LastRunOutcome: &fireFailed},
			store: store,
			want:  pb.CronJobStatus_CRON_JOB_STATUS_FAILED,
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cronJobStatus(context.Background(), tt.job, tt.store)
			if got != tt.want {
				t.Errorf("cronJobStatus = %v, want %v", got, tt.want)
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
