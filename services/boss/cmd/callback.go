package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/table"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/boss/internal/client"
	"github.com/recurser/boss/internal/views"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/githubcallback"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/vcs"
)

// githubCallbackJSON is the stable, documented schema emitted by
// `boss callback add|list --json`. Field names are part of the machine contract
// scripts depend on: renames are breaking changes. Timestamps are RFC3339
// strings, empty when the underlying timestamp is nil/zero. The registered
// message body is a secret and is deliberately NOT part of this schema — it is
// never echoed back on any surface.
type githubCallbackJSON struct {
	ID           string `json:"id"`
	GroupID      string `json:"group_id"`
	TargetChatID string `json:"target_chat_id"`
	RepoOwner    string `json:"repo_owner"`
	RepoName     string `json:"repo_name"`
	PRNumber     int32  `json:"pr_number"`
	Trigger      string `json:"trigger"`
	State        string `json:"state"`
	AttemptCount int32  `json:"attempt_count"`
	LastEvent    string `json:"last_event"`
	LastError    string `json:"last_error"`
	TriggeredAt  string `json:"triggered_at"`
	DeliveredAt  string `json:"delivered_at"`
	ExpiresAt    string `json:"expires_at"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// githubCallbackToJSON maps a proto GithubCallback to the stable JSON schema.
// It never copies the message body (a secret) into the output.
func githubCallbackToJSON(cb *pb.GithubCallback) githubCallbackJSON {
	return githubCallbackJSON{
		ID:           cb.GetId(),
		GroupID:      cb.GetGroupId(),
		TargetChatID: cb.GetTargetChatId(),
		RepoOwner:    cb.GetRepoOwner(),
		RepoName:     cb.GetRepoName(),
		PRNumber:     cb.GetPrNumber(),
		Trigger:      cb.GetTrigger(),
		State:        cb.GetState(),
		AttemptCount: cb.GetAttemptCount(),
		LastEvent:    cb.GetLastEvent(),
		LastError:    cb.GetLastError(),
		TriggeredAt:  rfc3339OrEmpty(cb.GetTriggeredAt()),
		DeliveredAt:  rfc3339OrEmpty(cb.GetDeliveredAt()),
		ExpiresAt:    rfc3339OrEmpty(cb.GetExpiresAt()),
		CreatedAt:    rfc3339OrEmpty(cb.GetCreatedAt()),
		UpdatedAt:    rfc3339OrEmpty(cb.GetUpdatedAt()),
	}
}

// resolveCallbackChat picks the target chat for a callback: the explicit --chat
// flag wins, else the ambient BOSS_AGENT_SESSION_ID (the chat this agent is
// running as). Returns an actionable error when neither is available so the
// caller knows exactly how to supply it.
func resolveCallbackChat(cmd *cobra.Command) (string, error) {
	if chat, _ := cmd.Flags().GetString("chat"); strings.TrimSpace(chat) != "" {
		return strings.TrimSpace(chat), nil
	}
	if chat := strings.TrimSpace(osGetenv("BOSS_AGENT_SESSION_ID")); chat != "" {
		return chat, nil
	}
	return "", fmt.Errorf("no target chat: pass --chat <agent-session-id> (BOSS_AGENT_SESSION_ID is unset, so there is no ambient chat to default to)")
}

// resolveCallbackRepo determines the owner/repo for a callback registration.
// The explicit --repo owner/repo flag wins. Otherwise, when a bare PR number
// was given, it falls back to the owning repo's origin (resolved from the local
// session context) so an agent running inside a repo need not restate it. A
// full PR URL carries its own owner/repo, so repo context is optional there and
// only validated for agreement. Returns ("", "") with nil error when no context
// is available — ParsePRRef then produces the actionable "needs repository
// context" error for a bare number, or accepts a URL as-is.
func resolveCallbackRepo(cmd *cobra.Command, c client.BossClient) (owner, repo string) {
	if slug, _ := cmd.Flags().GetString("repo"); strings.TrimSpace(slug) != "" {
		// Explicit flag: split leniently; a malformed value surfaces via
		// ParsePRRef's disagreement/needs-context errors rather than here.
		o, r, err := githubcallback.SplitRepo(slug)
		if err != nil {
			return "", ""
		}
		return o, r
	}
	// Fall back to the owning repo's origin URL, resolved from the working
	// directory. This is a local-only convenience; a remote (bosso) client
	// returns no context and the caller must pass --repo or a URL.
	wd, err := os.Getwd()
	if err != nil {
		return "", ""
	}
	resolved, err := c.ResolveContext(cmd.Context(), wd)
	if err != nil || resolved.GetRepo() == nil {
		return "", ""
	}
	slug := vcs.RepoSlug(resolved.GetRepo().GetOriginUrl())
	if slug == "" {
		return "", ""
	}
	o, r, err := githubcallback.SplitRepo(slug)
	if err != nil {
		return "", ""
	}
	return o, r
}

func runCallbackAdd(cmd *cobra.Command, prRef, triggerArg string) error {
	trigger, err := githubcallback.ValidateTrigger(triggerArg)
	if err != nil {
		return err
	}

	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	chat, err := resolveCallbackChat(cmd)
	if err != nil {
		return err
	}

	ctxOwner, ctxRepo := resolveCallbackRepo(cmd, c)
	ref, err := githubcallback.ParsePRRef(prRef, ctxOwner, ctxRepo)
	if err != nil {
		return err
	}

	expiresIn, _ := cmd.Flags().GetString("expires-in")
	expiresAt, err := githubcallback.ParseExpiresIn(expiresIn, time.Now().UTC())
	if err != nil {
		return err
	}

	message, _ := cmd.Flags().GetString("message")
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("--message is required (the prompt delivered to the chat when the callback fires)")
	}

	req := &pb.CreateGithubCallbackRequest{
		TargetChatId: chat,
		RepoOwner:    ref.Owner,
		RepoName:     ref.Repo,
		PrNumber:     ref.PRNumber,
		Trigger:      string(trigger),
		Message:      message,
		ExpiresAt:    timestamppb.New(expiresAt),
	}
	if group, _ := cmd.Flags().GetString("group"); strings.TrimSpace(group) != "" {
		g := strings.TrimSpace(group)
		req.GroupId = &g
	}

	cb, err := c.CreateGithubCallback(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("create github callback: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		return emitJSON(cmd, githubCallbackToJSON(cb))
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(),
		"Registered callback %s: notify %s when %s/%s#%d %s (expires %s)\n",
		cb.GetId(), cb.GetTargetChatId(), cb.GetRepoOwner(), cb.GetRepoName(),
		cb.GetPrNumber(), triggerLabel(cb.GetTrigger()), rfc3339OrEmpty(cb.GetExpiresAt()))
	return nil
}

func runCallbackList(cmd *cobra.Command) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}

	req := &pb.ListGithubCallbacksRequest{}
	// --chat filters by target chat; unset means "all chats" for a remote fan-out
	// or every callback on the local daemon.
	if cmd.Flags().Changed("chat") {
		chat, _ := cmd.Flags().GetString("chat")
		req.TargetChatId = &chat
	}
	if cmd.Flags().Changed("repo") {
		slug, _ := cmd.Flags().GetString("repo")
		owner, repo, err := githubcallback.SplitRepo(slug)
		if err != nil {
			return err
		}
		req.RepoOwner = &owner
		req.RepoName = &repo
	}
	if cmd.Flags().Changed("state") {
		state, _ := cmd.Flags().GetString("state")
		req.State = &state
	}
	if cmd.Flags().Changed("trigger") {
		trig, _ := cmd.Flags().GetString("trigger")
		trigger, err := githubcallback.ValidateTrigger(trig)
		if err != nil {
			return err
		}
		s := string(trigger)
		req.Trigger = &s
	}

	callbacks, err := c.ListGithubCallbacks(cmd.Context(), req)
	if err != nil {
		return fmt.Errorf("list github callbacks: %w", err)
	}

	if asJSON, _ := cmd.Flags().GetBool("json"); asJSON {
		out := make([]githubCallbackJSON, len(callbacks))
		for i, cb := range callbacks {
			out[i] = githubCallbackToJSON(cb)
		}
		return emitJSON(cmd, out)
	}

	if len(callbacks) == 0 {
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), "No GitHub callbacks.")
		return nil
	}

	ids := make([]string, len(callbacks))
	prs := make([]string, len(callbacks))
	triggers := make([]string, len(callbacks))
	states := make([]string, len(callbacks))
	chats := make([]string, len(callbacks))
	expiries := make([]string, len(callbacks))
	for i, cb := range callbacks {
		ids[i] = cb.GetId()
		prs[i] = fmt.Sprintf("%s/%s#%d", cb.GetRepoOwner(), cb.GetRepoName(), cb.GetPrNumber())
		triggers[i] = cb.GetTrigger()
		states[i] = cb.GetState()
		chats[i] = cb.GetTargetChatId()
		expiries[i] = orDash(rfc3339OrEmpty(cb.GetExpiresAt()))
	}

	cols := callbackListColumns(ids, prs, triggers, states, chats, expiries)
	rows := make([]table.Row, len(callbacks))
	for i := range callbacks {
		rows[i] = table.Row{ids[i], prs[i], triggers[i], states[i], chats[i], expiries[i]}
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

// triggerColCap is the TRIGGER column cap for `boss callback list`. It is
// derived from the trigger vocabulary instead of hard-coded because the table
// silently clips a cell to its column width: a cap shorter than the longest
// valid trigger renders `checks_passed_ready` as `checks_passed_re`. Deriving
// it means growing the vocabulary can never reintroduce that truncation, while
// still bounding the column against an unrecognised (arbitrarily long) trigger
// echoed back by the daemon.
func triggerColCap() int {
	widest := len("TRIGGER")
	for _, tr := range githubcallback.ValidTriggers() {
		if n := len(string(tr)); n > widest {
			widest = n
		}
	}
	return widest
}

// callbackListColumns builds the `boss callback list` table columns. Extracted
// so the widths — and the no-truncation guarantee for the TRIGGER column — are
// testable without a daemon.
func callbackListColumns(ids, prs, triggers, states, chats, expiries []string) []table.Column {
	return []table.Column{
		{Title: "ID", Width: views.MaxColWidth("ID", ids, 0)},
		{Title: "PR", Width: views.MaxColWidth("PR", prs, 30)},
		{Title: "TRIGGER", Width: views.MaxColWidth("TRIGGER", triggers, triggerColCap())},
		{Title: "STATE", Width: views.MaxColWidth("STATE", states, 12)},
		{Title: "CHAT", Width: views.MaxColWidth("CHAT", chats, 0)},
		{Title: "EXPIRES", Width: views.MaxColWidth("EXPIRES", expiries, 20)},
	}
}

func runCallbackRemove(cmd *cobra.Command, id string) error {
	c, err := newClient(cmd)
	if err != nil {
		return err
	}
	// The remote proxy routes a delete to the owning daemon by target chat id;
	// --chat supplies it when talking to bosso. Local deletes ignore it.
	chat, _ := cmd.Flags().GetString("chat")
	if strings.TrimSpace(chat) == "" {
		chat = strings.TrimSpace(osGetenv("BOSS_AGENT_SESSION_ID"))
	}
	if err := c.DeleteGithubCallback(cmd.Context(), chat, id); err != nil {
		return fmt.Errorf("remove github callback: %w", err)
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed callback %s\n", id)
	return nil
}

// triggerLabels maps each known trigger constant to its short natural-language
// clause for human output ("is merged", "passes checks", …). Keyed on the
// models.GithubCallbackTrigger constants, never on a positional index or a
// bare string literal, so the mapping stays correct regardless of the order
// ValidTriggers() returns them in.
var triggerLabels = map[models.GithubCallbackTrigger]string{
	models.GithubCallbackTriggerMerged:            "is merged",
	models.GithubCallbackTriggerClosed:            "is closed",
	models.GithubCallbackTriggerChecksPassed:      "passes checks",
	models.GithubCallbackTriggerChecksFailed:      "fails checks",
	models.GithubCallbackTriggerReadyForReview:    "is ready for review",
	models.GithubCallbackTriggerChecksPassedReady: "passes checks while ready for review",
}

// triggerLabel renders a trigger as a short natural-language clause for human
// output ("is merged", "passes checks", …). Delivery is only a signal that the
// event fired; callers must still verify the PR's actual state. Falls back to
// the raw trigger string for an unknown key.
func triggerLabel(trigger string) string {
	if label, ok := triggerLabels[models.GithubCallbackTrigger(trigger)]; ok {
		return label
	}
	return trigger
}

func callbackCmd() *cobra.Command {
	callback := &cobra.Command{
		Use:   "callback",
		Short: "Manage GitHub PR callbacks (durable one-shot event notifications)",
		Long: "Register a durable, one-shot notification that fires a prompt into a chat " +
			"when a GitHub pull request reaches a chosen state (merged, closed, checks passed, " +
			"checks failed, ready for review, or checks passed while ready for review). " +
			"Delivery is a signal that the event fired — always verify the " +
			"PR's actual state before acting on it.",
	}

	add := &cobra.Command{
		Use:   "add <pr> <trigger>",
		Short: "Register a callback for a pull request event",
		Long: "Register a one-shot callback. <pr> is a PR number (with repository context) " +
			"or a full https://github.com/owner/repo/pull/N URL. <trigger> is one of: " +
			strings.Join(githubcallback.ValidTriggerStrings(), ", ") + ". " +
			"Triggers match on PR state, not on transitions, so arming one against a PR " +
			"that already satisfies it fires on the next evaluation.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCallbackAdd(cmd, args[0], args[1])
		},
	}
	add.Flags().String("chat", "", "Target agent-session (chat) id to notify (default: $BOSS_AGENT_SESSION_ID)")
	add.Flags().String("repo", "", "Repository as owner/repo (default: the current repository's origin)")
	add.Flags().String("message", "", "Prompt delivered to the chat when the callback fires (required)")
	add.Flags().String("expires-in", "", "Expiry as a duration (e.g. 30m, 24h, 7d, 2w); default 24h, max 30d")
	add.Flags().String("group", "", "Optional group id; siblings in a group cancel each other on first fire")
	add.Flags().Bool("json", false, "Emit the created callback as a stable JSON schema")

	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List registered GitHub callbacks",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCallbackList(cmd)
		},
	}
	list.Flags().String("chat", "", "Filter by target agent-session (chat) id")
	list.Flags().String("repo", "", "Filter by repository as owner/repo")
	list.Flags().String("trigger", "", "Filter by trigger ("+strings.Join(githubcallback.ValidTriggerStrings(), ", ")+")")
	list.Flags().String("state", "", "Filter by state (active, leased, triggered, delivered, canceled, expired)")
	list.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")

	remove := &cobra.Command{
		Use:     "remove <callback-id>",
		Aliases: []string{"rm"},
		Short:   "Remove a GitHub callback by id",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCallbackRemove(cmd, args[0])
		},
	}
	remove.Flags().String("chat", "", "Owning chat id for remote routing (default: $BOSS_AGENT_SESSION_ID; ignored locally)")

	callback.AddCommand(add, list, remove)
	return callback
}
