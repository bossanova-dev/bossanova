package plugin_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/recurser/bossd/internal/adapter/dotenv"

	plugindoor "github.com/recurser/bossd/api/plugin"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/migrate"
	"github.com/recurser/bossd/internal/db"
	pluginpkg "github.com/recurser/bossd/internal/plugin"
	"github.com/recurser/bossd/internal/plugin/eventbus"
	"github.com/recurser/bossd/internal/plugin/pluginharness"
	"github.com/recurser/bossd/internal/status"
)

// branchTipProvider serves a movable base branch tip and records each read.
type branchTipProvider struct {
	testVCSProvider

	tipMu sync.Mutex
	tip   string
	reads []string
}

func (p *branchTipProvider) GetBranchHeadSHA(_ context.Context, repoPath, branch string) (string, error) {
	p.tipMu.Lock()
	defer p.tipMu.Unlock()
	p.reads = append(p.reads, repoPath+"@"+branch)
	return p.tip, nil
}

func (p *branchTipProvider) setTip(sha string) {
	p.tipMu.Lock()
	defer p.tipMu.Unlock()
	p.tip = sha
}

func (p *branchTipProvider) readCount() int {
	p.tipMu.Lock()
	defer p.tipMu.Unlock()
	return len(p.reads)
}

// TestRepairPluginRetriesConflictWhenBaseMoves runs the #3116 incident through
// the real daemon: SQLite stores, the real HostService, and the built
// bossd-plugin-repair binary. A conflicted PR has one failed repair recorded
// against base tip base-1. While the tip stays put the plugin skips it; once
// the base moves it tries again. The host here has no agent runner, so that
// fresh attempt lands as a start refusal in the blocked lane, which is the
// observable proof the duplicate guard let it through.
func TestRepairPluginRetriesConflictWhenBaseMoves(t *testing.T) {
	ctx := t.Context()
	sqlDB, err := db.OpenInMemory()
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := migrate.Run(sqlDB, os.DirFS(pluginharness.MigrationsDir())); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	repos := db.NewRepoStore(sqlDB)
	sessions := db.NewSessionStore(sqlDB)
	chats := db.NewAgentChatStore(sqlDB)

	repo, err := repos.Create(ctx, db.CreateRepoParams{
		DisplayName:       "base-move",
		LocalPath:         "/tmp/base-move",
		OriginURL:         "https://github.com/owner/base-move.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/base-move-worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	prNumber := 3116
	sess, err := sessions.Create(ctx, db.CreateSessionParams{
		RepoID:       repo.ID,
		Title:        "conflicted on a moving base",
		WorktreePath: "/tmp/base-move-worktrees/feature",
		BranchName:   "feature",
		BaseBranch:   "main",
		PRNumber:     &prNumber,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	state := int(machine.AwaitingChecks)
	if _, err := sessions.Update(ctx, sess.ID, db.UpdateSessionParams{State: &state}); err != nil {
		t.Fatalf("seed session state: %v", err)
	}
	if err := sessions.UpdateRepairDiagnostics(ctx, db.UpdateRepairDiagnosticsParams{
		SessionID:     sess.ID,
		StartedAt:     time.Now().Add(-2 * time.Hour),
		ExitError:     "exit status 1",
		HeadSHA:       "head-1",
		DisplayStatus: int32(bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT),
		BaseSHA:       "base-1",
	}); err != nil {
		t.Fatalf("seed failed repair: %v", err)
	}

	provider := &branchTipProvider{tip: "base-1"}
	display := status.NewDisplayTracker()
	display.Set(sess.ID, vcs.DisplayInfo{Status: vcs.DisplayStatusConflict, HeadSHA: "head-1"})
	bus := eventbus.New(zerolog.Nop())
	hostService := plugindoor.NewHostServiceServer(plugindoor.Config{EnvOverlay: dotenv.OverlayWithRepo, Provider: provider})
	host := pluginpkg.New(bus, hostService, zerolog.Nop())
	hostService.SetSessionDeps(repos, sessions, chats, display, status.NewTracker())
	t.Cleanup(func() { bus.Close() })
	t.Cleanup(func() {
		if err := host.Stop(); err != nil {
			t.Logf("host.Stop: %v", err)
		}
	})

	binPath := pluginharness.BuildPluginInto(t, t.TempDir(), "bossd-plugin-repair")
	if err := host.Start(ctx, []config.PluginConfig{{Name: "repair", Path: binPath, Enabled: true}}, config.Settings{}); err != nil {
		t.Fatalf("host.Start: %v", err)
	}
	host.SetWorkflowDesired("repair", &bossanovav1.StartWorkflowRequest{ConfigJson: `{"cooldown_minutes":1}`})
	armCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, _, err := host.EnsureWorkflowRunning(armCtx, "repair"); err != nil {
		t.Fatalf("EnsureWorkflowRunning: %v", err)
	}

	stored := func() *models.Session {
		t.Helper()
		got, err := sessions.Get(ctx, sess.ID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		return got
	}
	waitUntil := func(cond func() bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if cond() {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for: %s", msg)
	}

	host.NotifyStatusChange(ctx, sess.ID, vcs.DisplayStatusConflict, false)
	waitUntil(func() bool { return provider.readCount() > 0 },
		"the plugin reads the base tip of the session's base branch through the host")
	time.Sleep(500 * time.Millisecond)
	if got := stored().LastRepairBlockedReason; got != "" {
		t.Fatalf("blocked reason %q with the base unchanged; the failed repair's input must not be retried", got)
	}

	provider.setTip("base-2")
	host.NotifyStatusChange(ctx, sess.ID, vcs.DisplayStatusConflict, false)
	waitUntil(func() bool { return stored().LastRepairBlockedReason != "" },
		"a fresh repair attempt once the base branch moved")
	if got := stored().LastRepairBlockedReason; !strings.Contains(got, "not configured") {
		t.Fatalf("blocked reason = %q, want the host's agent-not-configured start refusal", got)
	}
	provider.tipMu.Lock()
	defer provider.tipMu.Unlock()
	if want := "https://github.com/owner/base-move.git@main"; provider.reads[0] != want {
		t.Fatalf("first base tip read = %q, want %q", provider.reads[0], want)
	}
}
