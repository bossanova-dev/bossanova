package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/recurser/boss/internal/auth"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// `boss trigger` manages inbound triggers: bosso-owned rules that start a NEW
// session when an authenticated HTTP request or a GitHub App event arrives.
// Triggers live only in the cloud, so every subcommand talks to bosso with the
// keychain login — whether or not --remote is set — and never to the daemon.
//
// The HTTP signing secret is returned by exactly two RPCs (create and rotate).
// It is printed once from those responses and never stored or logged by the
// CLI; triggerJSON deliberately has no field that could carry it.

// triggerClient is the cloud surface the trigger commands use.
// *client.RemoteClient satisfies it; tests substitute a fake server through
// newTriggerClient.
type triggerClient interface {
	organizationLister
	GetTriggerCatalog(ctx context.Context) (*pb.TriggerCatalog, error)
	ListTriggers(ctx context.Context, req *pb.ListTriggersRequest) ([]*pb.Trigger, error)
	GetTrigger(ctx context.Context, id string) (*pb.Trigger, error)
	CreateTrigger(ctx context.Context, req *pb.CreateTriggerRequest) (*pb.CreateTriggerResponse, error)
	UpdateTrigger(ctx context.Context, req *pb.UpdateTriggerRequest) (*pb.Trigger, error)
	DeleteTrigger(ctx context.Context, id string) error
	RotateTriggerSecret(ctx context.Context, id string) (*pb.RotateTriggerSecretResponse, error)
	TestTrigger(ctx context.Context, req *pb.TestTriggerRequest) (*pb.TriggerInvocation, error)
	ListTriggerInvocations(ctx context.Context, req *pb.ListTriggerInvocationsRequest) ([]*pb.TriggerInvocation, error)
}

// errTriggerNotLoggedIn is the failure for a caller with no usable cloud login.
var errTriggerNotLoggedIn = errors.New("triggers need a cloud login: run 'boss login' first")

// newTriggerClient builds the cloud client from the keychain login. A var so
// tests can inject a client pointed at a fake server.
var newTriggerClient = func(cmd *cobra.Command) (triggerClient, error) {
	remote, err := newCloudRemote(cmd, errTriggerNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// triggerRemote dials bosso with mgr's access token. No stored login, an
// expired one or a re-login demand all surface as errTriggerNotLoggedIn before
// any request is sent.
func triggerRemote(ctx context.Context, mgr *auth.Manager, url string) (triggerClient, error) {
	remote, err := dialCloudRemote(ctx, mgr, url, errTriggerNotLoggedIn)
	if err != nil {
		return nil, err
	}
	return remote, nil
}

// triggerStdinIsTerminal reports whether `boss trigger rm` may prompt. A var
// so tests force either branch.
var triggerStdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// triggerHistoryDefaultLimit is how many invocations `show` and `history` read
// by default.
const triggerHistoryDefaultLimit = 20

// secretNotice follows every printed secret.
const secretNotice = "store it now; it will not be shown again"

// --- Runners --------------------------------------------------------------

func triggerWantsJSON(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool(jsonFlagName)
	return v
}

// invalidTriggerArg tags a local validation failure so --json callers see
// INVALID_ARGUMENT, the same code the API would have returned.
func invalidTriggerArg(err error) error { return codedError(codeInvalidArgument, err) }

func runTriggerCatalog(cmd *cobra.Command, c triggerClient) error {
	catalog, err := c.GetTriggerCatalog(cmd.Context())
	if err != nil {
		return fmt.Errorf("get trigger catalog: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerCatalogToJSON(catalog))
	}
	var b strings.Builder
	for i, t := range catalog.GetTypes() {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%s — %s (config v%d)\n", t.GetName(), orDash(t.GetDisplayName()), t.GetConfigVersion())
		if len(t.GetEventTypes()) > 0 {
			b.WriteString("  Events (--event):\n")
			for _, e := range t.GetEventTypes() {
				fmt.Fprintf(&b, "    %s — %s\n", e.GetId(), orDash(e.GetDescription()))
			}
		}
		if len(t.GetFilterFields()) > 0 {
			b.WriteString("  Filter fields (--filter / --payload-field):\n")
			for _, f := range t.GetFilterFields() {
				ops := make([]string, 0, len(f.GetOperators()))
				for _, op := range f.GetOperators() {
					ops = append(ops, triggerOperatorSpelling(op))
				}
				name := f.GetField()
				if f.GetIsPrefix() {
					name += "<name>"
				}
				fmt.Fprintf(&b, "    %s [%s] — %s\n", name, strings.Join(ops, " "), orDash(f.GetDescription()))
			}
		}
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runTriggerList(cmd *cobra.Command, c triggerClient) error {
	req := &pb.ListTriggersRequest{}
	if v, _ := cmd.Flags().GetString("org"); strings.TrimSpace(v) != "" {
		org := strings.TrimSpace(v)
		req.OrganizationId = &org
	}
	triggers, err := c.ListTriggers(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("list triggers: %w", err)
	}
	if triggerWantsJSON(cmd) {
		out := struct {
			Triggers []triggerJSON `json:"triggers"`
		}{Triggers: make([]triggerJSON, len(triggers))}
		base := cloudURL(cmd)
		for i, t := range triggers {
			out.Triggers[i] = triggerToJSON(t, base)
		}
		return emitJSON(cmd, out)
	}
	if len(triggers) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No triggers.")
		return nil
	}
	renderTriggerTable(cmd, triggers)
	return nil
}

func renderTriggerTable(cmd *cobra.Command, triggers []*pb.Trigger) {
	rows := make([][]string, len(triggers))
	for i, t := range triggers {
		last := t.GetLastInvocation()
		lastStatus := "-"
		if last != nil {
			lastStatus = triggerInvocationStatus(last.GetStatus())
		}
		rows[i] = []string{
			t.GetId(), t.GetName(), t.GetTriggerType(), fmt.Sprintf("%t", t.GetIsEnabled()),
			orDash(t.GetRepoOriginUrl()), triggerPlacementName(t.GetPlacement()), lastStatus,
			orDash(rfc3339OrEmpty(last.GetReceivedAt())),
		}
	}
	renderCLITable(cmd, []string{"ID", "NAME", "TYPE", "ENABLED", "REPO", "PLACEMENT", "LAST STATUS", "LAST RECEIVED"}, rows)
}

// renderCLITable prints rows under titles in the shared CLI table style.
func renderCLITable(cmd *cobra.Command, titles []string, rows [][]string) {
	cells := make([][]string, len(titles))
	tableRows := make([]table.Row, len(rows))
	for i, row := range rows {
		tableRows[i] = row
		for col, cell := range row {
			cells[col] = append(cells[col], cell)
		}
	}
	cols := make([]table.Column, len(titles))
	for i, title := range titles {
		cols[i] = table.Column{Title: title, Width: views.MaxColWidth(title, cells[i], 0)}
	}
	t := table.New(
		table.WithColumns(cols),
		table.WithRows(tableRows),
		table.WithHeight(len(tableRows)+1),
		table.WithWidth(views.CLIColumnsWidth(cols)),
		table.WithStyles(views.CLITableStyles()),
		table.WithFocused(false),
	)
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), t.View())
}

func writeTriggerDetail(b *strings.Builder, t *pb.Trigger, baseURL string) {
	fmt.Fprintf(b, "ID:          %s\n", t.GetId())
	fmt.Fprintf(b, "Name:        %s\n", t.GetName())
	fmt.Fprintf(b, "Type:        %s\n", t.GetTriggerType())
	fmt.Fprintf(b, "Enabled:     %t\n", t.GetIsEnabled())
	fmt.Fprintf(b, "Org:         %s\n", orDash(t.GetOrganizationId()))
	fmt.Fprintf(b, "Repo:        %s\n", orDash(t.GetRepoOriginUrl()))
	if url := triggerEndpointURL(baseURL, t.GetEndpointPath()); url != "" {
		fmt.Fprintf(b, "Endpoint:    %s\n", url)
	}
	if h := t.GetHttp(); h != nil {
		fmt.Fprintf(b, "Methods:     %s\n", orDash(strings.Join(h.GetAllowedMethods(), ",")))
		fmt.Fprintf(b, "Idem header: %s\n", orDash(h.GetIdempotencyHeader()))
		fmt.Fprintf(b, "Dedup:       %ds\n", h.GetDedupWindowSeconds())
	}
	if g := t.GetGithub(); g != nil {
		fmt.Fprintf(b, "Events:      %s\n", orDash(strings.Join(g.GetEventTypes(), ", ")))
	}
	fmt.Fprintf(b, "Placement:   %s\n", triggerPlacementName(t.GetPlacement()))
	fmt.Fprintf(b, "Concurrency: %s\n", triggerConcurrencyName(t.GetConcurrencyPolicy()))
	fmt.Fprintf(b, "Cooldown:    %ds\n", t.GetCooldownSeconds())
	l := t.GetLaunch()
	fmt.Fprintf(b, "Skill:       %s\n", orDash(l.GetSkillName()))
	fmt.Fprintf(b, "Agent:       %s\n", orDash(l.GetAgentName()))
	fmt.Fprintf(b, "Model:       %s\n", orDash(l.GetModel()))
	fmt.Fprintf(b, "Effort:      %s\n", orDash(l.GetEffort()))
	fmt.Fprintf(b, "Base branch: %s\n", orDash(l.GetBaseBranch()))
	if len(t.GetFilters()) == 0 {
		b.WriteString("Filters:     - (matches everything)\n")
	} else {
		b.WriteString("Filters:\n")
		for _, f := range t.GetFilters() {
			fmt.Fprintf(b, "  %s\n", formatTriggerFilter(f))
		}
	}
	fmt.Fprintf(b, "Payload:     %s\n", orDash(strings.Join(t.GetPayloadFields(), ", ")))
	fmt.Fprintf(b, "Created:     %s\n", orDash(rfc3339OrEmpty(t.GetCreatedAt())))
	fmt.Fprintf(b, "Updated:     %s\n", orDash(rfc3339OrEmpty(t.GetUpdatedAt())))
	fmt.Fprintf(b, "\nPrompt:\n%s\n", l.GetPromptTemplate())
}

func writeTriggerInvocations(b *strings.Builder, invs []*pb.TriggerInvocation) {
	if len(invs) == 0 {
		b.WriteString("No invocations.\n")
		return
	}
	for _, inv := range invs {
		fmt.Fprintf(b, "%s  %-12s %-22s source=%s event=%s daemon=%s session=%s\n",
			orDash(rfc3339OrEmpty(inv.GetReceivedAt())), triggerInvocationStatus(inv.GetStatus()),
			orDash(inv.GetDecisionReason()), orDash(inv.GetSource()), orDash(inv.GetEventType()),
			orDash(inv.GetDaemonId()), orDash(inv.GetSessionId()))
		if d := inv.GetErrorDetail(); d != "" {
			fmt.Fprintf(b, "    %s\n", d)
		}
	}
}

func runTriggerShow(cmd *cobra.Command, c triggerClient, id string) error {
	t, err := c.GetTrigger(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("get trigger: %w", err)
	}
	invs, err := c.ListTriggerInvocations(cmd.Context(), &pb.ListTriggerInvocationsRequest{TriggerId: id, Limit: triggerHistoryDefaultLimit})
	if err != nil {
		return fmt.Errorf("list trigger invocations: %w", err)
	}
	base := cloudURL(cmd)
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerShowJSON{Trigger: triggerToJSON(t, base), Invocations: triggerInvocationsToJSON(invs)})
	}
	var b strings.Builder
	writeTriggerDetail(&b, t, base)
	fmt.Fprintf(&b, "\nLast %d invocations:\n", triggerHistoryDefaultLimit)
	writeTriggerInvocations(&b, invs)
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

// printTriggerSecret writes a one-time secret exactly once, with the warning.
func printTriggerSecret(b *strings.Builder, secret string) {
	fmt.Fprintf(b, "Signing secret: %s\n", secret)
	fmt.Fprintf(b, "%s\n", secretNotice)
}

func runTriggerAdd(cmd *cobra.Command, c triggerClient) error {
	req, err := buildCreateTriggerRequest(cmd)
	if err != nil {
		return invalidTriggerArg(err)
	}
	if req.OrganizationId, err = resolveOrgID(cmd, c); err != nil {
		return err
	}
	resp, err := c.CreateTrigger(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("create trigger: %w", err)
	}
	base := cloudURL(cmd)
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerSecretJSON{Trigger: triggerToJSON(resp.GetTrigger(), base), Secret: resp.GetSecret()})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Created trigger %s (%s)\n", resp.GetTrigger().GetId(), resp.GetTrigger().GetName())
	if url := triggerEndpointURL(base, resp.GetTrigger().GetEndpointPath()); url != "" {
		fmt.Fprintf(&b, "Endpoint: %s\n", url)
	}
	if resp.GetSecret() != "" {
		printTriggerSecret(&b, resp.GetSecret())
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runTriggerUpdate(cmd *cobra.Command, c triggerClient, id string) error {
	req, needsCurrent, err := buildUpdateTriggerRequest(cmd, id)
	if err != nil {
		return invalidTriggerArg(err)
	}
	if needsCurrent {
		current, err := c.GetTrigger(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("read trigger %s: %w", id, err)
		}
		if err := mergeTriggerUpdate(cmd, req, current); err != nil {
			return invalidTriggerArg(err)
		}
	}
	t, err := c.UpdateTrigger(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("update trigger: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerToJSON(t, cloudURL(cmd)))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated trigger %s\n", t.GetId())
	return nil
}

func runTriggerSetEnabled(cmd *cobra.Command, c triggerClient, id string, enabled bool) error {
	t, err := c.UpdateTrigger(cmd.Context(), &pb.UpdateTriggerRequest{Id: id, IsEnabled: &enabled})
	if err != nil {
		return fmt.Errorf("update trigger: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerToJSON(t, cloudURL(cmd)))
	}
	state := "enabled"
	if !enabled {
		state = "disabled"
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Trigger %s %s\n", id, state)
	return nil
}

func runTriggerRotateSecret(cmd *cobra.Command, c triggerClient, id string) error {
	resp, err := c.RotateTriggerSecret(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("rotate trigger secret: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerSecretJSON{Trigger: triggerToJSON(resp.GetTrigger(), cloudURL(cmd)), Secret: resp.GetSecret()})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Rotated the secret of trigger %s; the old secret no longer validates.\n", id)
	printTriggerSecret(&b, resp.GetSecret())
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runTriggerTest(cmd *cobra.Command, c triggerClient, id string) error {
	payload, err := readTriggerPayload(cmd)
	if err != nil {
		return invalidTriggerArg(err)
	}
	event, _ := cmd.Flags().GetString("event")
	launch, _ := cmd.Flags().GetBool("launch")
	inv, err := c.TestTrigger(cmd.Context(), &pb.TestTriggerRequest{
		TriggerId: id, SamplePayloadJson: payload, SampleEventType: event, ShouldLaunch: launch,
	})
	if err != nil {
		return fmt.Errorf("test trigger: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, triggerInvocationToJSON(inv))
	}
	var b strings.Builder
	mode := "Dry run"
	if launch {
		mode = "Test with launch"
	}
	fmt.Fprintf(&b, "%s of trigger %s recorded invocation %s\n", mode, id, inv.GetId())
	writeTriggerInvocations(&b, []*pb.TriggerInvocation{inv})
	if excerpt := inv.GetPayloadExcerpt(); excerpt != "" {
		fmt.Fprintf(&b, "Payload excerpt: %s\n", excerpt)
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runTriggerHistory(cmd *cobra.Command, c triggerClient, id string) error {
	limit, _ := cmd.Flags().GetInt32("limit")
	invs, err := c.ListTriggerInvocations(cmd.Context(), &pb.ListTriggerInvocationsRequest{TriggerId: id, Limit: limit})
	if err != nil {
		return fmt.Errorf("list trigger invocations: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, struct {
			Invocations []triggerInvocationJSON `json:"invocations"`
		}{Invocations: triggerInvocationsToJSON(invs)})
	}
	var b strings.Builder
	writeTriggerInvocations(&b, invs)
	_, _ = fmt.Fprint(cmd.OutOrStdout(), b.String())
	return nil
}

func runTriggerRemove(cmd *cobra.Command, c triggerClient, id string) error {
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		if triggerWantsJSON(cmd) || !triggerStdinIsTerminal() {
			return codedError(codeConfirmationRequired,
				errors.New("trigger rm: pass --yes to delete without a confirmation prompt (no terminal to prompt on, or --json)"))
		}
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Permanently delete trigger %s and its invocation history? [y/N] ", id)
		answer, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
		if a := strings.TrimSpace(answer); a != "y" && a != "Y" {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Cancelled.")
			return nil
		}
	}
	if err := c.DeleteTrigger(cmd.Context(), id); err != nil {
		return fmt.Errorf("delete trigger: %w", err)
	}
	if triggerWantsJSON(cmd) {
		return emitJSON(cmd, map[string]string{"deleted_trigger": id})
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Deleted trigger %s\n", id)
	return nil
}

// triggerRunE adapts a runner to cobra: it builds the cloud client first and
// routes every failure through the --json envelope.
func triggerRunE(run func(*cobra.Command, triggerClient, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		asJSON := triggerWantsJSON(cmd)
		c, err := newTriggerClient(cmd)
		if err != nil {
			return emitJSONFailure(cmd, asJSON, err)
		}
		return emitJSONFailure(cmd, asJSON, run(cmd, c, args))
	}
}

// --- Command tree ---------------------------------------------------------

func addTriggerConfigFlags(cmd *cobra.Command, update bool) {
	f := cmd.Flags()
	f.String("name", "", "Trigger name")
	f.String("repo-url", "", "Origin URL of the repo the launched session works in")
	f.String("prompt", "", "Prompt sent to the agent")
	f.String("prompt-file", "", "Read the prompt from a file (or '-' for stdin)")
	f.String("skill", "", "Skill to invoke as a slash command, without the leading '/'")
	f.String("agent", "", "Agent runner plugin name (empty = claude)")
	f.String("model", "", "Agent model id (empty = plugin default)")
	f.String("effort", "", "Agent reasoning-effort level (empty = plugin default)")
	f.String("base-branch", "", "Branch the session's worktree is cut from (empty = repo default)")
	f.String("daemon", "", "Launch only on this daemon id")
	f.Bool("first-available", false, "Launch on the first ready daemon that manages the repo")
	f.String("concurrency", "", "When the previous session is still working: skip (default), cancel (stop it, then launch), or allow (launch alongside)")
	f.Duration("cooldown", 0, "Minimum gap between two launches, e.g. 5m (0 = none)")
	f.StringArray("filter", nil, "Payload filter 'field op value[,value…]' (op: "+triggerOperatorHelp+"); repeat to AND several")
	f.StringArray("payload-field", nil, "Payload field copied into the prompt context, e.g. body.ref; repeat for several")
	f.StringArray("event", nil, "GitHub event id from `boss trigger catalog`; repeat for several (github only)")
	f.StringSlice("methods", nil, "Accepted HTTP methods, e.g. POST,PUT (http only; empty = POST)")
	f.String("idempotency-header", "", "Request header carrying the idempotency key (http only; empty = Idempotency-Key)")
	f.Duration("dedup-window", 0, "How long a seen idempotency key suppresses a repeat, e.g. 5m (http only; 0 = 5m)")
	f.Bool(jsonFlagName, false, "Emit the trigger as a stable JSON schema")
	if update {
		f.Bool("clear-filters", false, "Remove every filter")
		f.Bool("clear-payload-fields", false, "Remove every payload field")
	}
}

func triggerCmd() *cobra.Command {
	trigger := &cobra.Command{
		Use:   "trigger",
		Short: "Manage inbound HTTP and GitHub triggers that start new sessions",
	}

	catalog := &cobra.Command{
		Use:   "catalog",
		Short: "List trigger types, event ids and filter fields",
		Args:  cobra.NoArgs,
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, _ []string) error {
			return runTriggerCatalog(cmd, c)
		}),
	}
	catalog.Flags().Bool(jsonFlagName, false, "Emit the catalog as a stable JSON schema")

	list := &cobra.Command{
		Use:   "ls",
		Short: "List your triggers",
		Args:  cobra.NoArgs,
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, _ []string) error {
			return runTriggerList(cmd, c)
		}),
	}
	list.Flags().String("org", "", "Restrict to one organization id (default: every organization you belong to)")
	list.Flags().Bool(jsonFlagName, false, "Emit a stable JSON schema ({triggers}) instead of a table")

	show := &cobra.Command{
		Use:   "show <trigger-id>",
		Short: "Show a trigger's configuration and recent invocations",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerShow(cmd, c, args[0])
		}),
	}
	show.Flags().Bool(jsonFlagName, false, "Emit {trigger, invocations} as a stable JSON schema")

	add := &cobra.Command{
		Use:   "add",
		Short: "Create a trigger",
		Args:  cobra.NoArgs,
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, _ []string) error {
			return runTriggerAdd(cmd, c)
		}),
	}
	add.Flags().String("type", "", "Trigger type: http or github")
	add.Flags().String("org", "", orgFlagUsage)
	add.Flags().Bool("disabled", false, "Create the trigger disabled")
	addTriggerConfigFlags(add, false)
	for _, name := range []string{"type", "name", "repo-url"} {
		_ = add.MarkFlagRequired(name)
	}

	update := &cobra.Command{
		Use:   "update <trigger-id>",
		Short: "Change a trigger's settings",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerUpdate(cmd, c, args[0])
		}),
	}
	addTriggerConfigFlags(update, true)

	enable := &cobra.Command{
		Use:   "enable <trigger-id>",
		Short: "Enable a trigger",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerSetEnabled(cmd, c, args[0], true)
		}),
	}
	enable.Flags().Bool(jsonFlagName, false, "Emit the trigger as a stable JSON schema")

	disable := &cobra.Command{
		Use:   "disable <trigger-id>",
		Short: "Disable a trigger",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerSetEnabled(cmd, c, args[0], false)
		}),
	}
	disable.Flags().Bool(jsonFlagName, false, "Emit the trigger as a stable JSON schema")

	rotate := &cobra.Command{
		Use:   "rotate-secret <trigger-id>",
		Short: "Replace an HTTP trigger's signing secret",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerRotateSecret(cmd, c, args[0])
		}),
	}
	rotate.Flags().Bool(jsonFlagName, false, "Emit {trigger, secret} as a stable JSON schema")

	test := &cobra.Command{
		Use:   "test <trigger-id>",
		Short: "Run a sample payload through a trigger (dry run by default)",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerTest(cmd, c, args[0])
		}),
	}
	test.Flags().String("payload-file", "", "Sample JSON payload file (or '-' for stdin; default {})")
	test.Flags().String("event", "", "Event id the sample is treated as (required for github triggers)")
	test.Flags().Bool("launch", false, "Launch a real session when the sample passes every filter and policy")
	test.Flags().Bool(jsonFlagName, false, "Emit the recorded invocation as a stable JSON schema")

	history := &cobra.Command{
		Use:   "history <trigger-id>",
		Short: "Show a trigger's invocation history, newest first",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerHistory(cmd, c, args[0])
		}),
	}
	history.Flags().Int32("limit", triggerHistoryDefaultLimit, "Maximum invocations to show (max 200)")
	history.Flags().Bool(jsonFlagName, false, "Emit {invocations} as a stable JSON schema")

	remove := &cobra.Command{
		Use:   "rm <trigger-id>",
		Short: "Delete a trigger and its history",
		Args:  cobra.ExactArgs(1),
		RunE: triggerRunE(func(cmd *cobra.Command, c triggerClient, args []string) error {
			return runTriggerRemove(cmd, c, args[0])
		}),
	}
	remove.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	remove.Flags().Bool(jsonFlagName, false, "Emit the deleted trigger id as a stable JSON schema")

	trigger.AddCommand(catalog, list, show, add, update, enable, disable, rotate, test, history, remove)
	return trigger
}
