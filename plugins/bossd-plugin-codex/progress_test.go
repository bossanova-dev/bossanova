package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestProbeProgressLivenessIsUnknownForCodex pins the deliberate scope choice
// for BOS-667: codex declines to answer the progress-liveness probe rather
// than guessing a phase from a rollout tail whose last line is routinely a
// bookkeeping envelope (token_count / turn_context / task_started) carrying no
// phase meaning. known=false is the fail-open answer the daemon raises nothing
// on, so codex sessions get no stall detection until the rollout protocol
// yields a real phase mapping.
//
// If a future change makes codex answer for real, this test SHOULD fail —
// replace it with the mapping's own coverage rather than deleting the intent.
func TestProbeProgressLivenessIsUnknownForCodex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	work := t.TempDir()

	// Write a rollout whose final record is a semantic one, so the assertion
	// cannot pass merely because there is nothing on disk to read.
	dir := filepath.Join(home, ".codex", "sessions", "2026", "08", "03")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionID := "11111111-2222-3333-4444-555555555555"
	rollout := `{"timestamp":"2026-08-03T15:11:02.114Z","type":"session_meta","payload":{"id":"` + sessionID + `"}}` + "\n" +
		`{"timestamp":"2026-08-03T15:12:38.921Z","type":"event_msg","payload":{"type":"user_message","message":"run the build"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-08-03T15-11-02-"+sessionID+".jsonl"), []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := &Server{logger: zerolog.Nop()}
	resp, err := srv.ProbeProgressLiveness(context.Background(), &bossanovav1.ProbeProgressLivenessRequest{
		WorkDir:        work,
		AgentSessionId: sessionID,
	})
	if err != nil {
		t.Fatalf("ProbeProgressLiveness should never error: %v", err)
	}
	if resp.IsKnown {
		t.Error("IsKnown = true, want false (codex declines to answer)")
	}
	if resp.Phase != bossanovav1.AgentProgressPhase_AGENT_PROGRESS_PHASE_UNKNOWN {
		t.Errorf("Phase = %v, want UNKNOWN", resp.Phase)
	}
	if resp.LastProgressAt != nil {
		t.Errorf("LastProgressAt = %v, want nil (never fabricate a timestamp)", resp.LastProgressAt)
	}
}
