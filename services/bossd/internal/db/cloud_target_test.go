package db

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/domain"
)

func TestCloudChatReloadChecksStoredParent(t *testing.T) {
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	session, err := NewSessionStore(database).Create(context.Background(), CreateSessionParams{RepoID: repo.ID, Title: "cloud target", WorktreePath: "/tmp/cloud-target", BranchName: "cloud-target", BaseBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	chats := NewAgentChatStore(database)
	chat, err := chats.Create(context.Background(), CreateAgentChatParams{SessionID: session.ID, AgentSessionID: "cloud-chat"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := domain.WithCloudTarget(context.Background(), domain.CloudTarget{"chat:" + chat.AgentSessionID: "previous-repo", "repo:previous-repo": "previous-repo"})
	if _, err := chats.GetByAgentSessionID(ctx, chat.AgentSessionID); !errors.Is(err, domain.ErrCloudTarget) {
		t.Fatalf("reloaded chat escaped authorization: %v", err)
	}
	if _, err := chats.GetByAgentSessionID(context.Background(), chat.AgentSessionID); err != nil {
		t.Fatalf("local read changed: %v", err)
	}
}

func TestCloudRankRespaceRejectsProtectedRepoAtomically(t *testing.T) {
	f := newListRankFixture(t)
	allowedRepo := f.repoID
	f.add(t, "a", time.Now())
	f.add(t, "c", time.Now())
	protected, err := NewRepoStore(f.db).Create(f.ctx, CreateRepoParams{
		DisplayName: "protected", LocalPath: "/tmp/protected-ranks",
		OriginURL: "https://github.com/test/protected.git", DefaultBaseBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	f.repoID = protected.ID
	f.add(t, "b", time.Now())
	f.setRank(t, "a", 100)
	f.setRank(t, "b", 101)
	f.setRank(t, "c", 102)
	sessions, err := f.store.ListActive(f.ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	rows := domain.SessionListRankRowsFrom(sessions)
	domain.SortSessionListRankRows(rows)
	move := domain.ComputeListRankMove(rows, 2, true)
	if !move.IsRespaced || len(move.Ranks) != 3 {
		t.Fatalf("expected a three-row respace: %+v", move)
	}
	before := f.rawRanks(t)
	ctx := domain.WithCloudTarget(f.ctx, domain.CloudTarget{"repo:" + allowedRepo: allowedRepo, "session:c": allowedRepo})
	if n, err := f.store.SetListRanks(ctx, move.Ranks); !errors.Is(err, domain.ErrCloudTarget) || n != 0 {
		t.Fatalf("unauthorized rank respace = %d, %v", n, err)
	}
	if after := f.rawRanks(t); !reflect.DeepEqual(before, after) {
		t.Fatalf("refused respace changed ranks: before=%v after=%v", before, after)
	}
	if n, err := f.store.SetListRanks(f.ctx, move.Ranks); err != nil || n != 3 {
		t.Fatalf("local rank respace = %d, %v", n, err)
	}
}
