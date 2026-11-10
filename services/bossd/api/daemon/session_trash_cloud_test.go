package server

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

func TestEmptyTrashCloudSkipsProtected_Sessions_AC238103(t *testing.T) {
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{
		"allowed":        {ID: "allowed", RepoID: "a", BranchName: "one", ArchivedAt: cleanupPtr(time.Now())},
		"SECRET-SKIPPED": {ID: "SECRET-SKIPPED", RepoID: "b", BranchName: "two", ArchivedAt: cleanupPtr(time.Now())}}}
	worktrees := &cleanupWorktreeManager{}
	path := t.TempDir()
	if err := os.Mkdir(filepath.Join(path, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	s := &Server{sessions: sessions, worktrees: worktrees, repos: &cleanupRepoStore{repos: map[string]*models.Repo{"a": {ID: "a", LocalPath: path}, "b": {ID: "b", LocalPath: path}}}}
	ctx := domain.WithCloudTarget(context.Background(), domain.CloudTarget{"repo:a": "a", "repo:b": "b"})
	ctx = domain.WithCloudRepoCheck(ctx, func(_ context.Context, id string) bool { return id == "a" })
	result, err := s.EmptyTrash(ctx, connect.NewRequest(&pb.EmptyTrashRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Msg.DeletedCount != 1 || sessions.deleted["SECRET-SKIPPED"] || !sessions.deleted["allowed"] {
		t.Fatalf("wrong partial deletion: count=%d", result.Msg.DeletedCount)
	}
	if worktrees.reapLocalBranchesCalls != 0 {
		t.Fatal("cleaned git storage shared with protected session")
	}
}

func TestEmptyTrashCloudRechecksBeforeCleanup(t *testing.T) {
	path := t.TempDir()
	if err := os.Mkdir(filepath.Join(path, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	sessions := &cleanupSessionStore{sessions: map[string]*models.Session{"allowed": {ID: "allowed", RepoID: "a", BranchName: "one", ArchivedAt: cleanupPtr(time.Now())}}}
	worktrees := &cleanupWorktreeManager{}
	s := &Server{sessions: sessions, worktrees: worktrees, repos: &cleanupRepoStore{repos: map[string]*models.Repo{"a": {ID: "a", LocalPath: path}}}}
	ctx := domain.WithCloudTarget(context.Background(), domain.CloudTarget{"repo:a": "a"})
	ctx = domain.WithCloudRepoCheck(ctx, func(context.Context, string) bool { return !sessions.deleted["allowed"] })
	result, err := s.EmptyTrash(ctx, connect.NewRequest(&pb.EmptyTrashRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Msg.DeletedCount != 1 || worktrees.reapLocalBranchesCalls != 0 {
		t.Fatal("cleanup ignored policy downgrade after deletion")
	}
}

func TestTrashStorageKeyResolvesLinkedCheckout(t *testing.T) {
	root := t.TempDir()
	common := filepath.Join(root, ".git")
	linked := t.TempDir()
	metadata := filepath.Join(common, "worktrees", "linked")
	if err := os.MkdirAll(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadata, "commondir"), []byte("../..\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, ".git"), []byte("gitdir: "+metadata+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := trashStorageKey(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := trashStorageKey(linked)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("linked checkout did not resolve shared git storage")
	}
}

func TestCloudChatDowngradeBeforeTyping_ChatPicker_AC238102(t *testing.T) {
	chat := &domain.AgentChat{ID: "c1", AgentSessionID: "agent-1", SessionID: "s1", AgentName: "claude"}
	sess := &models.Session{ID: "s1", RepoID: "r1", WorktreePath: t.TempDir(), AgentName: "claude"}
	tmuxer := &fakeTmuxClient{available: true, hasSession: true}
	s := newSendMessageTestServer(t, chat, sess, tmuxer)
	ctx := domain.WithCloudTarget(context.Background(), domain.CloudTarget{"repo:r1": "r1", "chat:agent-1": "r1", "session:s1": "r1"})
	reads := 0
	ctx = domain.WithCloudRepoCheck(ctx, func(context.Context, string) bool { reads++; return reads == 1 })
	_, err := s.SendChatMessage(ctx, connect.NewRequest(&pb.SendChatMessageRequest{AgentSessionId: "agent-1", Message: "hello", Submit: true}))
	if connect.CodeOf(err) != connect.CodePermissionDenied || sentCount(tmuxer) != 0 {
		t.Fatalf("typed after downgrade: err=%v sends=%d", err, sentCount(tmuxer))
	}
}
