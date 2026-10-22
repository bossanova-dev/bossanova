package db

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/recurser/bossd/internal/domain"
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

	if err := chats.UpdateResumedChat(ctx, "agent-1", UpdateResumedChatParams{ClearStartError: true}); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	got, err = chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get after resume: %v", err)
	}
	if got.ReclaimedAt != nil || got.ReclaimReason != nil || got.IsRetired() {
		t.Fatalf("after a successful resume the replaced badge must clear; got reclaimed_at %v reason %v", got.ReclaimedAt, got.ReclaimReason)
	}

	if err := chats.MarkReclaimed(ctx, "no-such-chat", reason); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("MarkReclaimed(unknown) err = %v, want domain.ErrNotFound", err)
	}
}

// A callback wake or an operator attach relaunches a reclaimed chat by writing
// its new pane name. PR 3162's build chat was reclaimed at 04:33:24 and woken
// by a callback 21 seconds later; it rebased and pushed the PR while the chat
// list still called it replaced.
func TestAgentChatStoreRevivedChatIsNoLongerReplaced(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	sessionID := seedChatSession(t, db)
	chats := NewAgentChatStore(db)
	if _, err := chats.Create(ctx, CreateAgentChatParams{SessionID: sessionID, AgentSessionID: "agent-1", Title: "Run bs-build", Role: models.AgentChatRoleBuild}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := chats.MarkReclaimed(ctx, "agent-1", "auto-repair replacing idle chat"); err != nil {
		t.Fatalf("mark reclaimed: %v", err)
	}
	if err := chats.UpdateTmuxSessionName(ctx, "agent-1", nil); err != nil {
		t.Fatalf("clear tmux: %v", err)
	}
	got, err := chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ReclaimedAt == nil || got.ReclaimReason == nil {
		t.Fatalf("clearing the pane pointer erased the replaced mark: reclaimed_at %v reason %v", got.ReclaimedAt, got.ReclaimReason)
	}

	pane := "boss-x-agent-1"
	if err := chats.UpdateTmuxSessionName(ctx, "agent-1", &pane); err != nil {
		t.Fatalf("set tmux: %v", err)
	}
	got, err = chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get revived: %v", err)
	}
	if got.ReclaimedAt != nil || got.ReclaimReason != nil || got.IsRetired() {
		t.Fatalf("a chat with a live pane reads as replaced: reclaimed_at %v reason %v", got.ReclaimedAt, got.ReclaimReason)
	}
	if got.TmuxSessionName == nil || *got.TmuxSessionName != pane {
		t.Fatalf("tmux_session_name = %v, want %q", got.TmuxSessionName, pane)
	}
	routable, err := chats.ListRoutableChats(ctx)
	if err != nil {
		t.Fatalf("list routable: %v", err)
	}
	if !slices.ContainsFunc(routable, func(c *models.AgentChat) bool { return c.AgentSessionID == "agent-1" }) {
		t.Error("a revived chat is not routable")
	}
}

// A chat whose start was stamped failed (a daemon restart cut the run short)
// and that an operator attach or a callback wake then relaunched kept
// start_error, so the chat table read "× failed" over a working chat.
func TestAgentChatStoreRelaunchedFailedChatIsNoLongerFailed(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	sessionID := seedChatSession(t, db)
	chats := NewAgentChatStore(db)
	if _, err := chats.Create(ctx, CreateAgentChatParams{SessionID: sessionID, AgentSessionID: "agent-1", Title: "factory improvements"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := chats.MarkStartFailed(ctx, "agent-1", "daemon restarted mid-run"); err != nil {
		t.Fatalf("mark start failed: %v", err)
	}
	got, err := chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.StartError == nil || !got.IsRetired() {
		t.Fatalf("a failed start did not retire the chat: start_error %v", got.StartError)
	}

	pane := "boss-x-agent-1"
	if err := chats.UpdateTmuxSessionName(ctx, "agent-1", &pane); err != nil {
		t.Fatalf("set tmux: %v", err)
	}
	got, err = chats.GetByAgentSessionID(ctx, "agent-1")
	if err != nil {
		t.Fatalf("get relaunched: %v", err)
	}
	if got.StartError != nil || got.IsRetired() {
		t.Fatalf("a chat with a live pane reads as failed: start_error %v", got.StartError)
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

// Reclaiming a chat cancels every watch it has not finished, whatever stage
// the watch is at, so a later relaunch that clears the mark cannot revive one.
// A merge watch on the session's own PR survives, as do watches of other chats
// and watches already delivered.
func TestAgentChatStoreMarkReclaimedCancelsTheChatsWatches(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	sessionID := seedChatSession(t, db)
	if _, err := db.Exec(`UPDATE sessions SET pr_number = 7 WHERE id = ?`, sessionID); err != nil {
		t.Fatalf("set pr number: %v", err)
	}
	chats := NewAgentChatStore(db)
	for _, id := range []string{"build", "repair"} {
		if _, err := chats.Create(ctx, CreateAgentChatParams{SessionID: sessionID, AgentSessionID: id, Title: id, Role: models.AgentChatRoleBuild}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	callbacks := NewGitHubCallbackStore(db)
	watch := func(chat string, pr int, trigger models.GitHubCallbackTrigger) string {
		t.Helper()
		cb, _, err := callbacks.Create(ctx, CreateGitHubCallbackParams{TargetChatID: chat, RepoOwner: "test", RepoName: "repo", PRNumber: pr, Trigger: trigger, Message: "watch", IndependentWatch: true})
		if err != nil {
			t.Fatalf("create watch: %v", err)
		}
		return cb.ID
	}
	now := time.Now()
	active := watch("build", 7, models.GitHubCallbackTriggerChecksFailed)
	triggered := watch("build", 7, models.GitHubCallbackTriggerChecksPassed)
	if _, err := callbacks.MarkGroupTriggered(ctx, triggered, "checks_passed", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	leased := watch("build", 7, models.GitHubCallbackTriggerReadyForReview)
	if _, err := callbacks.MarkGroupTriggered(ctx, leased, "ready_for_review", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := callbacks.SetLease(ctx, leased, "worker", now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	delivered := watch("build", 7, models.GitHubCallbackTriggerClosed)
	if _, err := callbacks.MarkGroupTriggered(ctx, delivered, "closed", now); err != nil {
		t.Fatalf("trigger: %v", err)
	}
	if _, err := callbacks.SetLease(ctx, delivered, "worker", now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := callbacks.MarkDelivered(ctx, delivered, "worker", now); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	ownMerge := watch("build", 7, models.GitHubCallbackTriggerMerged)
	otherMerge := watch("build", 8, models.GitHubCallbackTriggerMerged)
	repair := watch("repair", 7, models.GitHubCallbackTriggerChecksFailed)

	if err := chats.MarkReclaimed(ctx, "build", "auto-repair replacing idle chat"); err != nil {
		t.Fatalf("mark reclaimed: %v", err)
	}
	pane := "boss-x-build"
	if err := chats.UpdateTmuxSessionName(ctx, "build", &pane); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	for id, want := range map[string]models.GitHubCallbackState{
		active: models.GitHubCallbackStateCanceled, triggered: models.GitHubCallbackStateCanceled, leased: models.GitHubCallbackStateCanceled, otherMerge: models.GitHubCallbackStateCanceled,
		delivered: models.GitHubCallbackStateDelivered, ownMerge: models.GitHubCallbackStateActive, repair: models.GitHubCallbackStateActive,
	} {
		got, err := callbacks.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.State != want {
			t.Errorf("%s watch on %s PR %d = %s, want %s", got.Trigger, got.TargetChatID, got.PRNumber, got.State, want)
		}
		if want == models.GitHubCallbackStateCanceled && (got.LastEvent == nil || *got.LastEvent != ReclaimedCallbackEvent) {
			t.Errorf("%s watch last_event = %v, want %q", got.Trigger, got.LastEvent, ReclaimedCallbackEvent)
		}
	}

	if err := chats.MarkReclaimed(ctx, "missing", "x"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("reclaim of a missing chat = %v, want domain.ErrNotFound", err)
	}
}

// A reclaim spares only the merge watch on the session's own PR: acme/widgets#7
// for a session on acme/widgets, whatever case its origin URL spells. A merge watch
// on other/lib#7 shares the number but not the repository, so it is canceled;
// otherwise the relaunch that clears the mark would let it wake the chat.
func TestAgentChatStoreMarkReclaimedSparesOnlyItsOwnRepositorysMerge(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	sessionID := seedChatSession(t, db)
	if _, err := db.Exec(`UPDATE repos SET origin_url = 'git@github.com:Acme/Widgets.git'`); err != nil {
		t.Fatalf("set origin: %v", err)
	}
	if _, err := db.Exec(`UPDATE sessions SET pr_number = 7 WHERE id = ?`, sessionID); err != nil {
		t.Fatalf("set pr number: %v", err)
	}
	chats := NewAgentChatStore(db)
	if _, err := chats.Create(ctx, CreateAgentChatParams{SessionID: sessionID, AgentSessionID: "build", Title: "build", Role: models.AgentChatRoleBuild}); err != nil {
		t.Fatalf("create: %v", err)
	}
	callbacks := NewGitHubCallbackStore(db)
	want := map[string]models.GitHubCallbackState{}
	for _, w := range []struct {
		owner, name string
		state       models.GitHubCallbackState
	}{
		{"acme", "widgets", models.GitHubCallbackStateActive},
		{"other", "lib", models.GitHubCallbackStateCanceled},
		{"acme", "lib", models.GitHubCallbackStateCanceled},
		{"other", "widgets", models.GitHubCallbackStateCanceled},
	} {
		cb, _, err := callbacks.Create(ctx, CreateGitHubCallbackParams{TargetChatID: "build", RepoOwner: w.owner, RepoName: w.name, PRNumber: 7, Trigger: models.GitHubCallbackTriggerMerged, Message: "watch", IndependentWatch: true})
		if err != nil {
			t.Fatalf("create watch on %s/%s: %v", w.owner, w.name, err)
		}
		want[cb.ID] = w.state
	}
	if err := chats.MarkReclaimed(ctx, "build", "auto-repair replacing idle chat"); err != nil {
		t.Fatalf("mark reclaimed: %v", err)
	}
	for id, state := range want {
		got, err := callbacks.Get(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.State != state {
			t.Errorf("merge watch on %s/%s#%d = %s, want %s", got.RepoOwner, got.RepoName, got.PRNumber, got.State, state)
		}
	}
}
