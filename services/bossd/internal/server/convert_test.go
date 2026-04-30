package server

import (
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
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
