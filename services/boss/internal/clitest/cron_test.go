package clitest_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/clitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func testCronJobs() []*pb.CronJob {
	return []*pb.CronJob{
		{
			Id:                    "cron-aaa",
			RepoId:                "repo-1",
			Name:                  "nightly-debt",
			Prompt:                "sweep technical debt",
			Schedule:              "0 3 * * *",
			Timezone:              "UTC",
			IsEnabled:             true,
			AgentName:             "claude",
			Model:                 "opus",
			GateCommand:           "make lint",
			ShouldRunSetupCommand: true,
			IsZeroOutput:          true,
			ConcurrencyPolicy:     pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT,
			LastRunSessionId:      "sess-aaa-111",
			LastRunAgentName:      "claude",
			LastRunAt:             timestamppb.New(timestampDaysAgo(1)),
			LastRunOutcome:        "pr_created",
			NextRunAt:             timestamppb.New(timestampDaysAgo(-1)),
		},
		{
			Id:        "cron-bbb",
			RepoId:    "repo-2",
			Name:      "weekly-mutation",
			Prompt:    "run mutation tests",
			Schedule:  "0 4 * * 1",
			Timezone:  "America/New_York",
			IsEnabled: false,
			AgentName: "codex",
		},
	}
}

// cronJSON mirrors the stable schema emitted by `boss cron ls/show --json`.
type cronJSON struct {
	ID                    string `json:"id"`
	RepoID                string `json:"repo_id"`
	Name                  string `json:"name"`
	Prompt                string `json:"prompt"`
	Schedule              string `json:"schedule"`
	Timezone              string `json:"timezone"`
	Enabled               bool   `json:"enabled"`
	AgentName             string `json:"agent_name"`
	Model                 string `json:"model"`
	GateCommand           string `json:"gate_command"`
	ShouldRunSetupCommand bool   `json:"run_setup_command"`
	IsZeroOutput          bool   `json:"zero_output"`
	ConcurrencyPolicy     string `json:"concurrency_policy"`
	LastRunSessionID      string `json:"last_run_session_id"`
	LastRunAgentName      string `json:"last_run_agent_name"`
	LastRunAt             string `json:"last_run_at"`
	LastRunOutcome        string `json:"last_run_outcome"`
	NextRunAt             string `json:"next_run_at"`
}

func TestCLI_Cron_Ls(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"cron-aaa", "nightly-debt", "claude", "cron-bbb", "weekly-mutation", "unknown"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Cron_Ls_Empty(t *testing.T) {
	h := clitest.New(t)
	res := h.Run("cron", "ls")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "No cron jobs") {
		t.Errorf("expected empty-state message, got %q", res.Stdout)
	}
}

func TestCLI_Cron_Ls_RepoFilter(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "ls", "--repo", "repo-2")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "weekly-mutation") {
		t.Errorf("stdout should contain repo-2 job, got %q", res.Stdout)
	}
	if strings.Contains(res.Stdout, "nightly-debt") {
		t.Errorf("stdout should NOT contain repo-1 job, got %q", res.Stdout)
	}
}

func TestCLI_Cron_Ls_JSON(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "ls", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var jobs []cronJSON
	if err := json.Unmarshal([]byte(res.Stdout), &jobs); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(jobs))
	}
	var first cronJSON
	for _, j := range jobs {
		if j.ID == "cron-aaa" {
			first = j
		}
	}
	if first.ID != "cron-aaa" {
		t.Fatalf("cron-aaa not present in JSON")
	}
	if first.Name != "nightly-debt" || first.RepoID != "repo-1" || first.Schedule != "0 3 * * *" {
		t.Errorf("unexpected fields: %+v", first)
	}
	if !first.Enabled || !first.ShouldRunSetupCommand {
		t.Errorf("expected enabled+run_setup_command true: %+v", first)
	}
	if !first.IsZeroOutput {
		t.Errorf("expected zero_output true in --json output: %+v", first)
	}
	if first.Model != "opus" || first.GateCommand != "make lint" || first.AgentName != "claude" {
		t.Errorf("unexpected agent/model/gate: %+v", first)
	}
	if first.LastRunSessionID != "sess-aaa-111" || first.LastRunOutcome != "pr_created" {
		t.Errorf("unexpected last-run fields: %+v", first)
	}
	if first.LastRunAgentName != "claude" {
		t.Errorf("unexpected last_run_agent_name: %+v", first)
	}
	if first.LastRunAt == "" || first.NextRunAt == "" {
		t.Errorf("expected RFC3339 timestamps, got last=%q next=%q", first.LastRunAt, first.NextRunAt)
	}
	// A job with nil timestamps renders empty strings.
	for _, j := range jobs {
		if j.ID == "cron-bbb" && (j.LastRunAt != "" || j.NextRunAt != "") {
			t.Errorf("expected empty timestamps for cron-bbb, got last=%q next=%q", j.LastRunAt, j.NextRunAt)
		}
	}
}

func TestCLI_Cron_Show(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "show", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"cron-aaa", "nightly-debt", "sweep technical debt", "0 3 * * *", "make lint", "pr_created"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout missing %q\n%s", want, res.Stdout)
		}
	}
}

func TestCLI_Cron_Show_LabelsLastRunAgentMismatch(t *testing.T) {
	jobs := testCronJobs()
	jobs[0].AgentName = "codex"
	jobs[0].LastRunAgentName = "claude"
	h := clitest.New(t, clitest.WithCronJobs(jobs...))
	res := h.Run("cron", "show", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Last run agent:      claude (mismatch: job codex)") {
		t.Fatalf("stdout missing mismatch label:\n%s", res.Stdout)
	}
}

func TestCLI_Cron_Show_LabelsUnknownLastRunAgent(t *testing.T) {
	jobs := testCronJobs()
	jobs[0].LastRunAgentName = ""
	h := clitest.New(t, clitest.WithCronJobs(jobs...))
	res := h.Run("cron", "show", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Last run agent:      unknown") {
		t.Fatalf("stdout missing unknown last-run agent:\n%s", res.Stdout)
	}
}

func TestCLI_Cron_Show_JSON(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "show", "cron-aaa", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var job cronJSON
	if err := json.Unmarshal([]byte(res.Stdout), &job); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if job.ID != "cron-aaa" || job.Prompt != "sweep technical debt" {
		t.Errorf("unexpected job: %+v", job)
	}
}

func TestCLI_Cron_Show_UnknownID(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "show", "does-not-exist")

	if res.ExitCode == 0 {
		t.Fatalf("expected non-zero exit, got 0; stdout=%q", res.Stdout)
	}
	if !strings.Contains(res.Stderr, "does-not-exist") {
		t.Errorf("stderr should mention the id, got %q", res.Stderr)
	}
}

func TestCLI_Cron_Add(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("cron", "add",
		"--repo", "repo-1",
		"--name", "my-job",
		"--schedule", "0 5 * * *",
		"--prompt", "do the thing",
		"--agent", "claude",
		"--gate", "make test",
		"--model", "opus",
		"--tz", "UTC",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls))
	}
	req := calls[0]
	if req.RepoId != "repo-1" || req.Name != "my-job" || req.Schedule != "0 5 * * *" {
		t.Errorf("unexpected core fields: %+v", req)
	}
	if req.Prompt != "do the thing" || req.AgentName != "claude" || req.GateCommand != "make test" {
		t.Errorf("unexpected prompt/agent/gate: %+v", req)
	}
	if req.Model != "opus" || req.Timezone != "UTC" {
		t.Errorf("unexpected model/tz: %+v", req)
	}
	if !req.IsEnabled {
		t.Errorf("expected Enabled default true")
	}
	// --run-setup not given: ShouldRunSetupCommand stays nil so server default applies.
	if req.ShouldRunSetupCommand != nil {
		t.Errorf("expected ShouldRunSetupCommand nil when --run-setup omitted, got %v", *req.ShouldRunSetupCommand)
	}
	// --zero-output not given: IsZeroOutput stays nil so the server default (false) applies.
	if req.IsZeroOutput != nil {
		t.Errorf("expected IsZeroOutput nil when --zero-output omitted, got %v", *req.IsZeroOutput)
	}
}

func TestCLI_Cron_Add_RunSetupFalse(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("cron", "add",
		"--repo", "repo-1", "--name", "j", "--schedule", "@daily",
		"--prompt", "p", "--run-setup=false",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls))
	}
	if calls[0].ShouldRunSetupCommand == nil {
		t.Fatalf("expected non-nil ShouldRunSetupCommand")
	}
	if *calls[0].ShouldRunSetupCommand {
		t.Errorf("expected ShouldRunSetupCommand false, got true")
	}
}

func TestCLI_Cron_Add_PromptFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/prompt.txt"
	body := "line one\nline two\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("cron", "add",
		"--repo", "repo-1", "--name", "j", "--schedule", "@daily",
		"--prompt-file", path,
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls))
	}
	if calls[0].Prompt != body {
		t.Errorf("expected multi-line prompt %q, got %q", body, calls[0].Prompt)
	}
}

func TestCLI_Cron_Add_PromptStdin(t *testing.T) {
	body := "from\nstdin\n"
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.RunWithStdin(body, "cron", "add",
		"--repo", "repo-1", "--name", "j", "--schedule", "@daily",
		"--prompt-file", "-",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls))
	}
	if calls[0].Prompt != body {
		t.Errorf("expected stdin prompt %q, got %q", body, calls[0].Prompt)
	}
}

func TestCLI_Cron_Add_MissingRequired(t *testing.T) {
	cases := [][]string{
		{"cron", "add", "--name", "j", "--schedule", "@daily", "--prompt", "p"},      // no --repo
		{"cron", "add", "--repo", "repo-1", "--schedule", "@daily", "--prompt", "p"}, // no --name
		{"cron", "add", "--repo", "repo-1", "--name", "j", "--prompt", "p"},          // no --schedule
	}
	for _, args := range cases {
		h := clitest.New(t, clitest.WithRepos(testRepos()...))
		res := h.Run(args...)
		if res.ExitCode == 0 {
			t.Errorf("expected non-zero exit for %v; stdout=%q", args, res.Stdout)
		}
	}
}

func TestCLI_Cron_Add_PromptMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/p.txt"
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := clitest.New(t, clitest.WithRepos(testRepos()...))

	// Both set → error.
	res := h.Run("cron", "add", "--repo", "repo-1", "--name", "j", "--schedule", "@daily",
		"--prompt", "p", "--prompt-file", path)
	if res.ExitCode == 0 {
		t.Errorf("expected error when both --prompt and --prompt-file set")
	}

	// Neither set → error.
	res = h.Run("cron", "add", "--repo", "repo-1", "--name", "j", "--schedule", "@daily")
	if res.ExitCode == 0 {
		t.Errorf("expected error when neither --prompt nor --prompt-file set")
	}
}

func TestCLI_Cron_Update(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa", "--schedule", "0 6 * * *")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls))
	}
	req := calls[0]
	if req.Id != "cron-aaa" {
		t.Errorf("expected id cron-aaa, got %q", req.Id)
	}
	if req.Schedule == nil || *req.Schedule != "0 6 * * *" {
		t.Errorf("expected Schedule set, got %v", req.Schedule)
	}
	// Untouched fields stay nil.
	if req.Name != nil || req.Prompt != nil || req.IsEnabled != nil || req.Model != nil {
		t.Errorf("expected unchanged fields nil, got %+v", req)
	}
}

func TestCLI_Cron_Enable_Disable(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		want bool
	}{
		{"enable", true},
		{"disable", false},
	} {
		t.Run(tc.sub, func(t *testing.T) {
			h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
			res := h.Run("cron", tc.sub, "cron-aaa")
			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			calls := h.Daemon.UpdateCronJobCalls()
			if len(calls) != 1 {
				t.Fatalf("expected 1 update call, got %d", len(calls))
			}
			if calls[0].IsEnabled == nil || *calls[0].IsEnabled != tc.want {
				t.Errorf("expected Enabled=%v, got %v", tc.want, calls[0].IsEnabled)
			}
		})
	}
}

func TestCLI_Cron_Remove(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "remove", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if h.Daemon.DeleteCronJobCallCount() != 1 {
		t.Errorf("expected 1 delete call, got %d", h.Daemon.DeleteCronJobCallCount())
	}
}

func TestCLI_Cron_RunNow_Started(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "run-now", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "cron-run-cron-aaa") {
		t.Errorf("expected started session id in output, got %q", res.Stdout)
	}
}

func TestCLI_Cron_RunNow_Skipped(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	h.Daemon.SetRunCronJobNowMode("alwaysSkip", "overlap with running session")
	res := h.Run("cron", "run-now", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "overlap with running session") {
		t.Errorf("expected skip reason in output, got %q", res.Stdout)
	}
}

// --- BOS-565: --zero-output on cron add/update, show, and --json ---

// TestCLI_Cron_Add_ZeroOutput verifies `cron add --zero-output` sends the flag
// as an explicit pointer.
func TestCLI_Cron_Add_ZeroOutput(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("cron", "add",
		"--repo", "repo-1", "--name", "j", "--schedule", "@daily",
		"--prompt", "p", "--zero-output",
	)

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls))
	}
	if calls[0].IsZeroOutput == nil {
		t.Fatalf("expected non-nil IsZeroOutput")
	}
	if !*calls[0].IsZeroOutput {
		t.Errorf("expected IsZeroOutput true, got false")
	}
}

// TestCLI_Cron_Update_ZeroOutputFalse verifies `cron update --zero-output=false`
// turns the flag off. An explicit false must reach the daemon, which is why the
// flag is read through Changed() rather than its value alone.
func TestCLI_Cron_Update_ZeroOutputFalse(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa", "--zero-output=false")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls))
	}
	if calls[0].IsZeroOutput == nil {
		t.Fatalf("expected non-nil IsZeroOutput")
	}
	if *calls[0].IsZeroOutput {
		t.Errorf("expected IsZeroOutput false, got true")
	}
}

// TestCLI_Cron_Update_ZeroOutputOmitted is the diff half: an update that does
// not name --zero-output must leave it unset so the current value survives.
func TestCLI_Cron_Update_ZeroOutputOmitted(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa", "--name", "renamed")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls))
	}
	if calls[0].IsZeroOutput != nil {
		t.Errorf("expected IsZeroOutput nil when --zero-output omitted, got %v", *calls[0].IsZeroOutput)
	}
}

// TestCLI_Cron_Update_NoFlagsErrorListsZeroOutput pins the error string that
// enumerates every supported update flag — a new flag missing from it is a
// discoverability bug.
func TestCLI_Cron_Update_NoFlagsErrorListsZeroOutput(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa")

	if res.ExitCode == 0 {
		t.Fatalf("expected a non-zero exit for an update with no flags, got 0: %q", res.Stdout)
	}
	out := res.Stdout + res.Stderr
	if !strings.Contains(out, "no flags provided") {
		t.Fatalf("expected the no-flags error, got %q", out)
	}
	if !strings.Contains(out, "--zero-output") {
		t.Errorf("no-flags error does not list --zero-output: %q", out)
	}
}

// TestCLI_Cron_Show_ZeroOutput verifies the human `cron show` output carries a
// Zero output line.
func TestCLI_Cron_Show_ZeroOutput(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "show", "cron-aaa")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Zero output:") {
		t.Errorf("show output missing a %q line:\n%s", "Zero output:", res.Stdout)
	}
}

// --- BOS-1443: --concurrency on cron add/update, show, and --json ---

func TestCLI_Cron_Add_Concurrency(t *testing.T) {
	for _, tt := range []struct {
		value string
		want  pb.CronJobConcurrencyPolicy
	}{
		{"allow-concurrent", pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT},
		{"allow_concurrent", pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT},
		{"ALLOW-CONCURRENT", pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_ALLOW_CONCURRENT},
		{"cancel-in-progress", pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_CANCEL_IN_PROGRESS},
		{"skip", pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP},
	} {
		t.Run(tt.value, func(t *testing.T) {
			h := clitest.New(t, clitest.WithRepos(testRepos()...))
			res := h.Run("cron", "add",
				"--repo", "repo-1", "--name", "j", "--schedule", "@hourly",
				"--prompt", "p", "--concurrency", tt.value,
			)

			if res.ExitCode != 0 {
				t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
			}
			calls := h.Daemon.CreateCronJobCalls()
			if len(calls) != 1 {
				t.Fatalf("expected 1 create call, got %d", len(calls))
			}
			if calls[0].ConcurrencyPolicy == nil {
				t.Fatal("expected non-nil ConcurrencyPolicy")
			}
			if got := *calls[0].ConcurrencyPolicy; got != tt.want {
				t.Errorf("ConcurrencyPolicy = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCLI_Cron_Add_ConcurrencyOmitted leaves the field unset so the daemon's
// default (skip) applies.
func TestCLI_Cron_Add_ConcurrencyOmitted(t *testing.T) {
	h := clitest.New(t, clitest.WithRepos(testRepos()...))
	res := h.Run("cron", "add", "--repo", "repo-1", "--name", "j", "--schedule", "@hourly", "--prompt", "p")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.CreateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 create call, got %d", len(calls))
	}
	if calls[0].ConcurrencyPolicy != nil {
		t.Errorf("expected ConcurrencyPolicy nil when --concurrency omitted, got %v", *calls[0].ConcurrencyPolicy)
	}
}

func TestCLI_Cron_Concurrency_InvalidValue(t *testing.T) {
	for _, args := range [][]string{
		{"cron", "add", "--repo", "repo-1", "--name", "j", "--schedule", "@hourly", "--prompt", "p", "--concurrency", "queue"},
		{"cron", "update", "cron-aaa", "--concurrency", "queue"},
	} {
		t.Run(args[1], func(t *testing.T) {
			h := clitest.New(t, clitest.WithRepos(testRepos()...), clitest.WithCronJobs(testCronJobs()...))
			res := h.Run(args...)

			if res.ExitCode == 0 {
				t.Fatalf("expected a non-zero exit for --concurrency queue, got 0: %q", res.Stdout)
			}
			out := res.Stdout + res.Stderr
			for _, want := range []string{"queue", "skip", "cancel-in-progress", "allow-concurrent"} {
				if !strings.Contains(out, want) {
					t.Errorf("error does not name %q: %q", want, out)
				}
			}
			if n := len(h.Daemon.CreateCronJobCalls()) + len(h.Daemon.UpdateCronJobCalls()); n != 0 {
				t.Errorf("expected no RPC for an invalid value, got %d", n)
			}
		})
	}
}

func TestCLI_Cron_Update_ConcurrencySkip(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa", "--concurrency", "skip")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls))
	}
	if calls[0].ConcurrencyPolicy == nil {
		t.Fatal("expected non-nil ConcurrencyPolicy")
	}
	if got := *calls[0].ConcurrencyPolicy; got != pb.CronJobConcurrencyPolicy_CRON_JOB_CONCURRENCY_POLICY_SKIP {
		t.Errorf("ConcurrencyPolicy = %v, want SKIP", got)
	}
}

// TestCLI_Cron_Update_ConcurrencyOmitted: an update that does not name
// --concurrency must leave it unset so the current value survives.
func TestCLI_Cron_Update_ConcurrencyOmitted(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa", "--name", "renamed")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	calls := h.Daemon.UpdateCronJobCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 update call, got %d", len(calls))
	}
	if calls[0].ConcurrencyPolicy != nil {
		t.Errorf("expected ConcurrencyPolicy nil when --concurrency omitted, got %v", *calls[0].ConcurrencyPolicy)
	}
}

func TestCLI_Cron_Update_NoFlagsErrorListsConcurrency(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "update", "cron-aaa")

	if res.ExitCode == 0 {
		t.Fatalf("expected a non-zero exit for an update with no flags, got 0: %q", res.Stdout)
	}
	if out := res.Stdout + res.Stderr; !strings.Contains(out, "--concurrency") {
		t.Errorf("no-flags error does not list --concurrency: %q", out)
	}
}

// TestCLI_Cron_Show_Concurrency verifies the human output carries an aligned
// Concurrency line, and that a job with no stored policy reads as skip.
func TestCLI_Cron_Show_Concurrency(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	for id, want := range map[string]string{
		"cron-aaa": "Concurrency:         allow_concurrent\n",
		"cron-bbb": "Concurrency:         skip\n",
	} {
		res := h.Run("cron", "show", id)
		if res.ExitCode != 0 {
			t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
		}
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("show %s output missing %q:\n%s", id, want, res.Stdout)
		}
	}
}

func TestCLI_Cron_Show_JSONConcurrency(t *testing.T) {
	h := clitest.New(t, clitest.WithCronJobs(testCronJobs()...))
	res := h.Run("cron", "show", "cron-aaa", "--json")

	if res.ExitCode != 0 {
		t.Fatalf("exit=%d stderr=%q", res.ExitCode, res.Stderr)
	}
	var job cronJSON
	if err := json.Unmarshal([]byte(res.Stdout), &job); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, res.Stdout)
	}
	if job.ConcurrencyPolicy != "allow_concurrent" {
		t.Errorf("concurrency_policy = %q, want the stored string %q", job.ConcurrencyPolicy, "allow_concurrent")
	}
}
