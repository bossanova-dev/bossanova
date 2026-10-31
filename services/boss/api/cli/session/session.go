package session

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/recurser/boss/internal/domain/rpc"

	"github.com/recurser/boss/api/cli/cliutil"

	"charm.land/bubbles/v2/table"
	"github.com/recurser/boss/internal/domain/display"

	"charm.land/lipgloss/v2"
	"github.com/recurser/boss/internal/domain/rpcerr"
	"github.com/spf13/cobra"

	"github.com/recurser/bossalib/kit/config"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func (g *Group) runLS(cmd *cobra.Command) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	allDaemons, _ := cmd.Flags().GetBool("all-daemons")
	if allDaemons && (commandFlag(cmd, "remote") != "" || commandFlag(cmd, "host") != "") {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("--all-daemons cannot be combined with --remote or --host"))
	}

	var c Client
	var err error
	if allDaemons {
		c, err = g.NewRemoteClient(cmd, cliutil.CloudURL(cmd))
	} else {
		c, err = g.newClient(cmd)
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	repoID, _ := cmd.Flags().GetString("repo")
	archived, _ := cmd.Flags().GetBool("archived")
	stateStrs, _ := cmd.Flags().GetStringSlice("state")

	// Parse state filters.
	var states []pb.SessionState
	for _, s := range stateStrs {
		key := "SESSION_STATE_" + strings.ToUpper(s)
		if val, ok := pb.SessionState_value[key]; ok {
			states = append(states, pb.SessionState(val))
		} else {
			return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("unknown state: %s", s))
		}
	}

	req := &pb.ListSessionsRequest{
		IncludeArchived: archived,
		States:          states,
	}
	if repoID != "" {
		req.RepoId = &repoID
	}

	ctx := context.Background()
	var sessions []*pb.Session
	var failures []*pb.OrganizationSessionReadFailure
	if allDaemons {
		sessions, failures, err = c.ListSessionsWithReadFailures(ctx, req, rpc.SessionReadOptions{})
	} else {
		sessions, err = c.ListSessions(ctx, req, rpc.SessionReadOptions{})
	}
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("list sessions: %w", err))
	}

	if asJSON {
		// The empty case emits `{"sessions": []}`, not the human "no sessions"
		// line and not `null` — a driver decodes one shape either way.
		rows := make([]sessionJSON, 0, len(sessions))
		for _, sess := range sessions {
			rows = append(rows, newSessionRowJSON(sess))
		}
		result := sessionListJSON{Sessions: rows}
		for _, failure := range failures {
			result.FailedOrganizations = append(result.FailedOrganizations, organizationReadFailureJSON{
				OrganizationID: failure.GetOrganizationId(), Reason: failure.GetReason(),
			})
		}
		return cliutil.EmitJSON(cmd, result)
	}
	for _, failure := range failures {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: organization %s: %s\n", failure.GetOrganizationId(), failure.GetReason())
	}

	if len(sessions) == 0 {
		fmt.Println("No sessions found.")
		return nil
	}

	ids := make([]string, len(sessions))
	titles := make([]string, len(sessions))
	stateStrs2 := make([]string, len(sessions))
	branchStrs := make([]string, len(sessions))
	prStrs := make([]string, len(sessions))
	for i, sess := range sessions {
		ids[i] = sess.Id
		t := sess.Title
		t = cliutil.TruncateString(t, 30)
		titles[i] = t
		stateStrs2[i] = display.StateLabel(sess.State)
		branchStrs[i] = sess.BranchName
		if sess.PrNumber != nil {
			prText := fmt.Sprintf("#%d", *sess.PrNumber)
			if sess.PrUrl != nil {
				prStrs[i] = lipgloss.NewStyle().Hyperlink(*sess.PrUrl).Render(prText)
			} else {
				prStrs[i] = prText
			}
		} else {
			prStrs[i] = "-"
		}
	}

	cols := []table.Column{
		{Title: "ID", Width: display.MaxColWidth("ID", ids, 0)},
		{Title: "TITLE", Width: display.MaxColWidth("TITLE", titles, 30)},
		{Title: "STATE", Width: display.MaxColWidth("STATE", stateStrs2, 14)},
		{Title: "BRANCH", Width: display.MaxColWidth("BRANCH", branchStrs, 40)},
		{Title: "PR", Width: display.MaxColWidth("PR", prStrs, 8)},
	}

	rows := make([]table.Row, len(sessions))
	for i := range sessions {
		rows[i] = table.Row{ids[i], titles[i], stateStrs2[i], branchStrs[i], prStrs[i]}
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

func (g *Group) runNew(cmd *cobra.Command) error {
	agentName, _ := cmd.Flags().GetString("agent")
	repo, _ := cmd.Flags().GetString("repo")
	prompt, _ := cmd.Flags().GetString("prompt")
	title, _ := cmd.Flags().GetString("title")
	model, _ := cmd.Flags().GetString("model")
	effort, _ := cmd.Flags().GetString("effort")
	account, _ := cmd.Flags().GetString("account")
	// Thread flag PRESENCE, not just the value: an explicit `--account=` (even
	// empty) is an opt-out to account 0, distinct from omitting the flag (the
	// daemon then applies its default-account policy). nil = absent.
	var accountArg *string
	if cmd.Flags().Changed("account") {
		a := account
		accountArg = &a
	}
	// --detach / --no-attach are deliberately NOT read. The scripting path below
	// always detaches — runNewDetach's predecessor already took the value as `_`
	// — and the interactive path launches the TUI regardless, so the flags are
	// inert on both. They stay registered because they are a documented surface
	// callers already pass; `--detach`'s help text is what says it is a no-op
	// here and points at --tmux-unattended for the durable-pane behaviour.
	tmuxUnattended, _ := cmd.Flags().GetBool("tmux-unattended")
	deferPR, _ := cmd.Flags().GetBool("defer-pr")
	quickChat, _ := cmd.Flags().GetBool("quick-chat")
	verifyOnly, _ := cmd.Flags().GetBool("verify-only")
	trackerID, _ := cmd.Flags().GetString("tracker-id")
	trackerSource, _ := cmd.Flags().GetString("tracker-source")
	trackerURL, _ := cmd.Flags().GetString("tracker-url")
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)
	// --pr carries flag PRESENCE: nil = absent (a fresh branch), non-nil = the
	// daemon's existing-pr path, which checks out that PR's head and binds it.
	var prNumber *int32
	if cmd.Flags().Changed("pr") {
		n, _ := cmd.Flags().GetInt32("pr")
		prNumber = &n
	}

	// Reject an unknown --tracker-source before any RPC is issued, on BOTH the
	// interactive and the scripting path. The daemon neither validates nor
	// stores the field — there is no tracker_source column, and server.go reads
	// it exactly once, to label a plan derived from a tracker_issue — so an
	// unrecognised value is silently ignored rather than refused. Failing here
	// turns a typo into an error the caller can see, instead of a tracker
	// binding that quietly never happens.
	if err := validateTrackerSource(trackerSource); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	// Disclose a displaced plugin default. resolveClaudeModel (and its siblings)
	// are request-wins by design, and the plugin-level pin exists BECAUSE an
	// unpinned run resolves non-deterministically — so an explicit --model does
	// not merely choose a model, it opts the session out of the host's pin,
	// which can change the context window it gets. Nothing said so. This only
	// reports; the resolution order is untouched.
	if cmd.Flags().Changed("model") && model != "" {
		settings, _ := g.LoadSettings()
		if configured := configuredPluginModel(settings, agentName); configured != "" && configured != model {
			// stderr on BOTH paths: the two-line stdout is a frozen scripting
			// surface and under --json stdout must stay exactly one JSON object.
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
				"notice: --model %s displaces the configured %s plugin default %s for this session; "+
					"the host's pinned model does not apply to it\n",
				model, configuredPluginModelAgent(settings, agentName), configured)
		}
	}

	// --quick-chat and --defer-pr are contradictory, so refuse the pair here —
	// before any RPC — rather than sending a request whose two fields ask the
	// daemon for incompatible shapes. A quick chat has no worktree, no branch and
	// no up-front draft PR (server.go routes it to StartQuickChatSession, which
	// never enters StartSession), so there is nothing for --defer-pr to defer:
	// the daemon would silently honour quick_chat and drop defer_pr, leaving the
	// caller believing it asked for something it did not get.
	if quickChat && deferPR {
		return cliutil.EmitJSONFailure(cmd, asJSON, cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
			"--quick-chat and --defer-pr are mutually exclusive: a quick chat has no "+
				"worktree, branch, or draft PR to defer; use --defer-pr for an "+
				"unattended run that may produce commits")))
	}

	// --pr targets an existing PR, which contradicts both a quick chat (no
	// branch at all) and a deferred PR (a PR that does not exist yet). Refuse
	// before any RPC, as with the pair above, rather than letting the daemon
	// silently pick one shape.
	if err := validatePRFlag(prNumber, quickChat, deferPR, repo != "" && prompt != ""); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	if verifyOnly && prNumber == nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
			"--verify-only requires --pr")))
	}

	// Non-interactive path: --repo and --prompt both provided.
	if repo != "" && prompt != "" {
		return g.runNewDetach(cmd, newSessionOpts{
			RepoID:         repo,
			Prompt:         prompt,
			Title:          title,
			AgentName:      agentName,
			Model:          model,
			Effort:         effort,
			Account:        accountArg,
			TmuxUnattended: tmuxUnattended,
			DeferPR:        deferPR,
			QuickChat:      quickChat,
			VerifyOnly:     verifyOnly,
			TrackerID:      trackerID,
			TrackerSource:  trackerSource,
			TrackerURL:     trackerURL,
			PrNumber:       prNumber,
		}, asJSON)
	}

	return g.LaunchTUI(cmd, LaunchOptions{NewSession: true, Agent: agentName, Account: accountArg})
}

// runNewDetach creates a session non-interactively, drains the setup stream,
// and prints the session-id and chat-id (agent_session_id) before returning.
// When detach is false the user can still pipe this output, but the session
// starts running in the background either way.
// newDetachRequest builds the CreateSessionRequest for the non-interactive
// `boss new --repo --prompt` scripting path. Detach is always true here: the
// prompt runs as a headless agent pass (codex exec / claude --print) on the
// daemon. The local --detach flag only governs whether this CLI waits/attaches
// after creating the session; interactive TUI sessions leave Detach false and
// start the agent on attach instead. agentName and model are optional
// (empty → server/agent default) and stay unset in the request when empty.
// account is optional and carries flag PRESENCE: nil omits account_id (the
// daemon applies its default-account policy), a non-nil pointer (even to "")
// binds explicitly — "" is the account-0 opt-out that skips the policy.
//
// It takes a named options struct rather than a positional parameter list:
// the list was already at six arguments and the tracker/tmux flags would have
// taken it to ten same-typed strings and bools, where a transposed pair is
// invisible at the call site and silently sends the URL as the id.
func newDetachRequest(opts newSessionOpts) *pb.CreateSessionRequest {
	req := &pb.CreateSessionRequest{
		RepoId: opts.RepoID,
		// Verbatim. The daemon only auto-submits an unattended prompt when it is
		// one trimmed line starting with `/` or `$`, so the CLI must not wrap,
		// prefix, trim or reflow it on the way in.
		Plan:  opts.Prompt,
		Title: opts.Title,
		// Detach and IsTmuxUnattended are orthogonal, not alternatives: the
		// daemon carries both on StartSessionOpts and mints a finalize hook
		// token when EITHER is set. Detach means "run the initial agent pass
		// headlessly"; is_tmux_unattended means "host it in a durable tmux pane
		// that survives a daemon restart". --tmux-unattended therefore adds to
		// this scripting path's always-true Detach rather than replacing it.
		Detach:           true,
		IsTmuxUnattended: opts.TmuxUnattended,
		// Plain proto bools use false for "not requested"; unlike --account,
		// they need no presence pointer. Preserve Detach: it mints the finalize
		// hook token, which opens a deferred PR when the run produces commits.
		// Paneless runs reach finalize through the run-completion poller.
		DeferPr:      opts.DeferPR,
		IsQuickChat:  opts.QuickChat,
		IsVerifyOnly: opts.VerifyOnly,
	}
	if opts.AgentName != "" {
		req.AgentName = &opts.AgentName
	}
	if opts.Model != "" {
		req.Model = &opts.Model
	}
	if opts.Effort != "" {
		req.Effort = &opts.Effort
	}
	if opts.Account != nil {
		req.AccountId = opts.Account
	}
	// The tracker fields are `optional` in the proto, so an omitted flag must
	// leave the field nil: a present-but-empty pointer is a different wire
	// signal, and the daemon's tracker-id dedup keys on presence.
	if opts.TrackerID != "" {
		req.TrackerId = &opts.TrackerID
	}
	if opts.TrackerSource != "" {
		req.TrackerSource = &opts.TrackerSource
	}
	if opts.TrackerURL != "" {
		req.TrackerUrl = &opts.TrackerURL
	}
	// pr_number is `optional`: nil keeps the fresh-branch path, a value selects
	// the daemon's existing-pr path.
	if opts.PrNumber != nil {
		n := *opts.PrNumber
		req.PrNumber = &n
	}
	return req
}

// newSessionOpts carries the `boss new` non-interactive flag set through to the
// CreateSessionRequest builder. Named fields, not positional arguments — see
// newDetachRequest.
type newSessionOpts struct {
	RepoID    string
	Prompt    string
	Title     string
	AgentName string
	Model     string
	Effort    string
	// Account carries flag PRESENCE: nil = absent, non-nil (even "") = explicit.
	Account *string
	// TmuxUnattended requests a durable tmux-hosted pane that survives a daemon
	// restart. Independent of Detach — see newDetachRequest.
	TmuxUnattended bool
	// DeferPR opens a PR at finalize only if the run committed work, while
	// still creating a worktree and branch. No-op runs leave no empty PR.
	DeferPR bool
	// QuickChat creates a session in the repo checkout without a worktree,
	// branch, PR or initial agent run. runNewDetach reports the absent chat id
	// on stderr and in JSON next_action. runNew rejects pairing with DeferPR.
	QuickChat bool
	// VerifyOnly prevents automatic repair for a session adopted to verify a PR.
	VerifyOnly bool
	// Tracker* bind the session to an external issue. Empty = flag absent, which
	// leaves the corresponding optional proto field unset.
	TrackerID     string
	TrackerSource string
	TrackerURL    string
	// PrNumber binds an existing PR's head; nil creates a fresh branch.
	PrNumber *int32
}

// validatePRFlag rejects a --pr the request cannot honour: a non-positive
// number, a pairing with --quick-chat or --defer-pr, or the interactive path,
// which has no PR picker to carry it.
func validatePRFlag(pr *int32, quickChat, deferPR, nonInteractive bool) error {
	if pr == nil {
		return nil
	}
	switch {
	case *pr <= 0:
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf("--pr must be a positive pull request number, got %d", *pr))
	case quickChat:
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
			"--pr and --quick-chat are mutually exclusive: a quick chat has no branch to check the PR out on"))
	case deferPR:
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
			"--pr and --defer-pr are mutually exclusive: --pr names a PR that already exists"))
	case !nonInteractive:
		return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
			"--pr is supported only on the non-interactive --repo + --prompt path"))
	}
	return nil
}

// trackerSources is the tracker_source vocabulary the proto documents
// (proto/bossanova/v1/daemon.proto: `optional string tracker_source = 13; //
// "linear" | "sentry"`).
var trackerSources = []string{"linear", "sentry"}

// validateTrackerSource rejects an unknown --tracker-source. The daemon does
// not validate this field — it stores it and uses it only to format a plan from
// a tracker issue — so an unrecognised value is persisted verbatim and the
// binding it was meant to establish silently does not happen. Catching it here
// keeps the failure at the call the caller can still fix.
//
// An empty value is valid: it means the flag was omitted.
func validateTrackerSource(source string) error {
	if source == "" || slices.Contains(trackerSources, source) {
		return nil
	}
	return cliutil.CodedError(cliutil.CodeInvalidArgument, fmt.Errorf(
		"invalid --tracker-source %q: want one of %s",
		source, strings.Join(trackerSources, ", ")))
}

// deriveSessionTitle builds a session title from the prompt for the
// non-interactive `boss new` path, mirroring the interactive TUI which always
// supplies a client-side title (newsession.go). It takes the first non-empty
// line, collapses internal whitespace runs, and caps the length on a rune
// boundary. A whitespace-only prompt falls back to a stable default so the
// server's "title is required" invariant is always satisfied.
func deriveSessionTitle(prompt string) string {
	const maxTitleRunes = 72
	const fallback = "New session"
	var line string
	for _, l := range strings.Split(prompt, "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
			break
		}
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return fallback
	}
	runes := []rune(line)
	if len(runes) > maxTitleRunes {
		return strings.TrimSpace(string(runes[:maxTitleRunes])) + "…"
	}
	return line
}

func (g *Group) runNewDetach(cmd *cobra.Command, opts newSessionOpts, asJSON bool) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	ctx := context.Background()

	// Resolve --repo to a repo_id.
	repos, err := c.ListRepos(ctx)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("list repos: %w", err))
	}
	repoID, err := resolveRepoArg(opts.RepoID, repos)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	opts.RepoID = repoID

	// The server requires a non-empty title. Mirror the interactive TUI and
	// derive one client-side from the prompt when --title is absent. Only the
	// TITLE is derived — opts.Prompt itself is never rewritten, so Plan reaches
	// the daemon byte-identical.
	if strings.TrimSpace(opts.Title) == "" {
		opts.Title = deriveSessionTitle(opts.Prompt)
	}

	req := newDetachRequest(opts)

	stream, err := c.CreateSession(ctx, req)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("create session: %w", err))
	}
	defer func() { _ = stream.Close() }()

	// Drain the stream. Setup output goes to stderr on both paths, so stdout
	// under --json carries exactly one JSON object. On the human path, print the
	// stable id lines as soon as their values are available; setup can keep
	// running for minutes after the session row exists, and callers need the ids
	// while it is still in progress. The accepted frame may not yet carry a chat
	// id, so do not freeze the chat-id line until it is populated or the stream
	// settles.
	var session *pb.Session
	printedSessionID := false
	printedChatID := false
	for stream.Receive() {
		msg := stream.Msg()
		if so := msg.GetSetupOutput(); so != nil {
			cliutil.PrintSetupOutput(cmd.ErrOrStderr(), so.Text)
		}
		if sc := msg.GetSessionCreated(); sc != nil {
			session = sc.Session
			if !asJSON {
				if !printedSessionID {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "session-id: %s\n", session.GetId())
					printedSessionID = true
				}
				if !printedChatID && session.GetAgentSessionId() != "" {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "chat-id:    %s\n", session.GetAgentSessionId())
					printedChatID = true
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("create session: %w", err))
	}
	if session == nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("daemon did not return a session"))
	}

	// A session that came back without a chat id started no agent — the
	// --quick-chat shape, which the daemon creates idle and only runs once
	// somebody attaches. Left unsaid that is a silent no-op, so name it. It goes
	// to STDERR on BOTH paths: the two lines below are a frozen scripting
	// surface, and under --json stdout must stay exactly one JSON object (setup
	// output already uses stderr for the same reason). The --json envelope
	// carries the machine-readable next_action in addition.
	if session.GetAgentSessionId() == "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), idleSessionNotice)
	}

	if asJSON {
		return cliutil.EmitJSON(cmd, newSessionJSON(session))
	}
	if !printedSessionID {
		// Byte-identical to what this path printed before --json existed: it is a
		// documented scripting surface and existing callers parse it positionally.
		// The chat-id line is printed even when empty, for the same reason.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "session-id: %s\n", session.GetId())
	}
	if !printedChatID {
		// The chat-id line is printed even when empty, for the same reason.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "chat-id:    %s\n", session.GetAgentSessionId())
	}
	return nil
}

// idleSessionNotice is the stderr warning for a create that launched no agent,
// and nextActionIdleSession its machine-readable counterpart in the --json
// envelope. Both mirror the MCP create_session surface's existing precedent
// (lib/bossalib/kit/bossmcp/tools_mutating.go), so a caller reading either surface
// is told the same thing about the same outcome.
const (
	idleSessionNotice = "notice: this session is idle awaiting attach — no agent was launched, " +
		"so the prompt has not run yet. Attach the session to start it, or re-create " +
		"without --quick-chat (adding --defer-pr) to run the prompt unattended."
	nextActionIdleSession = "session is idle awaiting attach; no agent was launched. Attach the " +
		"session to start it, or re-create without --quick-chat (adding --defer-pr) to run " +
		"the prompt unattended."
)

// configuredPluginModelAgent names the agent whose plugin default applies —
// the explicit --agent when given, else the configured default agent. Split out
// so the notice can name it without re-deriving the fallback.
func configuredPluginModelAgent(settings config.Settings, agentName string) string {
	if agentName != "" {
		return agentName
	}
	return settings.DefaultAgent
}

// configuredPluginModel returns the model this host pins for the agent plugin
// that will run the session, or "" when none is pinned. The plugin reads it as
// BOSS_PLUGIN_model; here it is the `model` entry of the matching plugin's
// config, matched on the plugin's short name (PluginConfig.Name), which is the
// same token `--agent` takes.
//
// A disabled plugin is skipped: it will not run the session, so its pin is not
// the one an explicit --model would displace.
func configuredPluginModel(settings config.Settings, agentName string) string {
	name := configuredPluginModelAgent(settings, agentName)
	if name == "" {
		return ""
	}
	for _, p := range settings.Plugins {
		if p.Name == name && p.Enabled {
			return p.Config["model"]
		}
	}
	return ""
}

// newSessionEnvelope is the `boss new --json` success envelope. It nests under
// "session" like `boss merge --json` so a driver reads one shape across the
// CLI, and it names the chat id `chat_id` — the same field the human two-line
// output labels `chat-id` — rather than leaking the proto's agent_session_id.
type newSessionEnvelope struct {
	Session newSessionBody `json:"session"`
}

type newSessionBody struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	ChatID string `json:"chat_id"`
	// Model is the session's EFFECTIVE model — the id the daemon resolved and
	// persisted for this session, which is the explicit `--model` when one was
	// given and the agent plugin's configured default otherwise. It is the same
	// value `boss show --json` reports as `model`, taken from the same
	// effective_model field, so one vocabulary covers both reads.
	//
	// Always emitted, never omitempty: a session whose model the daemon could
	// not resolve reports "", and that is a fact a caller must be able to see
	// rather than infer from an absent key. An explicit `--model` that displaces
	// a configured plugin default is additionally announced on stderr, because
	// the displacement — not the resulting id — is what a caller could not
	// otherwise learn.
	Model string `json:"model"`
	// NextAction is set only when the create launched no agent (empty chat id).
	// `omitempty` is load-bearing: it keeps the envelope byte-identical for every
	// existing caller on the ordinary path, so the key's mere presence is the
	// signal that this session needs attaching before anything runs.
	NextAction string `json:"next_action,omitempty"`
}

func newSessionJSON(session *pb.Session) newSessionEnvelope {
	body := newSessionBody{
		ID:     session.GetId(),
		Title:  session.GetTitle(),
		ChatID: session.GetAgentSessionId(),
		Model:  session.GetEffectiveModel(),
	}
	if body.ChatID == "" {
		body.NextAction = nextActionIdleSession
	}
	return newSessionEnvelope{Session: body}
}

// resolveRepoArg resolves a --repo argument (id, unique id prefix, display
// name, or local path) to a repo_id. Precedence favors EXACT matches over the
// non-exact id prefix so a value that unambiguously identifies a repo is never
// shadowed by a coincidental prefix collision: exact full id wins first (a full
// id is never treated as a prefix of itself), then an exact display name, then
// an exact local path, and only then a unique id prefix (making the id printed
// by `boss repo ls` directly usable). Ordering the prefix last matters because
// ids are hex: a pure-hex display name like "cafe" could otherwise be a prefix
// of an unrelated repo's id and silently resolve to the wrong repo — exactly the
// silent-mispick this ticket removes. A duplicated display name or an ambiguous
// id prefix returns an error listing the candidates instead of picking one.
func resolveRepoArg(arg string, repos []*pb.Repo) (string, error) {
	// Exact id wins first — a full id must never be treated as an ambiguous prefix.
	for _, r := range repos {
		if r.Id == arg {
			return r.Id, nil
		}
	}
	// Exact display name — error on duplicates (the "silently picks a broken
	// duplicate" bug). Checked before the id prefix so an exactly-named repo is
	// never shadowed by another repo whose id merely starts with the same chars.
	var nameMatches []*pb.Repo
	for _, r := range repos {
		if r.DisplayName == arg {
			nameMatches = append(nameMatches, r)
		}
	}
	if len(nameMatches) == 1 {
		return nameMatches[0].Id, nil
	}
	if len(nameMatches) > 1 {
		return "", fmt.Errorf("repo name %q is ambiguous; matches: %s; pass the full id", arg, repoCandidates(nameMatches))
	}
	// Exact local path (also before the id prefix, for the same reason — though a
	// path starts with '/' and can never collide with a hex id prefix).
	for _, r := range repos {
		if r.LocalPath == arg {
			return r.Id, nil
		}
	}
	// Unique id prefix (this is what makes the printed short id usable).
	var idMatches []*pb.Repo
	for _, r := range repos {
		if strings.HasPrefix(r.Id, arg) {
			idMatches = append(idMatches, r)
		}
	}
	if len(idMatches) == 1 {
		return idMatches[0].Id, nil
	}
	if len(idMatches) > 1 {
		return "", fmt.Errorf("repo %q is an ambiguous id prefix; matches: %s; pass the full id", arg, repoCandidates(idMatches))
	}
	return "", fmt.Errorf("repo %q not found; register it first with 'boss repo add'", arg)
}

// repoCandidates renders a repo disambiguation list, one candidate per entry as
// "<full-id> (<name>, <path>)". IDs are never truncated — surfacing a usable
// identifier is the whole point.
func repoCandidates(repos []*pb.Repo) string {
	parts := make([]string, len(repos))
	for i, r := range repos {
		parts[i] = fmt.Sprintf("%s (%s, %s)", r.Id, r.DisplayName, r.LocalPath)
	}
	return strings.Join(parts, "; ")
}

func (g *Group) runAttach(cmd *cobra.Command, sessionID string) error {
	return g.LaunchTUI(cmd, LaunchOptions{AttachSessionID: sessionID})
}

// mergeTargetDescription names what a merge is about to act on, so the
// confirmation prompt catches a mistyped prefix that resolved to a real (and
// possibly archived) session. A session with a linked PR merges that PR; one
// without takes the daemon's local-only-branch merge path.
func mergeTargetDescription(sess *pb.Session) string {
	if sess.PrNumber != nil {
		return fmt.Sprintf("PR #%d %q", sess.GetPrNumber(), sess.GetTitle())
	}
	base := sess.GetBaseBranch()
	if base == "" {
		base = "its base branch"
	}
	return fmt.Sprintf("local branch %s into %s", sess.GetBranchName(), base)
}

// mergeJSON is the `boss merge --json` success envelope.
type mergeJSON struct {
	Session mergeSessionJSON `json:"session"`
	PR      *mergePRJSON     `json:"pr,omitempty"`
	// Detail is the daemon's note about how the merge was performed (e.g. a
	// merge-strategy substitution), empty when it ran exactly as configured.
	// Always emitted, never omitted: over --remote it is always "" because
	// ProxyMergeSessionResponse carries no detail field, so a caller has to be
	// able to tell "the daemon said nothing" from "this transport cannot say".
	// (--host is not affected: it tunnels to a real local client and carries the
	// daemon's detail through unchanged.)
	Detail string `json:"detail"`
}

type mergeSessionJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// State is the protobuf enum name (e.g. SESSION_STATE_MERGED) — the same
	// vocabulary the wire uses, so it needs no client-side mapping table and
	// does not drift when a display label changes.
	//
	// It is the SETTLED state: the CLI re-reads the session after the merge
	// call returns and reports that. The merge response's own session cannot
	// be used, because the daemon's handler reads it before its own deferred
	// display refresh applies the Merged transition (see
	// services/bossd/internal/server/server.go, MergeSession) — so a genuine
	// merge could answer with the PRE-merge state, a field unable to mean what
	// its name implies.
	State string `json:"state"`
	// StateSettled reports whether State came from that post-merge re-read.
	// False means the re-read failed and State fell back to the merge
	// response's own value, which may lag — without this flag that fallback
	// would silently reintroduce the defect the re-read exists to remove. The
	// merge itself succeeded either way; a failed re-read is not a failed
	// merge, so it is reported rather than raised.
	StateSettled bool `json:"state_settled"`
}

type mergePRJSON struct {
	Number int32  `json:"number"`
	URL    string `json:"url"`
}

// newMergeJSON builds the success envelope. sess is the merge response's own
// session; settled is the post-merge re-read, or nil when that read failed.
// Identity (id, title) and the PR come from sess — the merge response is
// authoritative about the merge it just performed — while state comes from the
// re-read, which is the only value that can mean what the field's name implies.
//
// The pr object is omitted entirely unless the session carries a number or a
// URL: emitting it full of zero values would make a local-only-branch merge
// look like it merged PR #0.
//
// There is deliberately no already_merged field. The daemon's short-circuit
// returns an ordinary success plus a detail string, and MergeSessionResponse
// carries nothing else a client could key on — deriving the flag from detail
// text would reintroduce exactly the message matching this envelope removes.
// session.state is not a substitute for it either: a session that was already
// merged settles to MERGED just as a freshly merged one does.
func newMergeJSON(sess, settled *pb.Session, detail string) mergeJSON {
	state, stateSettled := sess.GetState().String(), false
	if settled != nil {
		state, stateSettled = settled.GetState().String(), true
	}
	env := mergeJSON{
		Session: mergeSessionJSON{
			ID:           sess.GetId(),
			Title:        sess.GetTitle(),
			State:        state,
			StateSettled: stateSettled,
		},
		Detail: detail,
	}
	if sess.PrNumber != nil || sess.PrUrl != nil {
		env.PR = &mergePRJSON{Number: sess.GetPrNumber(), URL: sess.GetPrUrl()}
	}
	return env
}

// runArchive requests an archive. The daemon defers it while a chat in the
// session is still working (BOS-1380), so both outcomes — archived now, or
// pending until every chat is idle — are successes and exit 0. Run from inside
// a session, the ambient $BOSS_AGENT_SESSION_ID is sent as the requester so the
// daemon treats the calling chat as busy until it settles.
func (g *Group) runArchive(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool("json")
	force, _ := cmd.Flags().GetBool("force")

	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}
	resp, err := c.ArchiveSession(ctx, &pb.ArchiveSessionRequest{
		Id:                      sessionID,
		ShouldForce:             force,
		RequesterAgentSessionId: strings.TrimSpace(g.Getenv("BOSS_AGENT_SESSION_ID")),
	})
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("archive session: %w", err))
	}
	sess := resp.GetSession()
	if sess == nil {
		sess = &pb.Session{Id: sessionID}
	}
	if asJSON {
		return cliutil.EmitJSON(cmd, newArchiveJSON(sess, resp))
	}
	line := fmt.Sprintf("Session %s archived (%s).\n", sess.GetId(), sess.GetTitle())
	if resp.GetIsDeferred() {
		line = archivePendingLine(sess.GetId(), resp)
	}
	if _, err := io.WriteString(cmd.OutOrStdout(), line); err != nil {
		return fmt.Errorf("write archive output: %w", err)
	}
	return nil
}

// archivePendingLine says which chat a deferred archive is waiting on and, when
// that chat has stopped making progress, for how long. The daemon counts from
// when it flagged the chat, which is already some minutes into the silence, so
// the figure is a lower bound.
func archivePendingLine(sessionID string, resp *pb.ArchiveSessionResponse) string {
	chat := resp.GetBlockingAgentSessionId()
	stalled := resp.GetBlockingChatStalledSeconds()
	if stalled <= 0 {
		return fmt.Sprintf("Session %s archive pending (chat %s working).\n", sessionID, chat)
	}
	minutes := max(stalled/60, 1)
	unit := "minutes"
	if minutes == 1 {
		unit = "minute"
	}
	return fmt.Sprintf("Session %s archive pending: chat %s has had no progress for over %d %s.\n", sessionID, chat, minutes, unit)
}

// archiveJSON is the `boss archive --json` success envelope.
type archiveJSON struct {
	Session archiveSessionJSON `json:"session"`
	// Outcome is "archived" when the archive ran now, or "pending" when the
	// daemon is holding it until every chat in the session is idle.
	Outcome string `json:"outcome"`
	// BlockingChatID is the agent session id of the chat a pending archive is
	// waiting on. Always emitted — empty when archived — so a caller never has
	// to probe for the key.
	BlockingChatID string `json:"blocking_chat_id"`
}

type archiveSessionJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

func newArchiveJSON(sess *pb.Session, resp *pb.ArchiveSessionResponse) archiveJSON {
	env := archiveJSON{
		Session: archiveSessionJSON{ID: sess.GetId(), Title: sess.GetTitle()},
		Outcome: archiveOutcomeArchived,
	}
	if resp.GetIsDeferred() {
		env.Outcome = archiveOutcomePending
		env.BlockingChatID = resp.GetBlockingAgentSessionId()
	}
	return env
}

func (g *Group) runRename(cmd *cobra.Command, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("rename: new title must not be empty")
	}
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	sess, err := c.UpdateSession(ctx, &pb.UpdateSessionRequest{Id: sessionID, Title: &title})
	if err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	fmt.Printf("Session %s renamed to %q.\n", sess.Id, sess.Title)
	return nil
}

func (g *Group) runSessionLinkPR(cmd *cobra.Command, sessionID, prRef string) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return err
	}
	return linkSessionPR(cmd, c, sessionID, prRef)
}

type sessionPRLinker interface {
	LinkSessionPR(ctx context.Context, id, pr string) (*pb.LinkSessionPRResponse, error)
}

func linkSessionPR(cmd *cobra.Command, c sessionPRLinker, sessionID, prRef string) error {
	resp, err := c.LinkSessionPR(context.Background(), sessionID, prRef)
	if rpcerr.KindOf(err) == rpcerr.AlreadyExists {
		return fmt.Errorf("link PR: %s", cliutil.EnvelopeMessage(err))
	}
	if err != nil {
		return fmt.Errorf("link PR: %w", err)
	}
	sess := resp.GetSession()
	out := cmd.OutOrStdout()
	switch {
	case sess.GetPrUrl() != "":
		_, _ = fmt.Fprintf(out, "Session %s linked to PR %s.\n", sess.GetId(), sess.GetPrUrl())
	case sess.PrNumber != nil:
		_, _ = fmt.Fprintf(out, "Session %s linked to PR #%d.\n", sess.GetId(), sess.GetPrNumber())
	default:
		_, _ = fmt.Fprintf(out, "Session %s linked to PR.\n", sess.GetId())
	}
	if note := finishedSessionNote(sess, false); note != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	if wt := resp.GetWorktreeBranch(); wt != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Session worktree is on %s; check out %s to push to this PR.\n", wt, sess.GetBranchName())
	}
	return nil
}

// finishedSessionNote explains why a PR command left a closed or merged
// session as it was, and how to bring it back; it is empty for a live session.
// Only linking an open PR reopens one. refreshed marks refresh-pr, which never
// changes the session state.
func finishedSessionNote(sess *pb.Session, refreshed bool) string {
	var state string
	switch sess.GetState() {
	case pb.SessionState_SESSION_STATE_CLOSED:
		state = "closed"
	case pb.SessionState_SESSION_STATE_MERGED:
		state = "merged"
	default:
		return ""
	}
	id, pr := sess.GetId(), sess.GetPrNumber()
	if !refreshed {
		return fmt.Sprintf("Session %s stays %s: only linking an open PR reopens it.", id, state)
	}
	switch sess.GetDisplayStatus() {
	case pb.DisplayStatus_DISPLAY_STATUS_CLOSED, pb.DisplayStatus_DISPLAY_STATUS_MERGED:
		return fmt.Sprintf("Session %s stays %s: refresh-pr re-reads the PR but never reopens a session. If the work moved to an open PR, run `boss session link-pr %s <pr-url>` to reopen it.", id, state, id)
	default:
		return fmt.Sprintf("Session %s stays %s although PR #%d is open again: refresh-pr never reopens a session. Run `boss session link-pr %s %d` to reopen it.", id, state, pr, id, pr)
	}
}

type sessionPRRefresher interface {
	RefreshSessionPR(ctx context.Context, req *pb.RefreshSessionPRRequest) (*pb.Session, error)
}

func (g *Group) runSessionRefreshPR(cmd *cobra.Command, sessionID string, prNumber int32) error {
	c, err := g.newClient(cmd)
	if err != nil {
		return err
	}
	return refreshSessionPR(cmd, c, sessionID, prNumber)
}

func refreshSessionPR(cmd *cobra.Command, c sessionPRRefresher, sessionID string, prNumber int32) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" && prNumber == 0 {
		return fmt.Errorf("refresh PR: session id or --pr is required")
	}
	if prNumber < 0 {
		return fmt.Errorf("refresh PR: --pr must be positive")
	}

	req := &pb.RefreshSessionPRRequest{}
	if sessionID != "" {
		req.Id = &sessionID
	}
	if prNumber > 0 {
		req.PrNumber = &prNumber
	}
	sess, err := c.RefreshSessionPR(context.Background(), req)
	if err != nil {
		return fmt.Errorf("refresh PR: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Session %s refreshed PR #%d (%s).\n", sess.GetId(), sess.GetPrNumber(), displayStatusName(sess.GetDisplayStatus()))
	if note := finishedSessionNote(sess, true); note != "" {
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), note)
	}
	return nil
}

// accountShowLabel renders the bound-account line for `boss show`. It prefers
// the server-computed account_label (one source of truth, matching the web
// SessionDetail badge); when the daemon has not populated a label it falls back
// to the raw account id, and to the unmanaged label for an unbound session (empty
// account id = the daemon's default-account policy applied).
func accountShowLabel(accountID, accountLabel string) string {
	if accountLabel != "" {
		return accountLabel
	}
	if accountID == "" {
		return display.UnmanagedLocalCredentialsLabel
	}
	return accountID
}

func recordedSessionSettingLabel(value string) string {
	if value == "" {
		return "not recorded"
	}
	return value
}

// printSessionShowHeader writes the key-value header block for `boss show` to
// stdout. Kept separate from runShow so its formatting (including the Account
// line) is unit-testable without a live daemon client.
func printSessionShowHeader(sess *pb.Session) {
	fmt.Printf("  ID:       %s\n", sess.Id)
	fmt.Printf("  Title:    %s\n", sess.Title)
	fmt.Printf("  Repo:     %s\n", sess.RepoDisplayName)
	fmt.Printf("  Branch:   %s\n", sess.BranchName)
	fmt.Printf("  State:    %s\n", display.StateLabel(sess.State))
	fmt.Printf("  Account:  %s\n", accountShowLabel(sess.GetAccountId(), sess.GetAccountLabel()))
	fmt.Printf("  Agent:    %s\n", recordedSessionSettingLabel(sess.GetAgentName()))
	fmt.Printf("  Model:    %s\n", recordedSessionSettingLabel(sess.GetEffectiveModel()))
	fmt.Printf("  Effort:   %s\n", recordedSessionSettingLabel(sess.GetEffectiveEffort()))
	if sess.PrNumber != nil {
		fmt.Printf("  PR:       #%d\n", *sess.PrNumber)
	}
	if sess.GetWorktreePath() != "" {
		fmt.Printf("  Worktree: %s\n", sess.GetWorktreePath())
	}
	// Printed in full and deliberately untruncated. Every other operator-facing
	// surface loses this text: the TUI hint applies firstLine + a 48-rune cap
	// (views/status.go), `boss ls` has no room for it at all, and a daemon
	// started detached writes its logs to /dev/null — which is exactly the
	// failure that most needs the raw reason read. This line is the CLI's answer
	// to "what actually went wrong", so it must never acquire a truncation.
	if reason := sess.GetBlockedReason(); reason != "" {
		fmt.Printf("  Blocked:  %s\n", strings.ReplaceAll(reason, "\n", "\n            "))
	}
	if sess.CreatedAt != nil {
		fmt.Printf("  Created:  %s\n", display.RelativeTime(sess.CreatedAt.AsTime()))
	}
	if sess.ArchivedAt != nil {
		fmt.Printf("  Archived: %s\n", display.RelativeTime(sess.ArchivedAt.AsTime()))
	}
}

func (g *Group) runShow(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)

	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	sess, err := c.GetSession(ctx, sessionID, rpc.SessionReadOptions{})
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("get session: %w", err))
	}

	if asJSON {
		// Session detail only — the chats table below is `boss chats`' shape,
		// which has to join in per-chat status, so it stays out of this
		// envelope rather than shipping a half-populated version of it.
		return cliutil.EmitJSON(cmd, sessionShowJSON{Session: newSessionDetailJSON(sess)})
	}

	// Print key-value header.
	printSessionShowHeader(sess)

	// Last repair-attempt diagnostics, when present. last_repair_started_at
	// is the "an attempt was recorded" sentinel — it stays nil for sessions
	// that the repair plugin has never run, so we suppress the line entirely
	// in that case instead of showing "Last repair: -" noise. The count is
	// a *consecutive-failure* tally (clean runs reset it to zero), which is
	// why the "(N×)" annotation only appears on the failure branches.
	switch {
	case sess.GetLastRepairRunnerError() != "":
		fmt.Printf("  Last repair: ✗ runner failed (%d×): %s\n",
			sess.GetLastRepairAttemptCount(), sess.GetLastRepairRunnerError())
	case sess.GetLastRepairExitError() != "":
		fmt.Printf("  Last repair: ✗ agent exited error (%d×): %s\n",
			sess.GetLastRepairAttemptCount(), sess.GetLastRepairExitError())
	case sess.GetLastRepairStartedAt() != nil:
		fmt.Printf("  Last repair: ✓ ok %s\n",
			display.RelativeTime(sess.GetLastRepairStartedAt().AsTime()))
	}

	// Non-fatal setup-script failure flag: the session was created in a
	// degraded state because its repo setup script failed during worktree
	// creation. Empty for clean runs.
	if sess.GetSetupError() != "" {
		fmt.Printf("  Setup:    ⚠ setup script failed: %s\n", sess.GetSetupError())
	}

	// List chats as a table.
	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("list chats: %w", err)
	}
	if len(chats) == 0 {
		fmt.Println("\n  No chats.")
		return nil
	}

	statuses, statusErr := fetchChatStatuses(ctx, c, sessionID)
	if statusErr != nil {
		warnChatStatusUnavailable(cmd, statusErr)
	}

	fmt.Println()
	printChatsTable(cmd, chats, statuses, statusErr == nil)
	return nil
}

func (g *Group) runChats(cmd *cobra.Command, sessionID string) error {
	asJSON, _ := cmd.Flags().GetBool(cliutil.JSONFlagName)

	c, err := g.newClient(cmd)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	ctx := context.Background()
	sessionID, err = cliutil.ResolveSessionID(c, ctx, sessionID)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, err)
	}

	chats, err := c.ListChats(ctx, sessionID)
	if err != nil {
		return cliutil.EmitJSONFailure(cmd, asJSON, fmt.Errorf("list chats: %w", err))
	}

	// The status read is what makes this command answerable at all: a chat row
	// without a status says nothing about whether the chat has settled. It is
	// fetched before the empty-list shortcut so the two output modes agree on
	// when status is unavailable regardless of how many chats came back.
	statuses, statusErr := fetchChatStatuses(ctx, c, sessionID)
	if statusErr != nil {
		if asJSON {
			// Fail loudly rather than emit rows with an absent status. Degrading
			// here produces rows that read `"status": "UNSPECIFIED"` — byte
			// identical to chats that genuinely have no cached status — so a
			// driver cannot tell "the read failed" from "this chat has not
			// reported yet", and one forgotten null check merges on a WORKING
			// chat. Refusing to answer is what makes "a missing status degrades
			// to a stall" true rather than aspirational.
			return cliutil.EmitJSONFailure(cmd, asJSON, cliutil.CodedError(cliutil.CodeChatStatusUnavailable,
				fmt.Errorf("get chat statuses: %w", statusErr)))
		}
		warnChatStatusUnavailable(cmd, statusErr)
	}

	if asJSON {
		return cliutil.EmitJSON(cmd, newChatsJSON(chats, statuses))
	}

	if len(chats) == 0 {
		fmt.Println("No chats found.")
		return nil
	}

	printChatsTable(cmd, chats, statuses, statusErr == nil)
	return nil
}

// chatStatusReader is the one client method the chat-status join needs. Naming
// it keeps fetchChatStatuses testable with a stub without dragging in the whole
// client surface.
type chatStatusReader interface {
	GetChatStatuses(ctx context.Context, sessionID string) ([]*pb.ChatStatusEntry, error)
}

// fetchChatStatuses reads the session's per-chat statuses and keys them by
// agent_session_id for the left join onto ListChats.
//
// Every error is reported to the caller, including rpcerr.Unimplemented.
// chatWaitTick (chat.go) tolerates Unimplemented because it has a transcript
// read to fall back on; this command has none. --remote is no longer a source of
// Unimplemented here (RemoteClient.GetChatStatuses proxies the read through
// ProxyGetChatStatuses since BOS-824), but a daemon older than that RPC still
// answers Unimplemented, and swallowing it would turn "this transport cannot
// tell you" into a silent "nothing is working".
//
// get_session_statuses is deliberately NOT a fallback here: it answers a
// different question (session aggregate vs the tracked chat), and a
// plausible-looking wrong answer on this path causes a wrong merge.
func fetchChatStatuses(ctx context.Context, c chatStatusReader, sessionID string) (map[string]*pb.ChatStatusEntry, error) {
	entries, err := c.GetChatStatuses(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	byChat := make(map[string]*pb.ChatStatusEntry, len(entries))
	for _, e := range entries {
		byChat[e.GetAgentSessionId()] = e
	}
	return byChat, nil
}

// warnChatStatusUnavailable prints the single stderr line the human table mode
// owes a reader whose STATUS cells all read "?".
func warnChatStatusUnavailable(cmd *cobra.Command, err error) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"boss: chat status unavailable (%v); STATUS and LAST OUTPUT show ? — no chat below can be read as settled\n", err)
}

// chatsJSON is the `boss chats --json` success envelope.
type chatsJSON struct {
	Chats []chatJSON `json:"chats"`
}

// chatJSON is one chat row. Every field is always emitted — never omitempty:
// an absent field is precisely what a settled-green driver must not have to
// guess about.
type chatJSON struct {
	AgentSessionID string `json:"agent_session_id"`
	Title          string `json:"title"`
	// Role is the purpose the chat was created for ("build", "repair",
	// "verify", "operator"); "" for a legacy chat. Key on it, not on Title,
	// which agents and operators rename.
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
	// ReclaimedAt and ReclaimReason say automation replaced this chat on purpose
	// (an idle agent-owned chat repair reclaimed); empty when it never was.
	ReclaimedAt   string `json:"reclaimed_at"`
	ReclaimReason string `json:"reclaim_reason"`
	// Status is the trimmed enum string ("IDLE"), not the numeric value: a
	// driver comparing against "IDLE" is the intended use, and a number would
	// invite off-by-one coupling to the enum.
	Status string `json:"status"`
	// LastOutputAt is the proto's last_output_at verbatim, and is named after
	// it. For a WORKING chat it is effectively fetch-time — every WORKING chat
	// in one fetch shares it to the nanosecond — so it says nothing about
	// staleness until the chat is IDLE. "IDLE and stale" is a sound settled
	// gate; "stale" alone is not.
	LastOutputAt string `json:"last_output_at"`
	// WaitingReason names the callback a chat has armed: the block of a WAITING
	// chat, or the callback a WORKING chat will park on once it goes idle.
	// Empty for every other status.
	WaitingReason string `json:"waiting_reason"`
	Phase         string `json:"phase"`
	// The three liveness discriminators the daemon already computes on
	// ChatStatusEntry. Without them LastOutputAt above cannot be read: a
	// spinner redraw advances it, so a pane whose only sign of life is the
	// spinner is indistinguishable from one producing real output. The MCP
	// chat surface already exposes them and its tool description tells callers
	// to gate on them; a CLI-transport driver had no way to follow the same
	// rule.
	//
	// SpinnerPresent is true while the pane shows a live agent spinner.
	SpinnerPresent bool `json:"spinner_present"`
	// LastSubstantiveOutputAt is the last pane change that was NOT merely a
	// spinner redraw — the signal to gate on. Empty when nothing is known.
	LastSubstantiveOutputAt string `json:"last_substantive_output_at"`
	// LastOutputSeeded is true while either timestamp is still the seed stamped
	// when the daemon first saw the chat rather than an observation of real
	// output. Every chat registered in one poll tick shares that instant, so
	// without this flag a nanosecond-identical value across rows cannot be told
	// from a real collision of simultaneous observations.
	LastOutputSeeded bool `json:"last_output_seeded"`
}

// newChatsJSON left-joins the statuses onto the chats by agent_session_id.
func newChatsJSON(chats []*pb.AgentChat, statuses map[string]*pb.ChatStatusEntry) chatsJSON {
	out := chatsJSON{Chats: make([]chatJSON, 0, len(chats))}
	for _, chat := range chats {
		st := statuses[chat.GetAgentSessionId()]
		out.Chats = append(out.Chats, chatJSON{
			AgentSessionID: chat.GetAgentSessionId(),
			Title:          chat.GetTitle(),
			Role:           display.ChatRoleName(chat.GetRole()),
			CreatedAt:      cliutil.Rfc3339OrEmpty(chat.GetCreatedAt()),
			ReclaimedAt:    cliutil.Rfc3339OrEmpty(chat.GetReclaimedAt()),
			ReclaimReason:  chat.GetReclaimReason(),
			Status:         display.ChatStatusName(st.GetStatus()),
			LastOutputAt:   cliutil.Rfc3339OrEmpty(st.GetLastOutputAt()),
			WaitingReason:  st.GetWaitingReason(),
			Phase:          st.GetPhase(),
			// GetX on a nil entry yields the zero value, which is the honest
			// reading for a chat the status read did not cover.
			SpinnerPresent:          st.GetSpinnerPresent(),
			LastSubstantiveOutputAt: cliutil.Rfc3339OrEmpty(st.GetLastSubstantiveOutputAt()),
			LastOutputSeeded:        st.GetLastOutputSeeded(),
		})
	}
	return out
}

// chatLivenessCell renders the three liveness discriminators into one cell.
// LAST OUTPUT alone cannot be read: a spinner redraw advances it, so a pane
// whose only sign of life is the spinner looks identical to one producing real
// output. The cell names the spinner, names a still-seeded (never-observed)
// timestamp, and ends with the age of the last SUBSTANTIVE output — the signal
// a reader should actually gate on.
//
//	"spinner seeded -"  a spinning pane that has never produced real output
//	"spinner 12m"       spinning, but nothing substantive for 12 minutes
//	"3h"                not spinning; last real output 3 hours ago
//
// The rendered prose is human-only. `boss chats --json` carries the three
// fields separately and is the stable surface for a driver.
func chatLivenessCell(st *pb.ChatStatusEntry) string {
	parts := make([]string, 0, 3)
	if st.GetSpinnerPresent() {
		parts = append(parts, "spinner")
	}
	if st.GetLastOutputSeeded() {
		parts = append(parts, "seeded")
	}
	if t := st.GetLastSubstantiveOutputAt(); t != nil {
		parts = append(parts, display.RelativeTime(t.AsTime()))
	} else {
		parts = append(parts, "-")
	}
	return strings.Join(parts, " ")
}

// printChatsTable renders the human table. statusAvailable false means the
// status read failed: STATUS and LAST OUTPUT render "?" so no cell can be
// mistaken for a settled chat.
func printChatsTable(cmd *cobra.Command, chats []*pb.AgentChat, statuses map[string]*pb.ChatStatusEntry, statusAvailable bool) {
	ids := make([]string, len(chats))
	titles := make([]string, len(chats))
	createds := make([]string, len(chats))
	statusCells := make([]string, len(chats))
	lastOutputs := make([]string, len(chats))
	livenessCells := make([]string, len(chats))
	for i, chat := range chats {
		ids[i] = chat.AgentSessionId
		t := chat.Title
		if t == "" {
			t = "New chat"
		}
		t = cliutil.TruncateString(t, 50)
		titles[i] = t
		if chat.CreatedAt != nil {
			createds[i] = display.RelativeTime(chat.CreatedAt.AsTime())
		} else {
			createds[i] = "-"
		}

		if !statusAvailable {
			statusCells[i], lastOutputs[i], livenessCells[i] = "?", "?", "?"
			continue
		}
		st := statuses[chat.GetAgentSessionId()]
		cell := display.ChatStatusName(st.GetStatus())
		if reason := st.GetWaitingReason(); reason != "" {
			// waiting_reason exists precisely to explain the wait, so the human
			// table shows it inline rather than making a reader run a second
			// command to find out what the chat is blocked on.
			cell = cliutil.TruncateString(cell+" ("+reason+")", chatStatusCellMax)
		}
		statusCells[i] = cell
		if lo := st.GetLastOutputAt(); lo != nil {
			lastOutputs[i] = display.RelativeTime(lo.AsTime())
		} else {
			lastOutputs[i] = "-"
		}
		livenessCells[i] = chatLivenessCell(st)
	}

	// The new columns are appended rather than inserted: ID / TITLE / CREATED
	// keep the positions they have always had, so a reader (or a script) that
	// already slices this table by field index is widened, not silently shifted
	// onto a different column.
	cols := []table.Column{
		{Title: "ID", Width: display.MaxColWidth("ID", ids, 0)},
		{Title: "TITLE", Width: display.MaxColWidth("TITLE", titles, 50)},
		{Title: "CREATED", Width: display.MaxColWidth("CREATED", createds, 12)},
		{Title: "STATUS", Width: display.MaxColWidth("STATUS", statusCells, chatStatusCellMax)},
		{Title: "LAST OUTPUT", Width: display.MaxColWidth("LAST OUTPUT", lastOutputs, 12)},
		{Title: "LIVENESS", Width: display.MaxColWidth("LIVENESS", livenessCells, 16)},
	}

	rows := make([]table.Row, len(chats))
	for i := range chats {
		rows[i] = table.Row{ids[i], titles[i], createds[i], statusCells[i], lastOutputs[i], livenessCells[i]}
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
}

func commandFlag(cmd *cobra.Command, name string) string {
	if cmd == nil || cmd.Root() == nil {
		return ""
	}
	value, _ := cmd.Root().Flags().GetString(name)
	return value
}

const (
	archiveOutcomeArchived = "archived"
	archiveOutcomePending  = "pending"
)

// chatStatusCellMax bounds the STATUS cell. A WAITING chat appends its
// waiting_reason inline, and reasons are free text from the daemon, so the same
// bound is applied to the cell text and to the column width — otherwise one long
// reason widens the table past a terminal for every other row.
const chatStatusCellMax = 44
