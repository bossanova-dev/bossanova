package testharness_test

// e2e_orphan_tmux_reap_test.go — proves the BOS-846 reaper end-to-end against a
// real SQLite store: a pane whose chat row has been hard-deleted is reported by
// a dry-run sweep and left alive, then killed by an armed sweep once its
// confirmation strike ages out — while the pane of a chat that still has a row
// survives every sweep.
//
// The unit suite in internal/status drives the classifier against mocks. This
// test exists because the classifier's whitelist is only as good as the queries
// behind it, and those are the real ones here.

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/status"
	"github.com/recurser/bossd/internal/testharness"
	"github.com/recurser/bossd/internal/tmux"
	"github.com/rs/zerolog"
)

const (
	reapE2EDaemonID = "reaper-e2e-daemon"
	// Agent session ids are hex so the derived pane names match the
	// boss-<8hex>-<8hex> shape the reaper's ownership gate requires.
	reapE2ELiveAgentID   = "aaaaaaaa11112222"
	reapE2EOrphanAgentID = "bbbbbbbb33334444"
	// Small enough to keep the test quick, large enough that the sleep below
	// clears it with room to spare.
	reapE2EGrace = time.Second
)

// newReapE2EReaper builds a reaper over the harness's real stores and tmux
// client. Nothing is stubbed: a reap here runs the production KillSession path,
// which the fake records as a `kill-session` call instead of executing.
func newReapE2EReaper(h *testharness.Harness, logs *bytes.Buffer, dryRun bool) *status.TmuxReaper {
	enabled, dry, reapUnstamped := true, dryRun, false
	cfg := config.TmuxReaperConfig{
		Enabled:              &enabled,
		DryRun:               &dry,
		ReapUnstamped:        &reapUnstamped,
		SweepIntervalSeconds: 1,
		GracePeriodSeconds:   int(reapE2EGrace / time.Second),
	}
	return status.NewTmuxReaper(h.AgentChats, h.Sessions, h.Repos, h.Tmux,
		reapE2EDaemonID, cfg, zerolog.New(logs).Level(zerolog.TraceLevel))
}

func TestOrphanedTmuxPaneIsReapedAndLiveChatPaneIsNot(t *testing.T) {
	fake := testharness.NewCronReadyTmuxFake()
	h := testharness.NewWithOptions(t, testharness.Options{
		TmuxCommandFactory: fake.Factory(),
		TmuxDaemonID:       reapE2EDaemonID,
	})
	ctx := h.Ctx()

	repoID := h.SeedRepo(t, "https://github.com/test/orphan-reap.git")
	sessionID := h.SeedSession(t, repoID, 846, pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN)

	livePane := tmux.ChatSessionName(repoID, reapE2ELiveAgentID)
	orphanPane := tmux.ChatSessionName(repoID, reapE2EOrphanAgentID)

	for _, spec := range []struct{ agentID, pane string }{
		{reapE2ELiveAgentID, livePane},
		{reapE2EOrphanAgentID, orphanPane},
	} {
		if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
			SessionID:      sessionID,
			AgentSessionID: spec.agentID,
			AgentName:      "claude",
			Title:          "reaper e2e " + spec.agentID,
		}); err != nil {
			t.Fatalf("create chat %s: %v", spec.agentID, err)
		}
		if err := h.AgentChats.UpdateTmuxSessionName(ctx, spec.agentID, &spec.pane); err != nil {
			t.Fatalf("record tmux name for %s: %v", spec.agentID, err)
		}
		if err := h.Tmux.NewSession(ctx, tmux.NewSessionOpts{
			Name: spec.pane, WorkDir: t.TempDir(), Command: []string{"true"},
		}); err != nil {
			t.Fatalf("create tmux session %s: %v", spec.pane, err)
		}
		// Both panes start older than the grace period; only the strike
		// clock below needs real elapsed time.
		fake.AgeSession(spec.pane, 5*time.Minute)
	}
	t.Cleanup(func() {
		_ = h.Tmux.KillSession(context.Background(), livePane)
		_ = h.Tmux.KillSession(context.Background(), orphanPane)
	})

	// Precondition: the daemon stamp is what lets the reaper claim the pane at
	// all. Without it every pane below lands in `unattributable` and the reap
	// assertions would fail for the wrong reason.
	if got := fake.SessionEnv(orphanPane)[tmux.DaemonIDEnvKey]; got != reapE2EDaemonID {
		t.Fatalf("%s = %q on the created pane, want %q", tmux.DaemonIDEnvKey, got, reapE2EDaemonID)
	}

	// The orphaning event: the row goes away while the pane keeps running,
	// exactly as it does when bossd is SIGKILLed mid-teardown.
	if err := h.AgentChats.DeleteByAgentSessionID(ctx, reapE2EOrphanAgentID); err != nil {
		t.Fatalf("delete orphan chat row: %v", err)
	}

	// --- dry run: reported, never killed ---------------------------------
	var dryLogs bytes.Buffer
	dry := newReapE2EReaper(h, &dryLogs, true)
	for i := range 3 {
		if n, err := dry.SweepOnce(ctx); err != nil || n != 0 {
			t.Fatalf("dry-run sweep %d: reaped %d, err %v; want 0, nil", i, n, err)
		}
		time.Sleep(reapE2EGrace + 100*time.Millisecond)
	}
	if !fake.HasLiveSession(orphanPane) {
		t.Fatal("dry run killed the orphaned pane")
	}
	if fake.HasCall("kill-session") {
		t.Fatalf("dry run issued kill-session; calls: %v", fake.Calls())
	}
	logs := dryLogs.String()
	for _, want := range []string{
		`"tmuxSession":"` + orphanPane + `"`,
		`"bucket":"reap"`,
		`"dryRun":true`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("dry-run log missing %s\nlogs:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `"tmuxSession":"`+livePane+`","bucket":"reap"`) {
		t.Fatalf("dry run proposed reaping the live chat's pane\nlogs:\n%s", logs)
	}

	// --- armed: one confirmation strike, then the kill --------------------
	var armedLogs bytes.Buffer
	armed := newReapE2EReaper(h, &armedLogs, false)

	if n, err := armed.SweepOnce(ctx); err != nil || n != 0 {
		t.Fatalf("first armed sweep: reaped %d, err %v; want 0, nil (unconfirmed)", n, err)
	}
	if !fake.HasLiveSession(orphanPane) {
		t.Fatal("the first armed sweep killed an unconfirmed candidate")
	}

	time.Sleep(reapE2EGrace + 100*time.Millisecond)

	n, err := armed.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("second armed sweep: %v", err)
	}
	if n != 1 {
		t.Fatalf("second armed sweep reaped %d, want 1\nlogs:\n%s", n, armedLogs.String())
	}
	if fake.HasLiveSession(orphanPane) {
		t.Fatal("the orphaned pane is still alive after a confirmed reap")
	}
	if !fake.HasCall("kill-session") {
		t.Fatalf("no kill-session was issued; calls: %v", fake.Calls())
	}

	// The live chat's pane survived every one of the five sweeps above.
	if !fake.HasLiveSession(livePane) {
		t.Fatalf("the live chat's pane was killed\nlogs:\n%s", armedLogs.String())
	}
	if chat, err := h.AgentChats.GetByAgentSessionID(ctx, reapE2ELiveAgentID); err != nil || chat == nil {
		t.Fatalf("live chat row missing after the sweeps: %v", err)
	}
}

// TestOrphanedTmuxPaneSurvivesWhenTheChatRowStillExists is the inverse control:
// with no row deleted, repeated armed sweeps must never kill anything. It is
// what would catch a whitelist regression that a green reap test cannot.
func TestOrphanedTmuxPaneSurvivesWhenTheChatRowStillExists(t *testing.T) {
	fake := testharness.NewCronReadyTmuxFake()
	h := testharness.NewWithOptions(t, testharness.Options{
		TmuxCommandFactory: fake.Factory(),
		TmuxDaemonID:       reapE2EDaemonID,
	})
	ctx := h.Ctx()

	repoID := h.SeedRepo(t, "https://github.com/test/orphan-reap-control.git")
	sessionID := h.SeedSession(t, repoID, 847, pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN)
	pane := tmux.ChatSessionName(repoID, reapE2ELiveAgentID)

	if _, err := h.AgentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: reapE2ELiveAgentID,
		AgentName:      "claude",
		Title:          "reaper e2e control",
	}); err != nil {
		t.Fatalf("create chat: %v", err)
	}
	// Deliberately NOT recording tmux_session_name: this is the StartTmuxChat
	// window in which the pane exists and the column is still NULL, which only
	// the recomputed whitelist leg covers.
	if err := h.Tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name: pane, WorkDir: t.TempDir(), Command: []string{"true"},
	}); err != nil {
		t.Fatalf("create tmux session: %v", err)
	}
	t.Cleanup(func() { _ = h.Tmux.KillSession(context.Background(), pane) })
	fake.AgeSession(pane, 5*time.Minute)

	var logs bytes.Buffer
	reaper := newReapE2EReaper(h, &logs, false)
	for i := range 3 {
		if n, err := reaper.SweepOnce(ctx); err != nil || n != 0 {
			t.Fatalf("sweep %d: reaped %d, err %v; want 0, nil\nlogs:\n%s", i, n, err, logs.String())
		}
		time.Sleep(reapE2EGrace + 100*time.Millisecond)
	}
	if !fake.HasLiveSession(pane) {
		t.Fatalf("a chat with a live row lost its pane\nlogs:\n%s", logs.String())
	}
	if fake.HasCall("kill-session") {
		t.Fatalf("kill-session issued against an accounted-for pane; calls: %v", fake.Calls())
	}
}
