package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// cronJobJSON is the stable, documented schema emitted by `boss cron ls --json`
// and `boss cron show --json`. Field names are part of the machine contract that
// scripts depend on: renames are breaking changes. Timestamps are RFC3339
// strings, empty when the underlying timestamp is nil/zero.
type cronJobJSON struct {
	ID               string `json:"id"`
	RepoID           string `json:"repo_id"`
	Name             string `json:"name"`
	Prompt           string `json:"prompt"`
	Schedule         string `json:"schedule"`
	Timezone         string `json:"timezone"`
	Enabled          bool   `json:"enabled"`
	AgentName        string `json:"agent_name"`
	Model            string `json:"model"`
	GateCommand      string `json:"gate_command"`
	RunSetupCommand  bool   `json:"run_setup_command"`
	LastRunSessionID string `json:"last_run_session_id"`
	LastRunAt        string `json:"last_run_at"`
	LastRunOutcome   string `json:"last_run_outcome"`
	NextRunAt        string `json:"next_run_at"`
}

// cronJobToJSON maps a proto CronJob to the stable JSON schema.
func cronJobToJSON(j *pb.CronJob) cronJobJSON {
	return cronJobJSON{
		ID:               j.GetId(),
		RepoID:           j.GetRepoId(),
		Name:             j.GetName(),
		Prompt:           j.GetPrompt(),
		Schedule:         j.GetSchedule(),
		Timezone:         j.GetTimezone(),
		Enabled:          j.GetEnabled(),
		AgentName:        j.GetAgentName(),
		Model:            j.GetModel(),
		GateCommand:      j.GetGateCommand(),
		RunSetupCommand:  j.GetRunSetupCommand(),
		LastRunSessionID: j.GetLastRunSessionId(),
		LastRunAt:        rfc3339OrEmpty(j.GetLastRunAt()),
		LastRunOutcome:   j.GetLastRunOutcome(),
		NextRunAt:        rfc3339OrEmpty(j.GetNextRunAt()),
	}
}

// rfc3339OrEmpty renders a protobuf timestamp as an RFC3339 string, or "" when
// the timestamp is nil/zero.
func rfc3339OrEmpty(ts *timestamppb.Timestamp) string {
	if ts == nil || !ts.IsValid() || ts.AsTime().IsZero() {
		return ""
	}
	return ts.AsTime().UTC().Format(time.RFC3339)
}

func runCronLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	repo, _ := cmd.Flags().GetString("repo")
	jobs, err := c.ListCronJobs(ctx, repo)
	if err != nil {
		return fmt.Errorf("list cron jobs: %w", err)
	}

	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		out := make([]cronJobJSON, len(jobs))
		for i, j := range jobs {
			out[i] = cronJobToJSON(j)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal cron jobs: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}

	if len(jobs) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No cron jobs.")
		return nil
	}

	ids := make([]string, len(jobs))
	names := make([]string, len(jobs))
	repos := make([]string, len(jobs))
	schedules := make([]string, len(jobs))
	tzs := make([]string, len(jobs))
	agents := make([]string, len(jobs))
	enabled := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.GetId()
		names[i] = j.GetName()
		repos[i] = j.GetRepoId()
		schedules[i] = j.GetSchedule()
		tzs[i] = orDash(j.GetTimezone())
		agents[i] = orDash(j.GetAgentName())
		enabled[i] = boolLabel(j.GetEnabled())
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "NAME", Width: views.MaxColWidth("NAME", names, 30)},
		{Title: "REPO", Width: views.MaxColWidth("REPO", repos, 0)},
		{Title: "SCHEDULE", Width: views.MaxColWidth("SCHEDULE", schedules, 20)},
		{Title: "TZ", Width: views.MaxColWidth("TZ", tzs, 20)},
		{Title: "AGENT", Width: views.MaxColWidth("AGENT", agents, 12)},
		{Title: "ENABLED", Width: views.MaxColWidth("ENABLED", enabled, 8)},
	}

	rows := make([]table.Row, len(jobs))
	for i := range jobs {
		rows[i] = table.Row{ids[i], names[i], repos[i], schedules[i], tzs[i], agents[i], enabled[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

func runCronShow(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	job, err := c.GetCronJob(ctx, id)
	if err != nil {
		return fmt.Errorf("get cron job: %w", err)
	}

	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		b, err := json.MarshalIndent(cronJobToJSON(job), "", "  ")
		if err != nil {
			return fmt.Errorf("marshal cron job: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "ID:                  %s\n", job.GetId())
	fmt.Fprintf(&b, "Repo ID:             %s\n", job.GetRepoId())
	fmt.Fprintf(&b, "Name:                %s\n", job.GetName())
	fmt.Fprintf(&b, "Prompt:              %s\n", job.GetPrompt())
	fmt.Fprintf(&b, "Schedule:            %s\n", job.GetSchedule())
	fmt.Fprintf(&b, "Timezone:            %s\n", orDash(job.GetTimezone()))
	fmt.Fprintf(&b, "Enabled:             %s\n", boolLabel(job.GetEnabled()))
	fmt.Fprintf(&b, "Agent:               %s\n", orDash(job.GetAgentName()))
	fmt.Fprintf(&b, "Model:               %s\n", orDash(job.GetModel()))
	fmt.Fprintf(&b, "Gate command:        %s\n", orDash(job.GetGateCommand()))
	fmt.Fprintf(&b, "Run setup command:   %s\n", boolLabel(job.GetRunSetupCommand()))
	fmt.Fprintf(&b, "Last run session ID: %s\n", orDash(job.GetLastRunSessionId()))
	fmt.Fprintf(&b, "Last run at:         %s\n", orDash(rfc3339OrEmpty(job.GetLastRunAt())))
	fmt.Fprintf(&b, "Last run outcome:    %s\n", orDash(job.GetLastRunOutcome()))
	fmt.Fprintf(&b, "Next run at:         %s\n", orDash(rfc3339OrEmpty(job.GetNextRunAt())))
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runCronAdd(cmd *cobra.Command) error {
	repo, _ := cmd.Flags().GetString("repo")
	name, _ := cmd.Flags().GetString("name")
	schedule, _ := cmd.Flags().GetString("schedule")
	if repo == "" {
		return fmt.Errorf("--repo is required")
	}
	if name == "" {
		return fmt.Errorf("--name is required")
	}
	if schedule == "" {
		return fmt.Errorf("--schedule is required")
	}

	prompt, _, err := readPromptFlag(cmd, true)
	if err != nil {
		return err
	}

	agent, _ := cmd.Flags().GetString("agent")
	gate, _ := cmd.Flags().GetString("gate")
	model, _ := cmd.Flags().GetString("model")
	tz, _ := cmd.Flags().GetString("tz")
	enabled, _ := cmd.Flags().GetBool("enabled")

	req := &pb.CreateCronJobRequest{
		RepoId:      repo,
		Name:        name,
		Prompt:      prompt,
		Schedule:    schedule,
		Timezone:    tz,
		Enabled:     enabled,
		AgentName:   agent,
		Model:       model,
		GateCommand: gate,
	}
	if cmd.Flags().Changed("run-setup") {
		v, _ := cmd.Flags().GetBool("run-setup")
		req.RunSetupCommand = &v
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	job, err := c.CreateCronJob(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("create cron job: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Created cron job %s\n", job.GetId())
	return nil
}

func runCronUpdate(cmd *cobra.Command, id string) error {
	req := &pb.UpdateCronJobRequest{Id: id}
	anyChanged := false

	stringFlags := []struct {
		flag   string
		setter func(v string)
	}{
		{"name", func(v string) { req.Name = proto.String(v) }},
		{"schedule", func(v string) { req.Schedule = proto.String(v) }},
		{"agent", func(v string) { req.AgentName = proto.String(v) }},
		{"gate", func(v string) { req.GateCommand = proto.String(v) }},
		{"model", func(v string) { req.Model = proto.String(v) }},
		{"tz", func(v string) { req.Timezone = proto.String(v) }},
	}
	for _, sf := range stringFlags {
		if cmd.Flags().Changed(sf.flag) {
			v, _ := cmd.Flags().GetString(sf.flag)
			sf.setter(v)
			anyChanged = true
		}
	}

	if cmd.Flags().Changed("enabled") {
		v, _ := cmd.Flags().GetBool("enabled")
		req.Enabled = proto.Bool(v)
		anyChanged = true
	}
	if cmd.Flags().Changed("run-setup") {
		v, _ := cmd.Flags().GetBool("run-setup")
		req.RunSetupCommand = proto.Bool(v)
		anyChanged = true
	}

	// Prompt: set only when --prompt or --prompt-file was provided.
	prompt, provided, err := readPromptFlag(cmd, false)
	if err != nil {
		return err
	}
	if provided {
		req.Prompt = proto.String(prompt)
		anyChanged = true
	}

	if !anyChanged {
		return fmt.Errorf("no flags provided — use --name, --schedule, --prompt, --agent, --gate, --model, --tz, --enabled, or --run-setup")
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	if _, err := c.UpdateCronJob(cmd.Context(), req); err != nil {
		return fmt.Errorf("update cron job: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated cron job %s\n", id)
	return nil
}

func runCronRemove(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	if err := c.DeleteCronJob(cmd.Context(), id); err != nil {
		return fmt.Errorf("remove cron job: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed cron job %s\n", id)
	return nil
}

func runCronRunNow(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	resp, err := c.RunCronJobNow(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("run cron job: %w", err)
	}
	out := cmd.OutOrStdout()
	if resp.GetSession() != nil {
		_, _ = fmt.Fprintf(out, "Started session %s for cron job %s\n", resp.GetSession().GetId(), id)
		return nil
	}
	reason := resp.GetSkippedReason()
	if reason == "" {
		reason = "no reason given"
	}
	_, _ = fmt.Fprintf(out, "Fire skipped for cron job %s: %s\n", id, reason)
	return nil
}

func runCronEnable(cmd *cobra.Command, id string) error {
	return setCronEnabled(cmd, id, true)
}

func runCronDisable(cmd *cobra.Command, id string) error {
	return setCronEnabled(cmd, id, false)
}

func setCronEnabled(cmd *cobra.Command, id string, enabled bool) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	req := &pb.UpdateCronJobRequest{Id: id, Enabled: proto.Bool(enabled)}
	if _, err := c.UpdateCronJob(cmd.Context(), req); err != nil {
		return fmt.Errorf("update cron job: %w", err)
	}
	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Cron job %s %s\n", id, state)
	return nil
}

// readPromptFlag resolves the prompt for add/update. It returns the prompt text
// and whether a prompt was provided at all. --prompt wins; otherwise
// --prompt-file is read (a path, or stdin when the value is "-"). When required
// is true (add), it errors if both are set or neither is set. When required is
// false (update), neither set is allowed and returns ("", false, nil).
func readPromptFlag(cmd *cobra.Command, required bool) (string, bool, error) {
	promptSet := cmd.Flags().Changed("prompt")
	fileSet := cmd.Flags().Changed("prompt-file")

	if promptSet && fileSet {
		return "", false, fmt.Errorf("cannot use both --prompt and --prompt-file")
	}
	if !promptSet && !fileSet {
		if required {
			return "", false, fmt.Errorf("one of --prompt or --prompt-file is required")
		}
		return "", false, nil
	}

	if promptSet {
		v, _ := cmd.Flags().GetString("prompt")
		return v, true, nil
	}

	path, _ := cmd.Flags().GetString("prompt-file")
	if path == "-" {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return "", false, fmt.Errorf("read prompt from stdin: %w", err)
		}
		return string(b), true, nil
	}
	// #nosec G304 -- path is the operator-named `--prompt-file` CLI argument; reading the cron prompt from the file the operator specifies is the feature, and it holds no secret. owner=@recurser review-by=2026-09-16 issue=BOS-414
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, fmt.Errorf("read prompt file: %w", err)
	}
	return string(b), true, nil
}

// orDash returns "-" for an empty string, else the string itself.
func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
