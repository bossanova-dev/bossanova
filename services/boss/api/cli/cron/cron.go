package cron

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/domain/display"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
)

// cronJobJSON is the stable, documented schema emitted by `boss cron ls --json`
// and `boss cron show --json`. Field names are part of the machine contract that
// scripts depend on: renames are breaking changes. Timestamps are RFC3339
// strings, empty when the underlying timestamp is nil/zero.
type cronJobJSON struct {
	ID                    string `json:"id"`
	RepoID                string `json:"repo_id"`
	Name                  string `json:"name"`
	Prompt                string `json:"prompt"`
	Schedule              string `json:"schedule"`
	Timezone              string `json:"timezone"`
	IsEnabled             bool   `json:"enabled"`
	AgentName             string `json:"agent_name"`
	Model                 string `json:"model"`
	GateCommand           string `json:"gate_command"`
	ShouldRunSetupCommand bool   `json:"run_setup_command"`
	IsZeroOutput          bool   `json:"zero_output"`
	ConcurrencyPolicy     string `json:"concurrency_policy"`
	ManagedBy             string `json:"managed_by,omitempty"` // config-ownership marker; omitted when unmarked
	LastRunSessionID      string `json:"last_run_session_id"`
	LastRunAgentName      string `json:"last_run_agent_name"`
	LastRunAt             string `json:"last_run_at"`
	LastRunOutcome        string `json:"last_run_outcome"`
	NextRunAt             string `json:"next_run_at"`
}

// cronJobToJSON maps a proto CronJob to the stable JSON schema.
func cronJobToJSON(j *pb.CronJob) cronJobJSON {
	return cronJobJSON{
		ID:                    j.GetId(),
		RepoID:                j.GetRepoId(),
		Name:                  j.GetName(),
		Prompt:                j.GetPrompt(),
		Schedule:              j.GetSchedule(),
		Timezone:              j.GetTimezone(),
		IsEnabled:             j.GetIsEnabled(),
		AgentName:             j.GetAgentName(),
		Model:                 j.GetModel(),
		GateCommand:           j.GetGateCommand(),
		ShouldRunSetupCommand: j.GetShouldRunSetupCommand(),
		IsZeroOutput:          j.GetIsZeroOutput(),
		ConcurrencyPolicy:     string(models.CronJobConcurrencyPolicyFromProto(j.GetConcurrencyPolicy())),
		ManagedBy:             j.GetManagedBy(),
		LastRunSessionID:      j.GetLastRunSessionId(),
		LastRunAgentName:      j.GetLastRunAgentName(),
		LastRunAt:             cliutil.Rfc3339OrEmpty(j.GetLastRunAt()),
		LastRunOutcome:        j.GetLastRunOutcome(),
		NextRunAt:             cliutil.Rfc3339OrEmpty(j.GetNextRunAt()),
	}
}

// cliutil.Rfc3339OrEmpty renders a protobuf timestamp as an RFC3339 string, or "" when
// the timestamp is nil/zero.

func (g *group) runCronLS(cmd *cobra.Command) error {
	c, err := g.newClient(cmd)
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
	runAgents := make([]string, len(jobs))
	enabled := make([]string, len(jobs))
	for i, j := range jobs {
		ids[i] = j.GetId()
		names[i] = j.GetName()
		repos[i] = j.GetRepoId()
		schedules[i] = j.GetSchedule()
		tzs[i] = cliutil.OrDash(j.GetTimezone())
		agents[i] = cliutil.OrDash(j.GetAgentName())
		runAgents[i] = lastRunAgentLabel(j)
		enabled[i] = cliutil.BoolLabel(j.GetIsEnabled())
	}

	cols := []table.Column{
		{Title: "ID", Width: display.MaxColWidth("ID", ids, 0)},
		{Title: "NAME", Width: display.MaxColWidth("NAME", names, 30)},
		{Title: "REPO", Width: display.MaxColWidth("REPO", repos, 0)},
		{Title: "SCHEDULE", Width: display.MaxColWidth("SCHEDULE", schedules, 20)},
		{Title: "TZ", Width: display.MaxColWidth("TZ", tzs, 20)},
		{Title: "AGENT", Width: display.MaxColWidth("AGENT", agents, 12)},
		{Title: "RUN AGENT", Width: display.MaxColWidth("RUN AGENT", runAgents, 20)},
		{Title: "ENABLED", Width: display.MaxColWidth("ENABLED", enabled, 8)},
	}

	rows := make([]table.Row, len(jobs))
	for i := range jobs {
		rows[i] = table.Row{ids[i], names[i], repos[i], schedules[i], tzs[i], agents[i], runAgents[i], enabled[i]}
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithRows(rows),
		table.WithHeight(len(rows)+1),
		table.WithWidth(display.CLIColumnsWidth(cols)),
		table.WithStyles(display.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
	return nil
}

func (g *group) runCronShow(cmd *cobra.Command, id string) error {
	c, err := g.newClient(cmd)
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
	fmt.Fprintf(&b, "Timezone:            %s\n", cliutil.OrDash(job.GetTimezone()))
	fmt.Fprintf(&b, "Enabled:             %s\n", cliutil.BoolLabel(job.GetIsEnabled()))
	fmt.Fprintf(&b, "Agent:               %s\n", cliutil.OrDash(job.GetAgentName()))
	fmt.Fprintf(&b, "Model:               %s\n", cliutil.OrDash(job.GetModel()))
	fmt.Fprintf(&b, "Gate command:        %s\n", cliutil.OrDash(job.GetGateCommand()))
	fmt.Fprintf(&b, "Run setup command:   %s\n", cliutil.BoolLabel(job.GetShouldRunSetupCommand()))
	fmt.Fprintf(&b, "Zero output:         %s\n", cliutil.BoolLabel(job.GetIsZeroOutput()))
	fmt.Fprintf(&b, "Concurrency:         %s\n", models.CronJobConcurrencyPolicyFromProto(job.GetConcurrencyPolicy()))
	if managedBy := job.GetManagedBy(); managedBy != "" {
		fmt.Fprintf(&b, "Managed by:          %s\n", managedBy)
	}
	fmt.Fprintf(&b, "Last run session ID: %s\n", cliutil.OrDash(job.GetLastRunSessionId()))
	fmt.Fprintf(&b, "Last run agent:      %s\n", lastRunAgentLabel(job))
	fmt.Fprintf(&b, "Last run at:         %s\n", cliutil.OrDash(cliutil.Rfc3339OrEmpty(job.GetLastRunAt())))
	fmt.Fprintf(&b, "Last run outcome:    %s\n", cliutil.OrDash(job.GetLastRunOutcome()))
	fmt.Fprintf(&b, "Next run at:         %s\n", cliutil.OrDash(cliutil.Rfc3339OrEmpty(job.GetNextRunAt())))
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func (g *group) runCronAdd(cmd *cobra.Command) error {
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

	prompt, _, err := cliutil.ReadPromptFlag(cmd, true)
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
		IsEnabled:   enabled,
		AgentName:   agent,
		Model:       model,
		GateCommand: gate,
	}
	if cmd.Flags().Changed("run-setup") {
		v, _ := cmd.Flags().GetBool("run-setup")
		req.ShouldRunSetupCommand = &v
	}
	if cmd.Flags().Changed("zero-output") {
		v, _ := cmd.Flags().GetBool("zero-output")
		req.IsZeroOutput = &v
	}
	policy, err := concurrencyFlag(cmd)
	if err != nil {
		return err
	}
	req.ConcurrencyPolicy = policy

	c, err := g.newClient(cmd)
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

// cronConcurrencyValues is the --concurrency spelling of each policy, in the
// order the forms list them. Named in the usage error for a bad value.
const cronConcurrencyValues = "skip, cancel-in-progress, allow-concurrent"

// concurrencyFlag reads --concurrency. It returns nil when the flag was not
// given (create resolves that to skip; update leaves the stored value alone).
// The value is matched case-insensitively with '_' accepted for '-'; anything
// else is a usage error naming the valid values.
func concurrencyFlag(cmd *cobra.Command) (*pb.CronJobConcurrencyPolicy, error) {
	if !cmd.Flags().Changed("concurrency") {
		return nil, nil
	}
	raw, _ := cmd.Flags().GetString("concurrency")
	policy, ok := models.ParseCronJobConcurrencyPolicyInput(raw)
	if !ok {
		return nil, fmt.Errorf("invalid --concurrency %q: want one of %s", raw, cronConcurrencyValues)
	}
	return models.CronJobConcurrencyPolicyToProto(policy).Enum(), nil
}

func (g *group) runCronUpdate(cmd *cobra.Command, id string) error {
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
		req.IsEnabled = proto.Bool(v)
		anyChanged = true
	}
	if cmd.Flags().Changed("run-setup") {
		v, _ := cmd.Flags().GetBool("run-setup")
		req.ShouldRunSetupCommand = proto.Bool(v)
		anyChanged = true
	}
	if cmd.Flags().Changed("zero-output") {
		v, _ := cmd.Flags().GetBool("zero-output")
		req.IsZeroOutput = proto.Bool(v)
		anyChanged = true
	}
	policy, err := concurrencyFlag(cmd)
	if err != nil {
		return err
	}
	if policy != nil {
		req.ConcurrencyPolicy = policy
		anyChanged = true
	}

	// Prompt: set only when --prompt or --prompt-file was provided.
	prompt, provided, err := cliutil.ReadPromptFlag(cmd, false)
	if err != nil {
		return err
	}
	if provided {
		req.Prompt = proto.String(prompt)
		anyChanged = true
	}

	if !anyChanged {
		return fmt.Errorf("no flags provided — use --name, --schedule, --prompt, --agent, --gate, --model, --tz, --enabled, --run-setup, --zero-output, or --concurrency")
	}

	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	if _, err := c.UpdateCronJob(cmd.Context(), req); err != nil {
		return fmt.Errorf("update cron job: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated cron job %s\n", id)
	return nil
}

func (g *group) runCronRemove(cmd *cobra.Command, id string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	if err := c.DeleteCronJob(cmd.Context(), id); err != nil {
		return fmt.Errorf("remove cron job: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed cron job %s\n", id)
	return nil
}

func (g *group) runCronRunNow(cmd *cobra.Command, id string) error {
	c, err := g.newClient(cmd)
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
	_, _ = fmt.Fprintf(out, "Skipped cron job %s: %s\n", id, display.RunNowSkipLabel(resp.GetSkippedReason()))
	return nil
}

func (g *group) runCronEnable(cmd *cobra.Command, id string) error {
	return g.setCronEnabled(cmd, id, true)
}

func (g *group) runCronDisable(cmd *cobra.Command, id string) error {
	return g.setCronEnabled(cmd, id, false)
}

func (g *group) setCronEnabled(cmd *cobra.Command, id string, enabled bool) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	req := &pb.UpdateCronJobRequest{Id: id, IsEnabled: proto.Bool(enabled)}
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

// cliutil.ReadPromptFlag resolves the prompt for add/update. It returns the prompt text
// and whether a prompt was provided at all. --prompt wins; otherwise
// --prompt-file is read (a path, or stdin when the value is "-"). When required
// is true (add), it errors if both are set or neither is set. When required is
// false (update), neither set is allowed and returns ("", false, nil).

// cliutil.OrDash returns "-" for an empty string, else the string itself.

func lastRunAgentLabel(job *pb.CronJob) string {
	runAgent := strings.TrimSpace(job.GetLastRunAgentName())
	if runAgent == "" {
		return "unknown"
	}
	jobAgent := strings.TrimSpace(job.GetAgentName())
	if jobAgent != "" && runAgent != jobAgent {
		return fmt.Sprintf("%s (mismatch: job %s)", runAgent, jobAgent)
	}
	return runAgent
}
