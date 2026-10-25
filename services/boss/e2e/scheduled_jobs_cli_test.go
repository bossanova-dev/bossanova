package e2e_test

import (
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestJourney_ScheduledJobs_AC1_Cron_Add(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(cliRepos()...))
	res := h.Run("cron", "add",
		"--repo", "repo-1",
		"--name", "my-job",
		"--schedule", "0 5 * * *",
		"--prompt", "do the thing",
		"--agent", "stub",
		"--gate", "make test",
		"--model", "opus",
		"--tz", "UTC",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := storedCLICronJobs(t, h)
	if len(calls) != 1 {
		t.Fatalf("expected 1 stored job, got %d", len(calls))
	}
	req := calls[0]
	if req.RepoId != h.RepoIDs["repo-1"] || req.Name != "my-job" || req.Schedule != "0 5 * * *" {
		t.Errorf("unexpected core fields: %+v", req)
	}
	if req.Prompt != "do the thing" || req.AgentName != "stub" || req.GateCommand != "make test" {
		t.Errorf("unexpected prompt/agent/gate: %+v", req)
	}
	if req.Model != "opus" || req.Timezone != "UTC" {
		t.Errorf("unexpected model/tz: %+v", req)
	}
	if !req.IsEnabled {
		t.Errorf("expected Enabled default true")
	}
	listed := h.Run("cron", "ls")
	if !listed.Success() {
		t.Fatalf("ls: %+v", listed)
	}
	for _, want := range []string{req.Id, "my-job", "stub"} {
		if !strings.Contains(listed.Stdout, want) {
			t.Errorf("created job missing %q from listing: %s", want, listed.Stdout)
		}
	}
	// Omitting --run-setup uses the daemon default (true).
	if !req.ShouldRunSetupCommand {
		t.Errorf("expected default ShouldRunSetupCommand true, got %v", req.ShouldRunSetupCommand)
	}
	// Omitting --zero-output uses the daemon default (false).
	if req.IsZeroOutput {
		t.Errorf("expected persisted IsZeroOutput unchanged or default, got %v", req.IsZeroOutput)
	}
}

// --- BOS-565: --zero-output on cron add/update, show, and --json ---

// --- BOS-1443: --concurrency on cron add/update, show, and --json ---

func storedCLICronJobs(t *testing.T, h *clitest.Harness) []*pb.CronJob {
	t.Helper()
	res, err := h.Stack.Client().ListCronJobs(t.Context(), realstackrpc.Request(&pb.ListCronJobsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.CronJobs
}
