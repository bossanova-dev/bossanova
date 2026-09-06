package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// accountJSON is the stable, documented schema emitted by `boss account ls
// --json` and `boss account test --json`. Field names are part of the machine
// contract that scripts depend on: renames are breaking changes. Timestamps are
// RFC3339 strings, empty when the underlying timestamp is nil/zero. There is
// NO credential field — the secret blob never crosses this boundary.
type accountJSON struct {
	ID             string   `json:"id"`
	Provider       string   `json:"provider"`
	Label          string   `json:"label"`
	Status         string   `json:"status"`
	Priority       int32    `json:"priority"`
	Health         string   `json:"health"`
	Tier           string   `json:"tier"`
	AllowedModels  []string `json:"allowed_models"`
	CooldownUntil  string   `json:"cooldown_until"`
	LastUsedAt     string   `json:"last_used_at"`
	LastTestOkAt   string   `json:"last_test_ok_at"`
	LastTestError  string   `json:"last_test_error"`
	CreatedAt      string   `json:"created_at"`
	UpdatedAt      string   `json:"updated_at"`
	Util5h         float64  `json:"util_5h"`
	Reset5h        string   `json:"reset_5h"`
	Util7d         float64  `json:"util_7d"`
	Reset7d        string   `json:"reset_7d"`
	UsageStatus    string   `json:"usage_status"`
	PlanTier       string   `json:"plan_tier"`
	UsageFetchedAt string   `json:"usage_fetched_at"`
	// Credential-check state (BOS-1141/BOS-1142). These are deliberately three
	// separate fields rather than one rolled-up string: "never checked" and
	// "checked, found nothing wrong" are opposite facts, and a consumer that
	// only sees an empty diagnostic cannot tell them apart. AuthCheckedAt
	// empty means the check never ran.
	AuthCheckOutcome      string `json:"auth_check_outcome"`
	AuthCheckFailureClass string `json:"auth_check_failure_class"`
	AuthCheckedAt         string `json:"auth_checked_at"`
}

// accountToJSON maps a proto Account to the stable JSON schema.
func accountToJSON(a *pb.Account) accountJSON {
	out := accountJSON{
		ID:            a.GetId(),
		Provider:      a.GetProvider(),
		Label:         a.GetLabel(),
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

		AuthCheckOutcome:      a.GetAuthCheck().GetOutcome(),
		AuthCheckFailureClass: a.GetAuthCheck().GetFailureClass(),
		AuthCheckedAt:         rfc3339OrEmpty(a.GetAuthCheck().GetCheckedAt()),
	}
	if u := a.GetUsage(); u != nil {
		out.Util5h = u.GetUtil_5H()
		out.Reset5h = rfc3339OrEmpty(u.GetReset_5H())
		out.Util7d = u.GetUtil_7D()
		out.Reset7d = rfc3339OrEmpty(u.GetReset_7D())
		out.UsageStatus = u.GetStatus()
		out.PlanTier = u.GetPlanTier()
		out.UsageFetchedAt = rfc3339OrEmpty(u.GetFetchedAt())
	}
	return out
}

// validAccountProviders is the set of providers `boss account add` accepts. It
// mirrors the agent-runner providers the daemon knows about (claude, codex).
var validAccountProviders = map[string]bool{"claude": true, "codex": true}

func runAccountLS(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	return accountLS(cmd, c)
}

type accountLister interface {
	ListAccounts(ctx context.Context, provider string, refresh bool) ([]*pb.Account, error)
}

func accountLS(cmd *cobra.Command, c accountLister) error {
	ctx := cmd.Context()

	provider, _ := cmd.Flags().GetString("provider")
	refresh, _ := cmd.Flags().GetBool("refresh")
	accounts, err := c.ListAccounts(ctx, provider, refresh)
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
	statuses := make([]string, len(accounts))
	healths := make([]string, len(accounts))
	util5hs := make([]string, len(accounts))
	util7ds := make([]string, len(accounts))
	usageStatuses := make([]string, len(accounts))
	usageAges := make([]string, len(accounts))
	cooldowns := make([]string, len(accounts))
	checks := make([]string, len(accounts))
	checkAges := make([]string, len(accounts))
	for i, a := range accounts {
		ids[i] = a.GetId()
		providers[i] = orDash(a.GetProvider())
		labels[i] = orDash(a.GetLabel())
		statuses[i] = orDash(a.GetStatus())
		healths[i] = fmtAccountHealth(a)
		util5hs[i] = fmtUtil(a.GetUsage(), a.GetUsage().GetUtil_5H(), a.GetUsage().GetReset_5H())
		util7ds[i] = fmtUtil(a.GetUsage(), a.GetUsage().GetUtil_7D(), a.GetUsage().GetReset_7D())
		usageStatuses[i] = fmtUsageStatus(a.GetUsage())
		usageAges[i] = fmtUsageAge(a.GetUsage())
		cooldowns[i] = fmtCooldown(a.GetCooldownUntil())
		checks[i] = fmtAuthCheck(a.GetAuthCheck())
		checkAges[i] = fmtAuthCheckAge(a.GetAuthCheck())
	}

	cols := []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "PROVIDER", Width: views.MaxColWidth("PROVIDER", providers, 8)},
		{Title: "LABEL", Width: views.MaxColWidth("LABEL", labels, 24)},
		{Title: "STATUS", Width: views.MaxColWidth("STATUS", statuses, 10)},
		{Title: "HEALTH", Width: views.MaxColWidth("HEALTH", healths, 8)},
		{Title: "UTIL5H", Width: views.MaxColWidth("UTIL5H", util5hs, 12)},
		{Title: "UTIL7D", Width: views.MaxColWidth("UTIL7D", util7ds, 12)},
		{Title: "USAGE", Width: views.MaxColWidth("USAGE", usageStatuses, 8)},
		{Title: "AGE", Width: views.MaxColWidth("AGE", usageAges, 6)},
		{Title: "COOLDOWN", Width: views.MaxColWidth("COOLDOWN", cooldowns, 10)},
		// 24 fits the longest "failed:<class>" the daemon emits; truncating the
		// class would make two different diagnoses render identically.
		{Title: "CHECK", Width: views.MaxColWidth("CHECK", checks, 24)},
		{Title: "CHECKED", Width: views.MaxColWidth("CHECKED", checkAges, 8)},
	}

	rows := make([]table.Row, len(accounts))
	for i := range accounts {
		rows[i] = table.Row{ids[i], providers[i], labels[i], statuses[i], healths[i], util5hs[i], util7ds[i], usageStatuses[i], usageAges[i], cooldowns[i], checks[i], checkAges[i]}
	}

	_, _ = fmt.Fprintln(cmd.OutOrStdout(), views.RenderCLITable(cols, rows))

	// The mark on a vetoed HEALTH cell is deliberately one rune wide — HEALTH is
	// early in a ~110-column fixed table and CHECK is at the far right, so on an
	// 80-column terminal the contradicting evidence is scrolled off screen and
	// the mark is all that is left of it. One legend line, printed only when a
	// mark is actually on the table, spells it back out.
	if legend := accountHealthLegend(healths); legend != "" {
		_, _ = fmt.Fprintln(cmd.OutOrStdout())
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), legend)
	}

	// BOS-327: steer the operator to the real remedy when a provider has accounts
	// but none ELIGIBLE — rotation cannot switch onto a disabled or unhealthy
	// account, and the rotation history's "no eligible account — status only" line
	// points here. The JSON path returns above, so this never pollutes the pure
	// array.
	if hints := noEligibleAccountHints(accounts); len(hints) > 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout()) // blank line separates the hint from the table
		for _, hint := range hints {
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), hint)
		}
	}
	return nil
}

// noEligibleAccountHints returns one hint line per provider that has ≥1 account
// but ZERO eligible to rotate to — where eligible means STATUS=="active" AND
// HEALTH=="ok", mirroring the engine's isSelectable predicate
// (services/bossd/internal/rotation/engine.go). Computed client-side from the
// already-listed accounts (no daemon-config plumbing); providers are emitted in
// first-seen order for deterministic output. Rotation cannot switch onto a
// disabled or unhealthy account, so this surfaces the remedy where the operator
// already looks (BOS-327).
//
// Gating on eligibility (not merely status) keeps the hint consistent with the
// rotation audit: an account that is active but health=="failed" is NOT eligible,
// so the engine still reports "no eligible account" — the hint must fire there
// too rather than staying silent because a nominally-active row exists.
//
// The "active"/"ok" literals mirror lib/bossalib/models.AccountStatusActive and
// AccountHealthOK, which this module cannot import across the boundary (same
// mirror the TUI keeps in internal/views/account_actions.go); the daemon
// validates the same values in services/bossd/internal/server/account.go.
func noEligibleAccountHints(accounts []*pb.Account) []string {
	const (
		accountStatusActive = "active"
		accountHealthOK     = "ok"
	)
	order := make([]string, 0, len(accounts))
	hasEligible := make(map[string]bool)
	seen := make(map[string]bool)
	for _, a := range accounts {
		p := a.GetProvider()
		if !seen[p] {
			seen[p] = true
			order = append(order, p)
		}
		if a.GetStatus() == accountStatusActive && a.GetHealth() == accountHealthOK {
			hasEligible[p] = true
		}
	}
	hints := make([]string, 0, len(order))
	for _, p := range order {
		if hasEligible[p] {
			continue
		}
		hints = append(hints, fmt.Sprintf(
			"Hint: provider %q has no eligible account — rotation cannot switch. Enable or re-authenticate one, e.g. boss account update <id> --status active",
			p))
	}
	return hints
}

// fmtDurationShort renders a non-negative duration as a single s/m/h/d bucket.
// Shared by AGE (elapsed), UTIL reset countdowns, and COOLDOWN (remaining).
func fmtDurationShort(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// fmtCooldown renders the time remaining before a benched account becomes
// selectable again, e.g. "in 47m". "-" means not cooling (nil or already past).
func fmtCooldown(ts *timestamppb.Timestamp) string {
	if ts == nil {
		return "-"
	}
	if d := time.Until(ts.AsTime()); d > 0 {
		return "in " + fmtDurationShort(d)
	}
	return "-"
}

// fmtUtil renders a utilization percentage, appending the window's reset
// countdown when known, e.g. "93% (5d)". Returns "-" when the probe carries no
// usable signal (nil, never probed, or unsupported/unspecified status).
func fmtUtil(u *pb.UsageSnapshot, pct float64, reset *timestamppb.Timestamp) string {
	if u == nil || u.GetFetchedAt() == nil || usageSnapshotUnsupported(u.GetStatus()) {
		return "-"
	}
	s := fmt.Sprintf("%.0f%%", pct*100)
	if reset != nil {
		if d := time.Until(reset.AsTime()); d > 0 {
			s += " (" + fmtDurationShort(d) + ")"
		}
	}
	return s
}

// fmtUsageStatus maps the rate-limit status (the RateLimitPlanStatus enum name
// from the probe, or an already-short token) to a compact display token.
func fmtUsageStatus(u *pb.UsageSnapshot) string {
	if u == nil || u.GetFetchedAt() == nil {
		return "-"
	}
	s := strings.ToLower(strings.TrimSpace(u.GetStatus()))
	s = strings.TrimPrefix(s, "rate_limit_plan_status_")
	switch s {
	case "rate_limited", "limited":
		return "limited"
	case "warning", "warn":
		return "warn"
	case "active", "ok":
		return "ok"
	default:
		return "-"
	}
}

// usageSnapshotUnsupported reports whether a probe could not authoritatively
// determine utilization, so the CLI should render "-" instead of a fabricated
// number. It mirrors rotation.UsageSnapshotProbeUnavailable (which this package
// cannot import across the module boundary): both UNSUPPORTED and UNSPECIFIED
// mean "no usable signal".
func usageSnapshotUnsupported(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "unsupported", "rate_limit_plan_status_unsupported",
		"unspecified", "rate_limit_plan_status_unspecified":
		return true
	default:
		return false
	}
}

func fmtUsageAge(u *pb.UsageSnapshot) string {
	if u == nil || u.GetFetchedAt() == nil {
		return "-"
	}
	return fmtDurationShort(time.Since(u.GetFetchedAt().AsTime()))
}

// fmtAuthCheck renders the credential-check verdict for the ls table.
//
// The critical case is the empty one: an account nobody has ever checked and an
// account checked five minutes ago with nothing wrong are opposite facts, and
// collapsing both to "-" is the BOS-892 defect. "never" says the observation
// did not happen; "ok" says it happened and found nothing.
func fmtAuthCheck(ac *pb.AuthCheck) string {
	switch ac.GetOutcome() {
	case "":
		return "never"
	case "healthy":
		// credential_superseded is the one class that qualifies a HEALTHY
		// verdict rather than a failing one: the provider accepted the stored
		// credential, but an ambient codex login for the same provider account
		// holds a different refresh token, so the stored chain is already dead
		// (BOS-1175). "ok" keeps its head because eligibility genuinely is
		// unaffected; dropping the class would render a superseded credential
		// identically to a clean one, which is the state the daemon's
		// credcheck.go comment says both client mirrors render.
		//
		// Matched by EXACT value, mirroring views.accountCheckLabel: an
		// unrecognised class on a healthy outcome stays a bare "ok" so a newer
		// daemon cannot make this build render a warning it cannot explain.
		if ac.GetFailureClass() == views.AuthCheckClassSuperseded {
			return "ok:" + views.AuthCheckClassSuperseded
		}
		return "ok"
	case "auth_invalid":
		// The failure class is already a short redacted token, never a raw
		// provider message, so it is safe to render verbatim.
		if fc := ac.GetFailureClass(); fc != "" {
			return "failed:" + fc
		}
		return "failed"
	case "refresh_chain_unproven":
		// The outcome token is the whole message: "refresh_not_observed"
		// restates it rather than naming a different remedy, and the pair is 43
		// columns against the 24-column CHECK budget below, so appending the
		// class would spend the truncation on the identifying half. Mirrors the
		// TUI's accountCheckLabel (internal/views/account_health.go).
		return ac.GetOutcome()
	default:
		if fc := ac.GetFailureClass(); fc != "" {
			return ac.GetOutcome() + ":" + fc
		}
		return ac.GetOutcome()
	}
}

// Marks a vetoed HEALTH cell wears in `boss account ls`. They mirror the TUI's
// healthVetoInvalidMark / healthVetoUnprovenMark (internal/views/account_health.go)
// and, like them, are one rune because the HEALTH column is sized from its own
// values — a word would only be truncated back off.
const (
	healthVetoInvalidMark  = "!"
	healthVetoUnprovenMark = "?"
	// healthSupersededMark mirrors views.healthSupersededMark. Distinct from the
	// two veto marks because it is not a veto: the check PASSED and the account
	// is still eligible. It marks HEALTH as well as CHECK because CHECK sits at
	// the far right of a ~110-column table and scrolls off at 80 (see
	// fmtAccountHealth) — a state only that column carries is one an operator on
	// a narrow terminal never sees.
	healthSupersededMark = "~"
)

// fmtAccountHealth renders the HEALTH cell with the same never-green veto both
// richer surfaces apply: the TUI's accountHealthCellFor and the web's
// accountHealthStatus. `boss account ls` prints a fixed ~110-column table with
// no colour and no responsive fold, and it appends CHECK/CHECKED at the FAR
// RIGHT — so at 80 columns a row that reads a bare "ok" has its contradicting
// evidence scrolled off screen entirely, which is the one place the "green
// while broken" state survived. Marking HEALTH itself puts the honest state in
// a column that is actually visible.
//
// A confirmed-invalid check and an undetermined one get DIFFERENT marks: the
// first indicts the credential and names a remedy, the second only says nothing
// verified it. A never-checked account keeps its unmarked health, exactly as on
// the other two surfaces — it is the normal state of a freshly registered
// account, and marking it would spend the signal until it meant nothing.
func fmtAccountHealth(a *pb.Account) string {
	health := orDash(a.GetHealth())
	if health != "ok" {
		return health
	}
	switch a.GetAuthCheck().GetOutcome() {
	case "healthy":
		// Matched by EXACT class value, and only on a HEALTHY outcome: a class is
		// an answer only where the daemon actually asked, and an unrecognised
		// class keeps the plain health so a newer daemon cannot make this CLI
		// mark a row it cannot explain.
		if a.GetAuthCheck().GetFailureClass() == views.AuthCheckClassSuperseded {
			return health + healthSupersededMark
		}
		return health
	case "":
		// Never checked: nothing was asked, so no class this row happens to
		// carry describes an answer.
		return health
	case "auth_invalid":
		return health + healthVetoInvalidMark
	default:
		// transient, unavailable, and any outcome a newer daemon adds: the check
		// reached no verdict, so it cannot vouch for the credential. Unknown
		// folds here rather than into the accusation above — a newer daemon must
		// not be able to make an older CLI indict a credential it cannot
		// classify.
		return health + healthVetoUnprovenMark
	}
}

// accountHealthLegend returns the legend for whichever veto marks the rendered
// HEALTH column actually carries, or "" when it carries none.
func accountHealthLegend(healths []string) string {
	var invalid, unproven, superseded bool
	for _, h := range healths {
		switch {
		case strings.HasSuffix(h, healthVetoInvalidMark):
			invalid = true
		case strings.HasSuffix(h, healthVetoUnprovenMark):
			unproven = true
		case strings.HasSuffix(h, healthSupersededMark):
			superseded = true
		}
	}
	var parts []string
	if invalid {
		parts = append(parts, healthVetoInvalidMark+" health says ok, but the last credential check found the credential rejected — see CHECK, then `boss account reauth <id>`")
	}
	if unproven {
		parts = append(parts, healthVetoUnprovenMark+" health says ok, but the last credential check did not establish it — see CHECK for what it did or did not conclude")
	}
	if superseded {
		parts = append(parts, healthSupersededMark+" health says ok and the credential still works, but an ambient `codex login` has replaced the stored refresh token — this account will stop working when its access token expires; run `boss account reauth <id>`")
	}
	return strings.Join(parts, "\n")
}

// fmtAuthCheckAge renders how long ago the credential check ran. A check that
// never ran has no age, and rendering one would be a fabrication.
func fmtAuthCheckAge(ac *pb.AuthCheck) string {
	if ac.GetCheckedAt() == nil {
		return "-"
	}
	return fmtDurationShort(time.Since(ac.GetCheckedAt().AsTime()))
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

	priority, _ := cmd.Flags().GetInt32("priority")

	req := &pb.AddAccountRequest{
		Provider:   provider,
		Label:      label,
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

// accountRefresher is the narrow client surface `boss account refresh` needs.
// The real client satisfies it; tests inject a fake.
type accountRefresher interface {
	RefreshAccount(ctx context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error)
}

func runAccountRefresh(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	// RefreshAccount is on the client interface itself now (BOS-1142), so this
	// no longer needs a capability type-assertion.
	return accountRefresh(cmd, c, id)
}

func accountRefresh(cmd *cobra.Command, c accountRefresher, id string) error {
	credential, err := readCredentialFlag(cmd)
	if err != nil {
		return err
	}
	testAfterSave, _ := cmd.Flags().GetBool("test")
	resp, err := c.RefreshAccount(cmd.Context(), &pb.RefreshAccountRequest{
		Id:            id,
		Credential:    credential,
		TestAfterSave: testAfterSave,
	})
	if err != nil {
		return fmt.Errorf("refresh account: %w", err)
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
			return fmt.Errorf("marshal refresh result: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}

	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Refreshed account %s\n", id)
	if testAfterSave && resp.GetDetail() != "" {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Detail: %s\n", resp.GetDetail())
	}
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
		return fmt.Errorf("no flags provided — use --label, --priority, --status, or --allowed-models")
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
	_, _ = fmt.Fprintf(out, "Provider check ran: %s\n", boolLabel(resp.GetLiveSmokeRan()))
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
	// #nosec G304 -- reads the operator-supplied --credential-file; reading the named file is the feature, operator/local-trust, secret-adjacent.
	// owner=@recurser review-by=2027-01-18 issue=BOS-28
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
