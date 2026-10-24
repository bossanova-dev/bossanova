// Package main is the entry point for the boss CLI.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/recurser/boss/internal/adapter/sshhost"

	"github.com/recurser/boss/api/cli/account"
	"github.com/recurser/boss/api/cli/cron"
	climcp "github.com/recurser/boss/api/cli/mcp"
	"github.com/recurser/boss/api/cli/other"
	"github.com/recurser/boss/api/cli/plugins"
	"github.com/recurser/boss/api/cli/skills"
	"github.com/recurser/boss/api/cli/trash"

	"github.com/recurser/boss/api/cli/broadcast"
	"github.com/recurser/boss/api/cli/callback"
	"github.com/recurser/boss/api/cli/notes"
	"github.com/recurser/boss/api/cli/trigger"
	"github.com/recurser/boss/api/cli/webhook"
	"golang.org/x/term"

	"github.com/recurser/boss/api/cli/cliutil"
	repocli "github.com/recurser/boss/api/cli/repo"

	settingscli "github.com/recurser/boss/api/cli/settings"
	"github.com/recurser/boss/internal/domain/clidoc"
	"github.com/recurser/bossalib/buildinfo"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/errortrack"
	bossalog "github.com/recurser/bossalib/log"
	"github.com/recurser/bossalib/telemetry"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"
)

func main() {
	if err := run(); err != nil {
		// An error with no message has already said everything it needs to
		// (`boss project diff --exit-code` reporting "not converged").
		if err.Error() != "" {
			fmt.Fprintf(os.Stderr, "boss: %v\n", err)
		}
		os.Exit(exitStatus(err))
	}
}

// exitStatus is 1 unless the error carries a specific status.
func exitStatus(err error) int {
	return repocli.ExitStatus(err)
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
	defer sshhost.Shutdown()

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
	return cliutil.EmitRootJSONFailure(root, os.Args[1:], err)
}

func rootCmd() *cobra.Command { return rootCmdWithSkills(newSkillsGroup()) }

type daemonCommands interface {
	Command() *cobra.Command
	WarnIfDaemonBinaryStale(*cobra.Command)
	WarnIfBossBinaryBehindCheckout(*cobra.Command)
	WarnIfDaemonUnsupervised(*cobra.Command)
}

func rootCmdWithSkills(g *skills.Group) *cobra.Command { return rootCmdWithGroups(g, newDaemonGroup()) }
func rootCmdWithGroups(g *skills.Group, daemonGroup daemonCommands) *cobra.Command {
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
			daemonGroup.WarnIfDaemonBinaryStale(cmd)
			// BOS-1300, and the same shape one binary over: a defect fixed in
			// source destroyed a live chat two days later because the running
			// `boss` predated the fix, and nothing compared the executing
			// binary's revision against the checkout. Doctor reports it in
			// full, but a diagnostic is run by someone who already suspects a
			// problem and the failure mode is not suspecting it. Warning only:
			// one line to stderr, never stdout, exit code untouched.
			daemonGroup.WarnIfBossBinaryBehindCheckout(cmd)
			// BOS-1518, the same shape a third time: a detached bossd served
			// the socket for a day and only `boss daemon doctor` said so.
			// One line to stderr, only for an unsupervised verdict, never
			// stdout, exit code untouched.
			daemonGroup.WarnIfDaemonUnsupervised(cmd)
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
			//
			// init joins for a third reason: on a terminal it runs its own
			// interview, and a startup [Y/n] would compete with its first
			// question. It offers `boss skills install` itself, through the
			// cron preflight remedy, exactly where the skills are needed.
			path := cmd.CommandPath()
			if path == "boss gen-skill" || path == "boss fix-terminal" ||
				path == "boss tail" || path == "boss cost" || path == "boss init" ||
				path == "boss skills" || strings.HasPrefix(path, "boss skills ") {
				return nil
			}
			return g.MaybeInstallSkills()
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

	addGrouped("session", lsCmd(), showCmd(), chatsCmd(), newCmd(), attachCmd(), mergeCmd(), prCmd(), archiveCmd(), renameCmd())
	addGrouped("chat", chatCmd())
	repoGroup := newRepoGroup()
	addGrouped("repo", repoGroup.RepoCommand(), repoGroup.ProjectCommand())
	addGrouped("cron", cron.New(func(cmd *cobra.Command) (cron.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}))
	addGrouped("callback", callback.New(func(cmd *cobra.Command) (callback.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, osGetenv))
	addGrouped("trigger", trigger.New(func(cmd *cobra.Command) (trigger.Client, error) {
		return settingscli.NewCloudRemote(cmd, trigger.ErrNotLoggedIn)
	}, func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }))
	addGrouped("broadcast", broadcast.New(func(cmd *cobra.Command) (broadcast.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, osGetenv))
	addGrouped("notes", notes.New(func(cmd *cobra.Command) (notes.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, func(cmd *cobra.Command) (notes.OrganizationClient, error) {
		return settingscli.NewCloudRemote(cmd, notes.ErrNotLoggedIn)
	}, osGetenv))
	addGrouped("webhook", webhook.New(func(cmd *cobra.Command) (webhook.Client, error) {
		return settingscli.NewCloudRemote(cmd, webhook.ErrNotLoggedIn)
	}))
	addGrouped("account", account.New(func(cmd *cobra.Command) (account.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}, remoteURL, hostDestination))
	addGrouped("trash", trash.New(func(cmd *cobra.Command) (trash.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}))
	addGrouped("daemon", daemonGroup.Command())
	mcpGroup := climcp.New(climcp.Config{
		SkillPayload: g.SkillPayload, ShellQuote: skills.ShellQuote, InstallSettingsLine: daemonInstallSettingsLine,
	})
	addGrouped("mcp", mcpGroup.Command(), mcpGroup.HermesCommand())
	// `boss init` joins the existing `skills` group rather than minting a new
	// one: it writes the config the boss skills read, so it belongs beside the
	// commands that install them.
	addGrouped("skills", g.Commands()...)
	settingsGroup := newSettingsGroup()
	addGrouped("settings", settingsGroup.SettingsCmd(), settingsGroup.ConfigCmd(), settingsGroup.LoginCmd(), settingsGroup.LogoutCmd(), settingsGroup.AuthStatusCmd())
	addGrouped("diagnostics", newDiagnosticsGroup().Commands()...)
	// `boss agents` sits in the plugins group rather than getting one of its
	// own: agent runners ARE loaded plugins, and the command a reader reaches
	// for next to it is `boss plugin list`.
	pluginGroup := plugins.New(func(cmd *cobra.Command) (plugins.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	})
	addGrouped("plugins", pluginGroup.PluginCommand(), pluginGroup.AgentsCommand())
	addGrouped("other", other.New(restartDaemonAfterUpgrade)...)

	// Deprecated (resurrect) and hidden (gen-skill) commands carry no group —
	// the skill extractor skips them, and cobra lists them under "Additional
	// Commands".
	root.AddCommand(trash.LegacyRestore(func(cmd *cobra.Command) (trash.Client, error) {
		if remote := remoteURL(cmd); remote != "" {
			return newRemoteClient(cmd, remote)
		}
		return newDaemonClient(cmd)
	}), newGenSkillCmd())

	// Installed once on the root, after the whole tree is registered: cobra's
	// FlagErrorFunc walks up to the parent when a command sets none, so this
	// single registration covers every subcommand's flag rejections.
	cliutil.InstallFlagErrorHook(root)

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

// isTailInvocation reports whether argv selects a log/cost diagnostic that must
// bypass the startup skill installer, scanning past the
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
		return arg == "tail" || arg == "cost"
	}
	return false
}
