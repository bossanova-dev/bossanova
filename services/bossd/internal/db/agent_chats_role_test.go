package db

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/dbtest"
)

const (
	agentChatsRoleVersion    int64 = 20261015110000
	preAgentChatsRoleVersion int64 = 20261015100000
)

func TestAgentChatStoreRoundTripsRole(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	sessionID := seedChatSession(t, db)
	chats := NewAgentChatStore(db)

	for _, tc := range []struct {
		agentSessionID string
		role           models.AgentChatRole
	}{
		{"chat-build", models.AgentChatRoleBuild},
		{"chat-repair", models.AgentChatRoleRepair},
		{"chat-verify", models.AgentChatRoleVerify},
		{"chat-operator", models.AgentChatRoleOperator},
		{"chat-unknown", models.AgentChatRoleUnknown},
	} {
		created, err := chats.Create(ctx, CreateAgentChatParams{SessionID: sessionID, AgentSessionID: tc.agentSessionID, Title: "t", Role: tc.role})
		if err != nil {
			t.Fatalf("create %s: %v", tc.agentSessionID, err)
		}
		if created.Role != tc.role {
			t.Errorf("Create(%s).Role = %q, want %q", tc.agentSessionID, created.Role, tc.role)
		}
		got, err := chats.GetByAgentSessionID(ctx, tc.agentSessionID)
		if err != nil {
			t.Fatalf("get %s: %v", tc.agentSessionID, err)
		}
		if got.Role != tc.role {
			t.Errorf("GetByAgentSessionID(%s).Role = %q, want %q", tc.agentSessionID, got.Role, tc.role)
		}
	}

	var nulls int
	if err := db.QueryRow(`SELECT COUNT(*) FROM agent_chats WHERE role IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count NULL roles: %v", err)
	}
	if nulls != 1 {
		t.Errorf("%d rows store a NULL role, want 1: the unknown role is NULL, never ''", nulls)
	}

	// A title rewrite, which is what codex auto-naming does, leaves the role.
	if err := chats.UpdateTitleByAgentSessionID(ctx, "chat-repair", "Repair pull request"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, err := chats.GetByAgentSessionID(ctx, "chat-repair")
	if err != nil {
		t.Fatalf("get renamed: %v", err)
	}
	if got.Role != models.AgentChatRoleRepair {
		t.Errorf("renamed repair chat role = %q, want repair", got.Role)
	}
}

func TestAgentChatStoreMarkReclaimedIsNotAFailedStart(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	sessionID := seedChatSession(t, db)
	chats := NewAgentChatStore(db)
	if _, err := chats.Create(ctx, CreateAgentChatParams{SessionID: sessionID, AgentSessionID: "agent-1", Title: "Repair pull request", Role: models.AgentChatRoleRepair}); err != nil {
		t.Fatalf("create: %v", err)
	}
	tmuxName := "boss-x-agent-1"
	if err := chats.UpdateTmuxSessionName(ctx, "agent-1", &tmuxName); err != nil {
		t.Fatalf("set tmux: %v", err)
	}
	// A chat that once failed to start and was then resumed and replaced
	// must read as replaced only.
	if err := chats.MarkStartFailed(ctx, "agent-1", "earlier launch failure"); err != nil {
		t.Fatalf("mark failed: %v", err)
	}

	const reason = "auto-repair replacing idle chat after 5m4s of silence; threshold 5m0s"
	if err := chats.MarkReclaimed(ctx, "agent-1", reason); err != nil {
		t.Fatalf("mark reclaimed: %v", err)
	}
	got, err := chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ReclaimedAt == nil || got.ReclaimReason == nil || *got.ReclaimReason != reason {
		t.Fatalf("reclaimed_at %v, reason %v; want both recorded", got.ReclaimedAt, got.ReclaimReason)
	}
	if got.StartError != nil || got.TmuxSessionName != nil {
		t.Fatalf("start_error %v, tmux %v; a replaced chat is neither a failed start nor live", got.StartError, got.TmuxSessionName)
	}
	if !got.IsRetired() {
		t.Error("a replaced chat must read as retired so liveness and broadcast skip it")
	}
	routable, err := chats.ListRoutableChats(ctx)
	if err != nil {
		t.Fatalf("list routable: %v", err)
	}
	for _, c := range routable {
		if c.AgentSessionID == "agent-1" {
			t.Error("a replaced chat is listed as routable; like a failed start, it has no pane to reach")
		}
	}

	if err := chats.RebindResumedChat(ctx, "agent-1", RebindResumedChatParams{ClearStartError: true}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	got, err = chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get after resume: %v", err)
	}
	if got.ReclaimedAt != nil || got.ReclaimReason != nil || got.IsRetired() {
		t.Fatalf("after a successful resume the replaced badge must clear; got reclaimed_at %v reason %v", got.ReclaimedAt, got.ReclaimReason)
	}

	if err := chats.MarkReclaimed(ctx, "no-such-chat", reason); !errors.Is(err, ErrAgentChatNotFound) {
		t.Fatalf("MarkReclaimed(unknown) err = %v, want ErrAgentChatNotFound", err)
	}
}

func TestAgentChatsRoleMigrationBackfillsOnlyCreatorTitles(t *testing.T) {
	db := dbtest.NewEmpty(t)
	fsys := os.DirFS(migrationsDir())
	if err := dbtest.RunUpTo(db, fsys, preAgentChatsRoleVersion); err != nil {
		t.Fatalf("migrate to pre-role schema: %v", err)
	}
	sessionID := seedChatSession(t, db)
	const insert = `INSERT INTO agent_chats (id, session_id, agent_session_id, agent_name, title, created_at)
		VALUES (?, ?, ?, 'codex', ?, datetime('now'))`
	rows := map[string]string{
		"repair":         "Repair: Add repo trust",
		"repair-renamed": "Repair pull request",
		"repair-lower":   "repair: lowercase",
		"verify":         "verify",
		"verify-padded":  " verify",
		"build":          "Build next planned ticket",
		"translated":     "再確認する PR #3100 とセッション状態",
	}
	for id, title := range rows {
		if _, err := db.Exec(insert, id, sessionID, id, title); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}

	if err := dbtest.RunUpTo(db, fsys, agentChatsRoleVersion); err != nil {
		t.Fatalf("apply role migration: %v", err)
	}

	want := map[string]models.AgentChatRole{
		"repair":         models.AgentChatRoleRepair,
		"repair-renamed": models.AgentChatRoleUnknown,
		"repair-lower":   models.AgentChatRoleUnknown,
		"verify":         models.AgentChatRoleVerify,
		"verify-padded":  models.AgentChatRoleUnknown,
		"build":          models.AgentChatRoleUnknown,
		"translated":     models.AgentChatRoleUnknown,
	}
	for id, role := range want {
		var stored *string
		if err := db.QueryRow(`SELECT role FROM agent_chats WHERE agent_session_id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		got := models.AgentChatRoleUnknown
		if stored != nil {
			got = models.AgentChatRole(*stored)
		}
		if got != role {
			t.Errorf("backfilled role of %q (title %q) = %q, want %q", id, rows[id], got, role)
		}
		if role == models.AgentChatRoleUnknown && stored != nil {
			t.Errorf("%q stores %q; an unknown role must stay NULL", id, *stored)
		}
	}

	if err := dbtest.RunDownTo(db, fsys, preAgentChatsRoleVersion); err != nil {
		t.Fatalf("roll back role migration: %v", err)
	}
	if _, err := db.Exec(insert, "after-down", sessionID, "after-down", "t"); err != nil {
		t.Fatalf("insert after down: %v", err)
	}
}
