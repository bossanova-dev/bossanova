// Package main is the entry point for the boss CLI.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/clidoc"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/errortrack"
	bossalog "github.com/recurser/bossalib/log"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossalib/telemetry"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "boss: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// File-only logging: all boss commands (TUI and plain CLI) write to the
	// rotated log file under $XDG_STATE_HOME/bossanova/logs/boss.log. Stderr
	// is skipped so the Bubble Tea TUI isn't corrupted by log output. Without
	// this, bug reports would always ship an empty BossLogTail.
	logCloser := setupCommandLogging()
	defer func() { _ = logCloser.Close() }()
	// A --host command leaves a supervised ssh child and a private socket
	// directory behind. Tearing them down here covers the TUI and every
	// short-lived CLI command alike, since both exit through run().
	defer shutdownHostTunnel()

	settings, _ := config.Load()
	bossEnv := config.EnvOr("BOSS_ENV", "local")
	var errortrackClose = func() {}
	if settings.ErrorTrackingEnabled {
		// boss reuses the daemon DSN per decision #1A.
		errortrackDSN := config.EnvOr("BOSS_SENTRY_DSN", "https://f8081ecc39984438b534485cb56a7391@o4511396716871680.ingest.de.sentry.io/4511396747608144")
		close, err := errortrack.Init(errortrack.Opts{
			DSN:         errortrackDSN,
			App:         "boss",
			Environment: bossEnv,
			Release:     buildinfo.Version + "-" + buildinfo.Commit,
		})
		if err != nil {
			// boss writes file-only logs (SetupFileOnly) -- log.Warn
			// here will land in the boss log file, not corrupt the TUI.
			log.Warn().Err(err).Msg("errortrack disabled")
		} else {
			errortrackClose = close
		}
	}
	defer errortrackClose()

	telemetryClient := telemetry.New(commandTelemetryConfig(settings))
	defer telemetryClient.Close()

	commandState := &executedCommandState{}
	ctx := context.WithValue(context.Background(), commandTelemetryContextKey{}, telemetryClient)
	ctx = context.WithValue(ctx, executedCommandContextKey{}, commandState)
	root := rootCmd()
	root.SetContext(ctx)
	installExecutedCommandRecorder(root)

	err := root.Execute()
	captureCommand(ctx, telemetryClient, executedCommand(ctx), err)
	// Backstop for a `--json` run that failed before its RunE was reached — a
	// rejected flag, an Args refusal, a PersistentPreRunE error. Those return
	// straight out of Execute, so without this the machine channel would be
	// empty and the caller would be back to parsing stderr. No-op when the
	// command already emitted its envelope. Telemetry is captured above, on the
	// error cobra actually produced.
	return emitRootJSONFailure(root, os.Args[1:], err)
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "boss",
		Short: "Bossanova — autonomous Claude coding sessions",
		Long:  "Boss manages Claude coding sessions with automatic PR creation, CI fix loops, and code review handling.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			// Transport selection is validated for every command, ahead of the
			// bypasses below and ahead of any dial: --host and --remote name
			// different daemons, so running either one would be a guess.
			if err := requireSingleTransport(cmd); err != nil {
				return err
			}
			// Likewise for every command: the daemon subtree acts on the local
			// machine's bossd, so --host must be refused rather than silently
			// managing the wrong daemon.
			if err := requireLocalDaemonTarget(cmd); err != nil {
				return err
			}
			// BOS-864: a bare `brew upgrade` leaves launchd respawning the old
			// staged bossd, and every prior mitigation was passive — the
			// daemon's own startup warning goes to a log file nobody reads.
			// This is the one surface the operator actually touches. It writes
			// at most one line to stderr, never to stdout, and can never fail
			// a command.
			warnIfDaemonBinaryStale(cmd)
			// gen-skill regenerates the embedded payload; the `skills` subtree is
			// itself the explicit, no-prompt installer. Both must bypass the
			// interactive startup installer/self-heal — otherwise `boss skills
			// sync` would hit the [Y/n] prompt (interactive) or be pre-empted by
			// selfHealSkills (non-TTY), reporting "up to date" instead of "updated".
			//
			// fix-terminal bypasses for a different reason: it is the escape hatch
			// for a terminal stranded in mouse-reporting mode, so it must write its
			// reset immediately. On an interactive tty the installer would print a
			// [Y/n] prompt into the very terminal being repaired and then block in
			// fmt.Scanln — hanging the remedy exactly when the user can least type.
			// See BOS-650.
			//
			// tail joins fix-terminal's family, not gen-skill's: it is a
			// diagnostic, and a [Y/n] on stderr blocking in fmt.Scanln stalls it
			// before a single log line appears. `boss tail -f | head` is the
			// reported symptom — stdout is the pipe but stdin and stderr are
			// still the terminal, so head never receives a line and the process
			// just sits there.
			//
			// Returning here skips maybeInstallSkills outright, so tail also
			// forgoes the silent non-TTY selfHealSkills path — the same wider
			// trade fix-terminal and the skills subtree already make. That is a
			// real, if minor, reduction in self-heal coverage; `boss skills
			// sync` stays the explicit remedy. See BOS-921.
			//
			// Each entry is keyed to the exact command path (the `skills` prefix
			// clause covers that subtree only). tail declares no aliases and adds
			// no subcommands today, so equality is exhaustive for it; a future
			// `boss tail <sub>` would NOT inherit this exemption and must be
			// added here deliberately. Note this is cobra's resolved
			// cmd.CommandPath(), deliberately distinct from the loose pre-cobra
			// argv scanner isTailInvocation() used by setupCommandLogging before
			// the command tree exists.
			path := cmd.CommandPath()
			if path == "boss gen-skill" || path == "boss fix-terminal" ||
				path == "boss tail" ||
				path == "boss skills" || strings.HasPrefix(path, "boss skills ") {
				return nil
			}
			return maybeInstallSkills()
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Default: launch interactive TUI home screen.
			return runTUI(cmd)
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().String("remote", "", "Connect to orchestrator URL instead of local daemon")
	// No -h shorthand: cobra owns -h for --help and redefining it panics.
	root.PersistentFlags().String("host", "", "Drive a bossd on another machine over SSH (ssh destination)")
	root.PersistentFlags().String("host-socket", "", "Remote bossd socket path, when remote boss is not on the SSH PATH")
	root.PersistentFlags().Bool("allow-insecure-keyring", false, "Fall back to the legacy static keyring passphrase when no writable passphrase location is available (insecure)")
	// Hidden — this is an escape hatch surfaced only in the keyring
	// helper's error message, not in --help. Users who need it will be
	// directed to it explicitly.
	_ = root.PersistentFlags().MarkHidden("allow-insecure-keyring")

	// Register the command groups that the /boss skill generator (and cobra's
	// own --help) use as section headings. The skill extractor hard-errors on
	// any non-hidden, non-deprecated command without a known GroupID, so every
	// such command below is assigned to one of these groups.
	for _, g := range clidoc.GroupOrder {
		root.AddGroup(&cobra.Group{ID: g.ID, Title: g.Title})
	}
	addGrouped := func(groupID string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = groupID
			root.AddCommand(c)
		}
	}

	addGrouped("session", lsCmd(), showCmd(), chatsCmd(), newCmd(), attachCmd(), mergeCmd(), archiveCmd(), renameCmd())
	addGrouped("chat", chatCmd())
	addGrouped("repo", repoCmd())
	addGrouped("cron", cronCmd())
	addGrouped("callback", callbackCmd())
	addGrouped("broadcast", broadcastCmd())
	addGrouped("notes", notesCmd())
	addGrouped("account", accountCmd())
	addGrouped("trash", trashCmd())
	addGrouped("daemon", daemonCmd())
	addGrouped("mcp", mcpCmd())
	// `boss init` joins the existing `skills` group rather than minting a new
	// one: it writes the config the boss skills read, so it belongs beside the
	// commands that install them.
	addGrouped("skills", skillsCmd(), initCmd())
	addGrouped("settings", settingsCmd(), configCmd(), loginCmd(), logoutCmd(), authStatusCmd())
	addGrouped("diagnostics", repairCmd(), sessionCmd(), envCmd(), proofCmd(), fixTerminalCmd(), tailCmd())
	// `boss agents` sits in the plugins group rather than getting one of its
	// own: agent runners ARE loaded plugins, and the command a reader reaches
	// for next to it is `boss plugin list`.
	addGrouped("plugins", pluginCmd(), agentsCmd())
	addGrouped("other", versionCmd(), upgradeCmd())

	// Deprecated (resurrect) and hidden (gen-skill) commands carry no group —
	// the skill extractor skips them, and cobra lists them under "Additional
	// Commands".
	root.AddCommand(resurrectCmd(), newGenSkillCmd())

	return root
}

// setupCommandLogging keeps boss tail from writing to a file it can read.
func setupCommandLogging() io.Closer {
	if isTailInvocation(os.Args[1:]) {
		log.Logger = zerolog.Nop()
		return io.NopCloser(strings.NewReader(""))
	}
	return bossalog.SetupFileOnly("boss")
}

// valuedRootFlags are the root persistent flags whose value is a separate argv
// element. The scan below must skip that value, or `boss --host <dest> tail`
// would read <dest> as the subcommand and attach the file logger anyway.
var valuedRootFlags = map[string]bool{
	"--remote":      true,
	"--host":        true,
	"--host-socket": true,
}

// isTailInvocation reports whether argv selects `boss tail`, scanning past the
// root flags that may precede the subcommand. The `--flag=value` form needs no
// special case: it is a single element and is skipped as an option.
func isTailInvocation(args []string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if valuedRootFlags[arg] {
			i++
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		return arg == "tail"
	}
	return false
}

func pluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Inspect installed plugins",
	}
	list := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List plugins the daemon attempted to load this run",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginList(cmd)
		},
	}
	cmd.AddCommand(list)
	return cmd
}

// agentsCmd lists the agent runners the daemon loaded. It is deliberately not a
// filtered `plugin list`: the question it answers — "is this agent runner
// available to request?" — is narrower, and it carries each runner's
// user_settings, which the plugin inventory does not.
func agentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents",
		Short: "List the agent runners the daemon loaded",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAgents(cmd)
		},
	}
	cmd.Flags().Bool(jsonFlagName, false, "Emit machine-readable JSON, including each agent's user settings")
	return cmd
}

func sessionCmd() *cobra.Command {
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
			return runSessionChecks(cmd, args[0], limit)
		},
	}
	checks.Flags().Int32("limit", 5, "Number of snapshots to show (newest first)")
	checks.Flags().Bool(jsonFlagName, false, "Emit a stable JSON schema instead of text")
	cmd.AddCommand(checks)
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
			return runSessionMCP(cmd, args[0])
		},
	}
	mcp.Flags().Bool(jsonFlagName, false, "Emit a stable JSON schema instead of text")
	mcp.Flags().Bool("tools", false, "Include each server's resolved tool names")
	cmd.AddCommand(mcp)
	cmd.AddCommand(&cobra.Command{
		Use:   "link-pr <session-id> <pr-number-or-url>",
		Short: "Attach an existing pull request to a session",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSessionLinkPR(cmd, args[0], args[1])
		},
	})
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
			return runSessionRefreshPR(cmd, sessionID, prNumber)
		},
	}
	refreshPR.Flags().Int32("pr", 0, "Pull request number to refresh")
	cmd.AddCommand(refreshPR)
	return cmd
}

func repairCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Auto-repair plugin operations",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "Health-check the auto-repair pipeline (plugin loaded, claude on PATH, recent logs, etc.)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepairDoctor(cmd)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "start",
		Short: "(Re-)arm the auto-repair workflow (e.g. after the repair plugin was stopped or restarted)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepairStart(cmd)
		},
	})
	return cmd
}

// --- Subcommands ---

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("boss " + buildinfo.String())
		},
	}
}

func lsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sessions (non-interactive)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runLS(cmd)
		},
	}
	cmd.Flags().String("repo", "", "Filter by repo ID")
	cmd.Flags().Bool("archived", false, "Include archived sessions")
	cmd.Flags().StringSlice("state", nil, "Filter by state(s)")
	cmd.Flags().Bool(jsonFlagName, false, "Emit a stable JSON schema instead of a table")
	return cmd
}

func showCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <session-id>",
		Short: "Show session details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runShow(cmd, args[0])
		},
	}
	cmd.Flags().Bool(jsonFlagName, false, "Emit a stable JSON schema instead of text")
	return cmd
}

func chatsCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "chats <session-id>",
		Short: "List chats in a session with their status and last output time",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runChats(cmd, args[0])
		},
	}
	c.Flags().Bool("json", false, "Emit a stable JSON envelope instead of human-readable text")
	return c
}

func newCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create a new coding session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNew(cmd)
		},
	}
	cmd.Flags().String("agent", "", "Override default agent plugin for this session (e.g. claude, opencode)")
	cmd.Flags().String("repo", "", "Repository id, name, or local path (enables non-interactive mode when combined with --prompt)")
	cmd.Flags().String("prompt", "", "Initial prompt / plan for the session (enables non-interactive mode when combined with --repo)")
	cmd.Flags().String("title", "", "Session title (optional, auto-derived from prompt when absent)")
	cmd.Flags().String("model", "", "Agent model id to run this session under (e.g. an Opus id); empty = agent default")
	cmd.Flags().String("account", "", "Account id or label to run this session under (empty = system default)")
	cmd.Flags().Bool("detach", false,
		"A no-op on the non-interactive --repo + --prompt path, which always runs "+
			"headlessly, prints session-id as soon as the session exists, prints chat-id later "+
			"if the daemon provides one, and streams setup progress on stderr; --tmux-unattended is the "+
			"distinct durable-pane option")
	cmd.Flags().Bool("no-attach", false, "Alias for --detach")
	cmd.Flags().Bool("tmux-unattended", false,
		"Host the session in a durable tmux pane that survives a daemon restart and is "+
			"attach-safe (independent of --detach, which only governs whether this command "+
			"attaches a chat pane before it exits)")
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
	// The example is a generic placeholder on purpose: this usage string is
	// harvested into the globally-installed `boss` skill core, which
	// TestPublishedCoresAreProjectAgnostic forbids from carrying a real
	// Bossanova backlog id.
	cmd.Flags().String("tracker-id", "", "External issue id to bind this session to (e.g. PROJ-42)")
	cmd.Flags().String("tracker-source", "", "External issue tracker: linear or sentry")
	cmd.Flags().String("tracker-url", "", "URL of the external issue this session is bound to")
	cmd.Flags().Bool(jsonFlagName, false,
		"Emit the created session as a stable JSON schema instead of the two-line output")
	return cmd
}

func attachCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <session-id>",
		Short: "Attach to a running session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAttach(cmd, args[0])
		},
	}
}

func repoCmd() *cobra.Command {
	repo := &cobra.Command{
		Use:   "repo",
		Short: "Manage repositories",
	}

	update := &cobra.Command{
		Use:   "update <repo-id>",
		Short: "Update repository settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRepoUpdate(cmd, args[0])
		},
	}
	update.Flags().String("name", "", "Set display name")
	update.Flags().String("setup-script", "", "Set setup script (empty string to clear)")
	update.Flags().String("merge-strategy", "", "Set merge strategy (merge, rebase, squash)")
	update.Flags().Bool("auto-merge", false, "Enable auto-merge")
	update.Flags().Bool("no-auto-merge", false, "Disable auto-merge")
	update.Flags().Bool("auto-merge-dependabot", false, "Enable auto-merge for Dependabot PRs")
	update.Flags().Bool("no-auto-merge-dependabot", false, "Disable auto-merge for Dependabot PRs")
	update.Flags().Bool("auto-repair", false, "Enable automatic repair (failing checks, conflicts, review feedback)")
	update.Flags().Bool("no-auto-repair", false, "Disable automatic repair")
	update.Flags().Bool("delete-branches", false, "Enable deleting safe local branches after archiving")
	update.Flags().Bool("no-delete-branches", false, "Disable deleting local branches after archiving")
	update.Flags().Bool("keep-branches-current", false, "Enable proactively rebasing in-flight session branches when the base advances")
	update.Flags().Bool("no-keep-branches-current", false, "Disable proactively rebasing in-flight session branches when the base advances")

	repo.AddCommand(
		&cobra.Command{
			Use:   "add",
			Short: "Register a repository",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runRepoAdd(cmd)
			},
		},
		&cobra.Command{
			Use:   "ls",
			Short: "List registered repositories",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runRepoLS(cmd)
			},
		},
		&cobra.Command{
			Use:   "remove <repo-id>",
			Short: "Remove a registered repository",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runRepoRemove(cmd, args[0])
			},
		},
		update,
	)

	return repo
}

func cronCmd() *cobra.Command {
	cron := &cobra.Command{
		Use:   "cron",
		Short: "Manage scheduled cron jobs",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List cron jobs",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCronLS(cmd)
		},
	}
	ls.Flags().String("repo", "", "Filter by repo ID")
	ls.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")

	show := &cobra.Command{
		Use:   "show <cron-id>",
		Short: "Show cron job details",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCronShow(cmd, args[0])
		},
	}
	show.Flags().Bool("json", false, "Emit a stable JSON schema instead of text")

	add := &cobra.Command{
		Use:   "add",
		Short: "Create a cron job",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCronAdd(cmd)
		},
	}
	add.Flags().String("repo", "", "Repository ID (required)")
	add.Flags().String("name", "", "Job name (required)")
	add.Flags().String("schedule", "", "5-field cron expression or @daily/@hourly/etc (required)")
	add.Flags().String("prompt", "", "Prompt / plan for each run")
	add.Flags().String("prompt-file", "", "Read the prompt from a file (or '-' for stdin)")
	add.Flags().String("agent", "", "Agent runner plugin name (empty = claude)")
	add.Flags().String("gate", "", "Gate command run before each fire (empty = no gate)")
	add.Flags().String("model", "", "Agent model id (empty = plugin default)")
	add.Flags().String("tz", "", "IANA timezone name (empty = daemon-local)")
	add.Flags().Bool("enabled", true, "Whether the job is enabled")
	add.Flags().Bool("run-setup", true, "Run the repo setup script before the agent")
	// Unlike --enabled/--run-setup, this defaults to false: a zero-output job
	// runs with no worktree, branch or PR, which is never a safe default.
	add.Flags().Bool("zero-output", false, "Run with no worktree, branch, or PR (for jobs that change nothing in this repo)")

	update := &cobra.Command{
		Use:   "update <cron-id>",
		Short: "Update cron job settings",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCronUpdate(cmd, args[0])
		},
	}
	update.Flags().String("name", "", "Set job name")
	update.Flags().String("schedule", "", "Set the cron schedule")
	update.Flags().String("prompt", "", "Set the prompt / plan")
	update.Flags().String("prompt-file", "", "Read a new prompt from a file (or '-' for stdin)")
	update.Flags().String("agent", "", "Set the agent runner plugin name")
	update.Flags().String("gate", "", "Set the gate command (empty string clears it)")
	update.Flags().String("model", "", "Set the agent model id (empty string clears it)")
	update.Flags().String("tz", "", "Set the IANA timezone (empty string clears it)")
	// Defaults are false (not true as on `add`) because runCronUpdate only reads
	// these via Flags().Changed() — an omitted flag preserves the current value,
	// so a "true" default would never apply and would render a misleading
	// "(default: true)" in the generated skill reference. false is elided there.
	update.Flags().Bool("enabled", false, "Enable or disable the job (unset preserves current)")
	update.Flags().Bool("run-setup", false, "Run the repo setup script before the agent (unset preserves current)")
	update.Flags().Bool("zero-output", false, "Run with no worktree, branch, or PR (unset preserves current)")

	cron.AddCommand(
		ls,
		show,
		add,
		update,
		&cobra.Command{
			Use:   "remove <cron-id>",
			Short: "Remove a cron job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runCronRemove(cmd, args[0])
			},
		},
		&cobra.Command{
			Use:   "run-now <cron-id>",
			Short: "Fire a cron job immediately",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runCronRunNow(cmd, args[0])
			},
		},
		&cobra.Command{
			Use:   "enable <cron-id>",
			Short: "Enable a cron job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runCronEnable(cmd, args[0])
			},
		},
		&cobra.Command{
			Use:   "disable <cron-id>",
			Short: "Disable a cron job",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runCronDisable(cmd, args[0])
			},
		},
	)

	return cron
}

func accountCmd() *cobra.Command {
	account := &cobra.Command{
		Use:   "account",
		Short: "Manage agent accounts (credential registry)",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List accounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccountLS(cmd)
		},
	}
	ls.Flags().String("provider", "", "Filter by provider (claude|codex)")
	ls.Flags().Bool("json", false, "Emit a stable JSON schema instead of a table")
	ls.Flags().Bool("refresh", false, "Force a live usage probe of each account before listing")

	add := &cobra.Command{
		Use:   "add [provider]",
		Short: "Register an agent account",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runAccountAddDispatch,
	}
	add.Flags().String("provider", "", "Account provider ("+trimmedProviderList()+") (or pass as a positional arg)")
	add.Flags().String("label", "", "Human label, unique per provider (required for the non-interactive flag path)")
	add.Flags().Int32("priority", 0, "Sort order; lower = preferred")
	// Credential hygiene: prefer stdin/env over --token so the secret does not
	// land in shell history.
	add.Flags().String("token", "", "Credential token (prefer --credential-file - or stdin to keep it out of shell history)")
	add.Flags().String("credential-file", "", "Read the credential from a file (or '-' for stdin); preferred over --token")
	// Interactive registration (claude setup-token walkthrough / codex device flow).
	add.Flags().Bool("token-stdin", false, "claude only: read the setup token from stdin instead of running the walkthrough")
	add.Flags().Duration("timeout", 10*time.Minute, "Deadline for an interactive registration walkthrough")

	refresh := &cobra.Command{
		Use:   "refresh <account-id>",
		Short: "Replace an account's stored credential",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccountRefresh(cmd, args[0])
		},
	}
	refresh.Flags().String("token", "", "Credential token (prefer --credential-file - or stdin to keep it out of shell history)")
	refresh.Flags().String("credential-file", "", "Read the credential from a file (or '-' for stdin); preferred over --token")
	refresh.Flags().Bool("test", false, "Validate the refreshed credential after saving")
	refresh.Flags().Bool("json", false, "Emit a stable JSON schema instead of text")

	update := &cobra.Command{
		Use:   "update <account-id>",
		Short: "Update account metadata",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccountUpdate(cmd, args[0])
		},
	}
	update.Flags().String("label", "", "Set the label")
	update.Flags().Int32("priority", 0, "Set the priority (lower = preferred)")
	update.Flags().String("status", "", "Set the status (active|disabled)")
	update.Flags().StringSlice("allowed-models", nil, "Replace the allowed-models set (comma-separated)")

	account.AddCommand(
		ls,
		add,
		refresh,
		update,
		&cobra.Command{
			Use:   "remove <account-id>",
			Short: "Remove an account and its stored credential",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				return runAccountRemove(cmd, args[0])
			},
		},
	)

	test := &cobra.Command{
		Use:   "test <account-id>",
		Short: "Validate an account's credential and record the outcome",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runAccountTest(cmd, args[0])
		},
	}
	test.Flags().Bool("json", false, "Emit a stable JSON schema instead of text")
	account.AddCommand(test)

	switchCmd := &cobra.Command{
		Use:   "switch <session> <account>",
		Short: "Stop a session's live chat, rebind it to the chosen account, and resume",
		Long: "Stop the session's live chat, rebind it to the chosen account, and respawn with resume.\n\n" +
			"<account> is an account id or label; pass \"system-default\" (or \"0\"/\"none\") to target the system default (account 0).\n" +
			"By default the session's primary live chat is switched; use --chat to target a specific agent chat.\n" +
			"A mid-turn (WORKING) chat is rejected unless --force is set.",
		Args: cobra.ExactArgs(2),
		RunE: runAccountSwitch,
	}
	switchCmd.Flags().String("chat", "", "Target a specific agent chat (agent session id); default: the session's primary live chat")
	switchCmd.Flags().Bool("force", false, "Interrupt a mid-turn / WORKING chat")
	account.AddCommand(switchCmd)

	return account
}

func archiveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "archive <session-id>",
		Short: "Archive a session (keep branch, remove worktree)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runArchive(cmd, args[0])
		},
	}
}

func mergeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "merge <session-id>",
		Short: "Merge a session's pull request (or its local-only branch)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMerge(cmd, args[0])
		},
	}
	c.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")
	c.Flags().Bool("json", false, "Emit a stable JSON envelope instead of human-readable text (requires --yes)")
	return c
}

func renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <session-id> <new-title...>",
		Short: "Rename a session (updates its title; syncs the linked PR title if any)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRename(cmd, args[0], strings.Join(args[1:], " "))
		},
	}
}

func resurrectCmd() *cobra.Command {
	return &cobra.Command{
		Use:        "resurrect <session-id>",
		Short:      "Resurrect an archived session",
		Deprecated: "use 'boss trash restore' instead",
		Args:       cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResurrect(cmd, args[0])
		},
	}
}

func daemonCmd() *cobra.Command {
	d := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the bossd daemon",
	}

	install := &cobra.Command{
		Use:   "install",
		Short: "Install bossd as a background service (launchd on macOS, systemd on Linux)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonInstall(cmd)
		},
	}
	install.Flags().Bool("force", false, "Overwrite existing service file")

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Stop the bossd daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDaemonStop(cmd)
		},
	}
	stop.Flags().Bool("all-standalone", false, "Stop all user-owned bossd processes instead of only the current profile")

	d.AddCommand(
		install,
		&cobra.Command{
			Use:   "doctor",
			Short: "Diagnose the bossd daemon: staged binary, LaunchAgent path, macOS folder permissions, and live upstream auth/registration state",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonDoctor(cmd)
			},
		},
		&cobra.Command{
			Use:   "uninstall",
			Short: "Uninstall the bossd LaunchAgent",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonUninstall(cmd)
			},
		},
		&cobra.Command{
			Use:   "status",
			Short: "Show bossd daemon status",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonStatus(cmd)
			},
		},
		&cobra.Command{
			Use:   "start",
			Short: "Start the bossd daemon",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonStart(cmd)
			},
		},
		stop,
		&cobra.Command{
			Use:   "restart",
			Short: "Restart the bossd daemon",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonRestart(cmd)
			},
		},
		&cobra.Command{
			Use:   "rotate-token",
			Short: "Rotate the daemon socket auth token (regenerated on next daemon start)",
			RunE: func(cmd *cobra.Command, args []string) error {
				return runDaemonRotateToken(cmd)
			},
		},
	)

	return d
}

func settingsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "settings",
		Short: "View or update global settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSettings(cmd)
		},
	}
	cmd.Flags().Bool("skip-permissions", false, "Enable Claude --dangerously-skip-permissions")
	cmd.Flags().Bool("no-skip-permissions", false, "Disable Claude --dangerously-skip-permissions")
	cmd.Flags().Bool("managed-accounts", false, "Enable managed accounts (bossd credential rotation)")
	cmd.Flags().Bool("no-managed-accounts", false, "Disable managed accounts (use the terminal's own login)")
	// Back-compat hidden aliases.
	cmd.Flags().Bool("rotation", false, "Deprecated alias for --managed-accounts")
	cmd.Flags().Bool("no-rotation", false, "Deprecated alias for --no-managed-accounts")
	_ = cmd.Flags().MarkHidden("rotation")
	_ = cmd.Flags().MarkHidden("no-rotation")
	cmd.Flags().String("worktree-dir", "", "Set worktree base directory")
	cmd.Flags().String("default-agent", "", "Set the default agent plugin (e.g. claude, opencode)")
	cmd.Flags().Int("poll-interval", 0, "Set poll interval in seconds (0 = default)")
	return cmd
}

func configCmd() *cobra.Command {
	cfg := &cobra.Command{
		Use:   "config",
		Short: "Manage configuration",
	}

	init := &cobra.Command{
		Use:   "init",
		Short: "Initialize bossd plugin settings in settings.json from a plugin directory",
		Long: "Initialize bossd plugin settings in settings.json from a plugin directory.\n\n" +
			"Unrelated to `boss init`, which writes the .boss-skills.json the boss skills read.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConfigInit(cmd)
		},
	}
	init.Flags().String("plugin-dir", "", "Directory containing plugin binaries (auto-detected if omitted)")

	cfg.AddCommand(init)
	return cfg
}

type skillInstallAgent struct {
	name    string
	command string
	agent   libskillinstall.Agent
}

var (
	skillInstallAgents = []skillInstallAgent{
		{name: "Claude", command: "claude", agent: libskillinstall.AgentClaude},
		{name: "Codex", command: "codex", agent: libskillinstall.AgentCodex},
	}
	skillInstallLookPath   = exec.LookPath
	skillInstallIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }
	skillInstallReadAnswer = func() string {
		var answer string
		if _, err := fmt.Scanln(&answer); err != nil {
			return ""
		}
		return answer
	}
)

// maybeInstallSkills prompts the user to install or update boss skills for each
// available coding agent on startup. Prompts are shown in agent order.
func maybeInstallSkills() error {
	if os.Getenv("BOSS_SKIP_SKILLS") != "" {
		return nil
	}
	if !skillInstallIsTerminal() {
		// Headless (daemon/tmux/cron) boss invocations previously bailed here,
		// so a merged skill edit never reached live sessions. Instead of
		// prompting, silently self-heal a stale-but-installed tree (update-only,
		// never a fresh install into an empty dir) so the on-disk global skills
		// track the checkout payload when available, otherwise this binary's
		// embed.
		return selfHealSkills()
	}
	payload, err := startupSkillPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping checkout skill refresh: %v\n", err)
		return nil // non-fatal
	}
	manifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return nil // non-fatal
	}
	settings, _ := config.Load()
	settingsChanged := false

	for _, target := range skillInstallAgents {
		if _, err := skillInstallLookPath(target.command); err != nil {
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			continue
		}
		installed := libskillinstall.IsInstalled(dir)
		needsUpdate := false
		if installed {
			needsUpdate, err = libskillinstall.NeedsUpdate(dir, payload.fsys)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to check %s skills: %v\n", target.name, err)
				continue
			}
		}
		agentName := string(target.agent)
		if installed && !needsUpdate {
			if rememberInstalledSkillManifest(&settings, agentName, manifest) {
				settingsChanged = true
			}
			continue
		}
		if skillPromptDeclined(settings, target.agent, installed, manifest) {
			continue
		}

		action := "Install"
		preposition := "to"
		if installed {
			action = "Update"
			preposition = "in"
		}
		// Clear any stranded input-reporting modes before blocking on the
		// prompt. This hook runs ahead of the TUI's own self-heal, so without
		// this a user relaunching boss to fix a terminal that is spewing mouse
		// reports would have that garbage echoed into the answer line — and
		// fmt.Scanln would consume it as a non-empty, non-"n" answer. Acute on
		// the first run after an upgrade, which is exactly when a payload change
		// makes this prompt appear. See BOS-650.
		writeStderrReset()
		fmt.Fprintf(os.Stderr, "%s boss skills for %s %s %s? [Y/n] ", action, target.name, preposition, dir)
		answer := strings.ToLower(strings.TrimSpace(skillInstallReadAnswer()))
		if answer == "n" || answer == "no" {
			if rememberDeclinedSkillPrompt(&settings, agentName, manifest) {
				settingsChanged = true
			}
			continue
		}
		if err := libskillinstall.Extract(dir, payload.fsys); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to install %s skills: %v\n", target.name, err)
			continue
		}
		if recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
		if installed {
			fmt.Fprintf(os.Stderr, "Boss skills updated for %s.\n", target.name)
		} else {
			fmt.Fprintf(os.Stderr, "Boss skills installed for %s.\n", target.name)
		}
	}
	if settingsChanged {
		_ = config.Save(settings)
	}
	warnBinarySkillsDrift(payload, false, manifest)
	return nil
}

func skillPromptDeclined(settings config.Settings, agent libskillinstall.Agent, installed bool, manifest string) bool {
	agentName := string(agent)
	declined := settings.SkillsDeclinedByAgent[agentName]
	declinedManifest := settings.SkillsDeclinedManifestByAgent[agentName]
	if agent == libskillinstall.AgentClaude && settings.SkillsDeclined && !declined && declinedManifest == "" {
		return !installed
	}
	return declined && declinedManifest == manifest
}

func rememberDeclinedSkillPrompt(settings *config.Settings, agentName, manifest string) bool {
	if settings.SkillsDeclinedByAgent == nil {
		settings.SkillsDeclinedByAgent = map[string]bool{}
	}
	if settings.SkillsDeclinedManifestByAgent == nil {
		settings.SkillsDeclinedManifestByAgent = map[string]string{}
	}
	changed := !settings.SkillsDeclinedByAgent[agentName] || settings.SkillsDeclinedManifestByAgent[agentName] != manifest
	settings.SkillsDeclinedByAgent[agentName] = true
	settings.SkillsDeclinedManifestByAgent[agentName] = manifest
	return changed
}

func rememberInstalledSkillManifest(settings *config.Settings, agentName, manifest string) bool {
	if settings.SkillsInstalledManifestByAgent == nil {
		settings.SkillsInstalledManifestByAgent = map[string]string{}
	}
	changed := settings.SkillsInstalledManifestByAgent[agentName] != manifest
	settings.SkillsInstalledManifestByAgent[agentName] = manifest
	return changed
}

func clearDeclinedSkillPrompt(settings *config.Settings, agentName string) bool {
	changed := false
	if settings.SkillsDeclinedByAgent != nil {
		if settings.SkillsDeclinedByAgent[agentName] {
			changed = true
		}
		delete(settings.SkillsDeclinedByAgent, agentName)
	}
	if settings.SkillsDeclinedManifestByAgent != nil {
		if _, ok := settings.SkillsDeclinedManifestByAgent[agentName]; ok {
			changed = true
		}
		delete(settings.SkillsDeclinedManifestByAgent, agentName)
	}
	return changed
}

// recordSkillInstall applies the post-extract settings bookkeeping shared by the
// interactive prompt, the non-interactive `boss skills` command, and the headless
// self-heal: record the installed manifest, clear any declined-prompt state, and
// clear the legacy Claude decline bit. Returns whether settings changed.
func recordSkillInstall(settings *config.Settings, agent libskillinstall.Agent, manifest string) bool {
	agentName := string(agent)
	changed := rememberInstalledSkillManifest(settings, agentName, manifest)
	if clearDeclinedSkillPrompt(settings, agentName) {
		changed = true
	}
	if agent == libskillinstall.AgentClaude {
		if settings.SkillsDeclined {
			changed = true
		}
		settings.SkillsDeclined = false
	}
	return changed
}

// selfHealSkills refreshes stale-but-installed skill trees for each agent on PATH
// without prompting. It never fresh-installs into an empty target (a headless
// first run should not silently populate a user's global skills dir) and never
// prints a [Y/n] prompt, so daemon/tmux/cron `boss` invocations keep the global
// skills current instead of bailing. The caller has already honored
// BOSS_SKIP_SKILLS and the non-TTY check.
func selfHealSkills() error {
	payload, err := startupSkillPayload()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping checkout skill refresh: %v\n", err)
		return nil // non-fatal
	}
	manifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return nil // non-fatal
	}
	// A malformed/unreadable settings.json makes config.Load return default
	// settings alongside an error. Saving those defaults would silently replace
	// the user's real file, so on a load error we still refresh the skill trees
	// (the point of self-heal) but skip all manifest bookkeeping and the save.
	settings, loadErr := config.Load()
	if loadErr != nil {
		fmt.Fprintf(os.Stderr, "Warning: skipping skill-manifest bookkeeping; failed to load settings: %v\n", loadErr)
	}
	settingsChanged := false
	refreshed := false
	installedTargetsCurrent := true
	for _, target := range skillInstallAgents {
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			installedTargetsCurrent = false
			continue
		}
		if _, err := skillInstallLookPath(target.command); err != nil {
			if libskillinstall.IsInstalled(dir) {
				installedTargetsCurrent = false
			}
			continue
		}
		// Honor a previously declined update for this exact manifest. An
		// interactive `n` records the decline via rememberDeclinedSkillPrompt,
		// and the interactive path skips the update through skillPromptDeclined.
		// Silently refreshing here would reduce that opt-out to interactive
		// sessions only — every headless daemon/tmux/cron `boss` invocation
		// would overwrite the declined tree. Explicit `boss skills sync/install`
		// remains the override. On a settings load error we cannot read the
		// decline state, so we fall through and refresh (matching the
		// bookkeeping skip above); default settings carry no declines anyway.
		if loadErr == nil && skillPromptDeclined(settings, target.agent, true, manifest) {
			installedTargetsCurrent = false
			continue
		}
		updated, err := libskillinstall.EnsureUpdated(dir, payload.fsys)
		if err != nil {
			installedTargetsCurrent = false
			fmt.Fprintf(os.Stderr, "Warning: failed to refresh %s skills: %v\n", target.name, err)
			continue
		}
		if !updated {
			continue
		}
		refreshed = true
		if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
	}
	if settingsChanged {
		_ = config.Save(settings)
	}
	warnBinarySkillsDrift(payload, refreshed && installedTargetsCurrent, manifest)
	return nil
}

func trashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Manage archived sessions",
	}

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashLS(cmd)
		},
	}

	restore := &cobra.Command{
		Use:   "restore <session-id>",
		Short: "Restore an archived session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runResurrect(cmd, args[0])
		},
	}

	del := &cobra.Command{
		Use:   "delete <session-id>",
		Short: "Permanently delete an archived session",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashDelete(cmd, args[0])
		},
	}
	del.Flags().BoolP("yes", "y", false, "Skip confirmation prompt")

	empty := &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete all archived sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashEmpty(cmd)
		},
	}
	empty.Flags().String("older-than", "", "Only delete sessions archived longer than this duration (e.g. 30d)")

	cmd.AddCommand(ls, restore, del, empty)

	return cmd
}
