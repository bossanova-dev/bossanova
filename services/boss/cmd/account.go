package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/table"
	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// accountJSON is the stable, documented schema emitted by `boss account ls
// --json` and `boss account test --json`. Field names are part of the machine
// contract that scripts depend on: renames are breaking changes. Timestamps are
// RFC3339 strings, empty when the underlying timestamp is nil/zero. There is
// NO credential field — the secret blob never crosses this boundary.
type accountJSON struct {
	ID            string   `json:"id"`
	Provider      string   `json:"provider"`
	Label         string   `json:"label"`
	Email         string   `json:"email"`
	Status        string   `json:"status"`
	Priority      int32    `json:"priority"`
	Health        string   `json:"health"`
	Tier          string   `json:"tier"`
	AllowedModels []string `json:"allowed_models"`
	CooldownUntil string   `json:"cooldown_until"`
	LastUsedAt    string   `json:"last_used_at"`
	LastTestOkAt  string   `json:"last_test_ok_at"`
	LastTestError string   `json:"last_test_error"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

// accountToJSON maps a proto Account to the stable JSON schema.
func accountToJSON(a *pb.Account) accountJSON {
	return accountJSON{
		ID:            a.GetId(),
		Provider:      a.GetProvider(),
		Label:         a.GetLabel(),
		Email:         a.GetEmail(),
		Status:        a.GetStatus(),
		Priority:      a.GetPriority(),
		Health:        a.GetHealth(),
		Tier:          a.GetTier(),
		AllowedModels: a.GetAllowedModels(),
		CooldownUntil: rfc3339OrEmpty(a.GetCooldownUntil()),
		LastUsedAt:    rfc3339OrEmpty(a.GetLastUsedAt()),
		LastTestOkAt:  rfc3339OrEmpty(a.GetLastTestOkAt()),
		LastTestError: a.GetLastTestError(),
		CreatedAt:     rfc3339OrEmpty(a.GetCreatedAt()),
		UpdatedAt:     rfc3339OrEmpty(a.GetUpdatedAt()),
	}
}

// validAccountProviders is the set of providers `boss account add` accepts. It
// mirrors the agent-runner providers the daemon knows about (claude, codex).
var validAccountProviders = map[string]bool{"claude": true, "codex": true}

func runAccountLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	ctx := cmd.Context()

	provider, _ := cmd.Flags().GetString("provider")
	accounts, err := c.ListAccounts(ctx, provider)
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		out := make([]accountJSON, len(accounts))
		for i, a := range accounts {
			out[i] = accountToJSON(a)
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal accounts: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}

	if len(accounts) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No accounts.")
		return nil
	}

	ids := make([]string, len(accounts))
	providers := make([]string, len(accounts))
	labels := make([]string, len(accounts))
	emails := make([]string, len(accounts))
	statuses := make([]string, len(accounts))
	priorities := make([]string, len(accounts))
	healths := make([]string, len(accounts))
	cooldowns := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = shortID(a.GetId())
		providers[i] = orDash(a.GetProvider())
		labels[i] = orDash(a.GetLabel())
		emails[i] = orDash(a.GetEmail())
		statuses[i] = orDash(a.GetStatus())
		priorities[i] = strconv.FormatInt(int64(a.GetPriority()), 10)
		healths[i] = orDash(a.GetHealth())
		cooldowns[i] = orDash(rfc3339OrEmpty(a.GetCooldownUntil()))
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 8)},
		{Title: "PROVIDER", Width: views.MaxColWidth("PROVIDER", providers, 8)},
		{Title: "LABEL", Width: views.MaxColWidth("LABEL", labels, 24)},
		{Title: "EMAIL", Width: views.MaxColWidth("EMAIL", emails, 28)},
		{Title: "STATUS", Width: views.MaxColWidth("STATUS", statuses, 10)},
		{Title: "PRIORITY", Width: views.MaxColWidth("PRIORITY", priorities, 8)},
		{Title: "HEALTH", Width: views.MaxColWidth("HEALTH", healths, 8)},
		{Title: "COOLDOWN", Width: views.MaxColWidth("COOLDOWN", cooldowns, 20)},
	}

	rows := make([]table.Row, len(accounts))
	for i := range accounts {
		rows[i] = table.Row{ids[i], providers[i], labels[i], emails[i], statuses[i], priorities[i], healths[i], cooldowns[i]}
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

// resolveAddProvider resolves the account provider from the positional arg
// (preferred) or the --provider flag, and validates it against the known set.
func resolveAddProvider(cmd *cobra.Command, args []string) (string, error) {
	provider, _ := cmd.Flags().GetString("provider")
	if len(args) > 0 {
		provider = args[0]
	}
	if provider == "" {
		return "", fmt.Errorf("provider is required: one of %s (positional arg or --provider)", trimmedProviderList())
	}
	if !validAccountProviders[provider] {
		return "", fmt.Errorf("unknown provider %q: must be one of %s", provider, trimmedProviderList())
	}
	return provider, nil
}

// runAccountAddDispatch routes `boss account add` between the non-interactive
// flag path (--token / --credential-file, from BOS-160) and the interactive
// registration walkthroughs. Provider validation runs first so unknown or
// missing providers error before any RPC or subprocess.
func runAccountAddDispatch(cmd *cobra.Command, args []string) error {
	provider, err := resolveAddProvider(cmd, args)
	if err != nil {
		return err
	}
	// Non-interactive path: a credential source pins the BOS-160 flag behavior.
	if cmd.Flags().Changed("token") || cmd.Flags().Changed("credential-file") {
		return runAccountAdd(cmd, provider)
	}
	// Interactive path: drive the provider's registration flow.
	switch provider {
	case "claude":
		return runAccountAddClaude(cmd)
	case "codex":
		return runAccountAddCodex(cmd)
	default:
		// Unreachable: resolveAddProvider already rejected unknown providers.
		return fmt.Errorf("unknown provider %q", provider)
	}
}

func runAccountAdd(cmd *cobra.Command, provider string) error {
	label, _ := cmd.Flags().GetString("label")
	if label == "" {
		return fmt.Errorf("--label is required")
	}

	credential, err := readCredentialFlag(cmd)
	if err != nil {
		return err
	}

	email, _ := cmd.Flags().GetString("email")
	priority, _ := cmd.Flags().GetInt32("priority")

	req := &pb.AddAccountRequest{
		Provider:   provider,
		Label:      label,
		Email:      email,
		Priority:   priority,
		Credential: credential,
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	account, err := c.AddAccount(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("add account: %w", err)
	}
	// Never echo the credential — only the server-assigned metadata id.
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added account %s\n", account.GetId())
	return nil
}

func runAccountUpdate(cmd *cobra.Command, id string) error {
	req := &pb.UpdateAccountRequest{Id: id}
	anyChanged := false

	stringFlags := []struct {
		flag   string
		setter func(v string)
	}{
		{"label", func(v string) { req.Label = &v }},
		{"email", func(v string) { req.Email = &v }},
		{"status", func(v string) { req.Status = &v }},
	}
	for _, sf := range stringFlags {
		if cmd.Flags().Changed(sf.flag) {
			v, _ := cmd.Flags().GetString(sf.flag)
			sf.setter(v)
			anyChanged = true
		}
	}

	if cmd.Flags().Changed("priority") {
		v, _ := cmd.Flags().GetInt32("priority")
		req.Priority = &v
		anyChanged = true
	}
	if cmd.Flags().Changed("allowed-models") {
		v, _ := cmd.Flags().GetStringSlice("allowed-models")
		req.AllowedModels = v
		anyChanged = true
	}

	if !anyChanged {
		return fmt.Errorf("no flags provided — use --label, --email, --priority, --status, or --allowed-models")
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	if _, err := c.UpdateAccount(cmd.Context(), req); err != nil {
		return fmt.Errorf("update account: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Updated account %s\n", id)
	return nil
}

// accountSwitcher is the narrow client surface `boss account switch` needs. The
// real client (client.BossClient) satisfies it; tests inject a fake.
type accountSwitcher interface {
	SwitchSessionAccount(ctx context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error)
}

// switchTargetAccountID maps the `<account>` positional arg to the account_id
// the daemon expects. The daemon treats an empty account_id as the system
// default (account 0), so the friendly sentinels below all resolve to "".
// Any other value is passed through verbatim as the account id (or label the
// daemon resolves).
func switchTargetAccountID(arg string) string {
	switch strings.ToLower(strings.TrimSpace(arg)) {
	case "", "system-default", "system", "default", "none", "0":
		return ""
	default:
		return arg
	}
}

// runAccountSwitch is the cobra RunE for `boss account switch <session> <account>`.
func runAccountSwitch(cmd *cobra.Command, args []string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	return accountSwitch(cmd, c, args[0], args[1])
}

// accountSwitch stops the session's live chat, rebinds it to the chosen
// account, and respawns with resume. It is split from runAccountSwitch so tests
// can drive it with a fake accountSwitcher without a live daemon.
func accountSwitch(cmd *cobra.Command, c accountSwitcher, sessionArg, accountArg string) error {
	req := &pb.SwitchSessionAccountRequest{
		SessionId: sessionArg,
		AccountId: switchTargetAccountID(accountArg),
	}
	if force, _ := cmd.Flags().GetBool("force"); force {
		req.Force = true
	}
	// Only pin a specific chat when --chat is given; otherwise leave nil so the
	// daemon targets the session's primary live chat.
	if cmd.Flags().Changed("chat") {
		chat, _ := cmd.Flags().GetString("chat")
		req.AgentSessionId = proto.String(chat)
	}

	resp, err := c.SwitchSessionAccount(cmd.Context(), req)
	if err != nil {
		// The daemon's mid-turn rejection is FailedPrecondition and its message
		// already asks to confirm/--force; surface it cleanly. Add the --force
		// nudge when the caller has not already passed it.
		if force, _ := cmd.Flags().GetBool("force"); connect.CodeOf(err) == connect.CodeFailedPrecondition && !force {
			return fmt.Errorf("switch account: %w\nre-run with --force to interrupt a mid-turn (WORKING) chat", err)
		}
		return fmt.Errorf("switch account: %w", err)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), resp.GetNoticeText())
	return nil
}

func runAccountRemove(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	if err := c.RemoveAccount(cmd.Context(), id); err != nil {
		return fmt.Errorf("remove account: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed account %s\n", id)
	return nil
}

func runAccountTest(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	resp, err := c.TestAccount(cmd.Context(), id)
	if err != nil {
		return fmt.Errorf("test account: %w", err)
	}

	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		out := struct {
			Account      accountJSON `json:"account"`
			LiveSmokeRan bool        `json:"live_smoke_ran"`
			Detail       string      `json:"detail"`
		}{
			Account:      accountToJSON(resp.GetAccount()),
			LiveSmokeRan: resp.GetLiveSmokeRan(),
			Detail:       resp.GetDetail(),
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal test result: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}

	acct := resp.GetAccount()
	result := "ok"
	if acct.GetLastTestError() != "" {
		result = "error: " + acct.GetLastTestError()
	}
	out := cmd.OutOrStdout()
	_, _ = fmt.Fprintf(out, "Account %s: %s\n", id, result)
	_, _ = fmt.Fprintf(out, "Live smoke ran: %s\n", boolLabel(resp.GetLiveSmokeRan()))
	if detail := resp.GetDetail(); detail != "" {
		_, _ = fmt.Fprintf(out, "Detail: %s\n", detail)
	}
	return nil
}

// readCredentialFlag resolves the credential blob for `account add`. The token
// is sourced ONLY from --token, --credential-file (a path, or "-" for stdin);
// it is never echoed. Exactly one source must be provided.
func readCredentialFlag(cmd *cobra.Command) ([]byte, error) {
	tokenSet := cmd.Flags().Changed("token")
	fileSet := cmd.Flags().Changed("credential-file")

	if tokenSet && fileSet {
		return nil, fmt.Errorf("cannot use both --token and --credential-file")
	}
	if !tokenSet && !fileSet {
		return nil, fmt.Errorf("a credential source is required: one of --token or --credential-file (use --credential-file - for stdin)")
	}

	if tokenSet {
		v, _ := cmd.Flags().GetString("token")
		if v == "" {
			return nil, fmt.Errorf("--token must not be empty")
		}
		return []byte(v), nil
	}

	path, _ := cmd.Flags().GetString("credential-file")
	if path == "-" {
		b, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, fmt.Errorf("read credential from stdin: %w", err)
		}
		return b, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credential file: %w", err)
	}
	return b, nil
}

// trimmedProviderList renders the accepted providers for help text.
func trimmedProviderList() string {
	return strings.Join([]string{"claude", "codex"}, "|")
}
