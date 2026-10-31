package session

import (
	"strings"

	"github.com/recurser/boss/api/cli/cliutil"

	"github.com/spf13/cobra"
)

func (g *Group) SessionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "session",
		Short: "Session diagnostics",
	}
	checks := &cobra.Command{
		Use:   "checks <session-id>",
		Short: "Show what bossd's display poller saw for this session's CI checks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			limit, _ := cmd.Flags().GetInt32("limit")
			return g.runSessionChecks(cmd, args[0], limit)
		},
	}
	checks.Flags().Int32("limit", 5, "Number of snapshots to show (newest first)")
	checks.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")
	cmd.AddCommand(checks)
	cmd.AddCommand(g.sessionPhaseCmd())
	cmd.AddCommand(g.sessionFinishCmd())
	mcp := &cobra.Command{
		Use:   "mcp <chat-id>",
		Short: "Show which MCP servers this chat's agent actually resolved, with tools and source",
		Long: "Show which MCP servers this chat's agent actually resolved, with tools and source.\n\n" +
			"This answers \"what does a fresh probe in this chat's worktree resolve?\", not \"what did the running pane resolve?\" — " +
			"it starts a new agent invocation in the chat's worktree under the chat's re-derived environment and reads what that " +
			"invocation reports, rather than inspecting the live process. The probe never runs a coding turn to completion — it " +
			"is killed the moment the startup event is read — though a probe that never sees that event may briefly start one " +
			"before its deadline fires.\n\n" +
			"The source column is repo_file, user_config, injected or unknown. injected is reserved for a launcher that supplies " +
			"MCP config on argv; boss does not, so it is never emitted today — an unattributable server reports unknown.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runSessionMCP(cmd, args[0])
		},
	}
	mcp.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")
	mcp.Flags().Bool("tools", false, "Include each server's resolved tool names")
	cmd.AddCommand(mcp)
	cmd.AddCommand(&cobra.Command{
		Use:   "link-pr <session-id> <pr-number-or-url>",
		Short: "Attach an existing pull request to a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runSessionLinkPR(cmd, args[0], args[1])
		},
	})
	cmd.AddCommand(g.sessionSetTrackerCmd())
	refreshPR := &cobra.Command{
		Use:   "refresh-pr [session-id]",
		Short: "Refresh one session's cached pull request status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, _ := cmd.Flags().GetInt32("pr")
			sessionID := ""
			if len(args) > 0 {
				sessionID = args[0]
			}
			return g.runSessionRefreshPR(cmd, sessionID, prNumber)
		},
	}
	refreshPR.Flags().Int32("pr", 0, "Pull request number to refresh")
	cmd.AddCommand(refreshPR)
	return cmd
}

func (g *Group) LSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sessions (non-interactive)",
		Long: "List sessions.\n\n" +
			"last_agent_activity_at is a floor, not liveness: any pane change advances it " +
			"(a spinner redraw keeps it fresh), and every session working at the moment of " +
			"the fetch reports it to the nanosecond, so one sample cannot tell a live session " +
			"from a frozen snapshot. To judge whether a peer is actually progressing, read " +
			"the per-chat discriminators with `boss chats --json <session-id>`: " +
			"spinner_present, last_substantive_output_at and last_output_seeded.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runLS(cmd)
		},
	}
	cmd.Flags().String("repo", "", "Filter by repo ID")
	cmd.Flags().Bool("archived", false, "Include archived sessions")
	cmd.Flags().Bool("all-daemons", false, "List live sessions from every daemon reachable through the orchestrator")
	cmd.Flags().StringSlice("state", nil, "Filter by state(s)")
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of a table")
	return cmd
}

func (g *Group) ShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <session-id>",
		Short: "Show session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runShow(cmd, args[0])
		},
	}
	cmd.Flags().Bool(cliutil.JSONFlagName, false, "Emit a stable JSON schema instead of text")
	return cmd
}

func (g *Group) ChatsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "chats <session-id>",
		Short: "List chats in a session with their status and last output time",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runChats(cmd, args[0])
		},
	}
	c.Flags().Bool("json", false, "Emit a stable JSON envelope instead of human-readable text")
	return c
}

func (g *Group) NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new coding session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runNew(cmd)
		},
	}
	cmd.Flags().String("agent", "", "Override default agent plugin for this session (e.g. claude, opencode)")
	cmd.Flags().String("repo", "", "Repository id, name, or local path (enables non-interactive mode when combined with --prompt)")
	cmd.Flags().String("prompt", "", "Initial prompt / plan for the session (enables non-interactive mode when combined with --repo)")
	cmd.Flags().String("title", "", "Session title (optional, auto-derived from prompt when absent)")
	cmd.Flags().String("model", "", "Agent model id to run this session under (e.g. an Opus id); empty = agent default")
	cmd.Flags().String("effort", "", "Agent reasoning effort to run this session under; empty = agent default")
	cmd.Flags().String("account", "", "Account id or label to run this session under (empty = system default)")
	cmd.Flags().Bool("detach", false,
		"A no-op on the non-interactive --repo + --prompt path, which always runs "+
			"unattended (BOSS_UNATTENDED=true; skills take their headless branch), prints session-id as soon as the session exists, prints chat-id later "+
			"if the daemon provides one, and streams setup progress on stderr; --tmux-unattended is the "+
			"distinct durable-pane option")
	cmd.Flags().Bool("no-attach", false, "Alias for --detach")
	cmd.Flags().Bool("tmux-unattended", false,
		"Host the session in a durable tmux pane that survives a daemon restart and is "+
			"attach-safe (independent of --detach, which only governs whether this command "+
			"attaches a chat pane before it exits). Not needed for unattended mode: every "+
			"--repo + --prompt run already gets BOSS_UNATTENDED=true")
	cmd.Flags().Bool("defer-pr", false,
		"Open no draft PR up front; a PR is opened at finalize only if the run produced "+
			"commits. For runs not expected to change the repository. Pair with "+
			"--tmux-unattended so a restart cannot strand commits before finalize. "+
			"Non-interactive --repo + --prompt path only")
	cmd.Flags().Bool("quick-chat", false,
		"Create a session with no worktree, branch, or PR, in the repository checkout. "+
			"The agent starts when you attach; unattended runs want --defer-pr. "+
			"Mutually exclusive with --defer-pr. Non-interactive --repo + --prompt "+
			"path only")
	cmd.Flags().Int32("pr", 0,
		"Create the session on an existing pull request's head branch, bound to that PR, "+
			"instead of a fresh branch. Mutually exclusive with --quick-chat and --defer-pr. "+
			"Non-interactive --repo + --prompt path only")
	cmd.Flags().Bool("verify-only", false,
		"Mark a --pr session as verify-only: automatic repair never acts on it. Requires --pr")
	// The example is a generic placeholder on purpose: this usage string is
	// harvested into the globally-installed `boss` skill core, which
	// TestPublishedCoresAreProjectAgnostic forbids from carrying a real
	// Bossanova backlog id.
	cmd.Flags().String("tracker-id", "", "External issue id to bind this session to (e.g. PROJ-42)")
	cmd.Flags().String("tracker-source", "", "External issue tracker: linear or sentry")
	cmd.Flags().String("tracker-url", "", "URL of the external issue this session is bound to")
	cmd.Flags().Bool(cliutil.JSONFlagName, false,
		"Emit the created session as a stable JSON schema instead of the two-line output")
	return cmd
}

func (g *Group) AttachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach to a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runAttach(cmd, args[0])
		},
	}
}

func (g *Group) ArchiveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "archive <session-id>",
		Short: "Archive a session (keep branch, remove worktree)",
		Long: "Archive a session: stop its chats, remove the worktree, keep the branch.\n\n" +
			"If a chat in the session is still working, the archive is deferred and runs once every chat is idle; " +
			"the command reports it as pending and exits 0. --force archives now regardless.\n" +
			"Run from inside a session, the calling chat ($BOSS_AGENT_SESSION_ID) counts as working until it settles, " +
			"so a session can safely archive itself.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runArchive(cmd, args[0])
		},
	}
	c.Flags().Bool("force", false, "Archive now even if a chat in the session is still working")
	c.Flags().Bool("json", false, "Emit a stable JSON envelope instead of human-readable text")
	return c
}

func (g *Group) MergeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "merge <session-id>",
		Short: "Merge a session's pull request (or its local-only branch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runMerge(cmd, args[0])
		},
	}
	c.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	c.Flags().Bool("json", false, "Emit a stable JSON envelope instead of human-readable text (requires --yes)")
	c.Flags().Bool("approve", false, "Record a head-pinned operator approval before merging")
	c.Flags().String("match-head", "", "Refuse the merge unless the PR head is exactly this commit SHA (40 hex)")
	return c
}

func (g *Group) RenameCmd() *cobra.Command {
	return &cobra.Command{
		// A cobra usage grammar, not a sentence: ellipsis: literal-dots ok
		Use:   "rename <session-id> <new-title...>",
		Short: "Rename a session (updates its title; syncs the linked PR title if any)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return g.runRename(cmd, args[0], strings.Join(args[1:], " "))
		},
	}
}
