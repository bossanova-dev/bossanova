package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/recurser/bossalib/bossmcp"
	"github.com/recurser/bossalib/config"
	"github.com/recurser/bossalib/revisiondrift"
	"github.com/spf13/cobra"

	"github.com/recurser/boss/cmd/skillgen"
)

// EnvReport is the stable, documented schema emitted by `boss env --json`. It
// describes the current boss context (as the managed agent sees it) plus a full
// enumerated inventory of CLI and MCP capabilities. Field names are part of the
// contract: renames are breaking changes.
type EnvReport struct {
	// Mode is one of "managed" (inside a bossanova-managed chat), "cron" (a
	// scheduler-spawned managed chat), or "standalone" (run directly by a human
	// outside a managed session).
	Mode         string      `json:"mode"`
	Profile      string      `json:"profile"` // ambient BOSS_ENV (e.g. "local"); deployment profile, not the daemon profile
	Session      EnvSession  `json:"session"`
	Cron         *EnvCron    `json:"cron,omitempty"`
	Binaries     EnvBinaries `json:"binaries"`
	Daemon       EnvDaemon   `json:"daemon"`
	Revision     EnvRevision `json:"revision"`
	Capabilities EnvCaps     `json:"capabilities"`
}

// EnvRevision reports whether the executing boss binary contains the commits
// the checkout it is being run from has. Additive: it is a new field, and no
// existing EnvReport field name changes, because renames are breaking changes.
//
// Relation is a tri-state string rather than a boolean, for the reason the
// classifier pairs every verdict with a known flag: an undeterminable input has
// to render as unknown, and a boolean has nowhere to put that. Reason carries
// WHICH unknown, so a consumer is never left to guess between "no checkout" and
// "no git".
type EnvRevision struct {
	BinaryVersion    string `json:"binary_version"`
	BinaryRevision   string `json:"binary_revision"`
	CheckoutRoot     string `json:"checkout_root"`
	CheckoutRevision string `json:"checkout_revision"`
	Relation         string `json:"relation"` // "descendant", "behind" or "unknown"
	Reason           string `json:"reason"`
	Detail           string `json:"detail,omitempty"`
}

// EnvSession holds the managed-chat identifiers. All fields are empty in
// standalone mode (only a managed launch knows them).
type EnvSession struct {
	SessionID      string `json:"session_id"`
	AgentSessionID string `json:"agent_session_id"`
	RepoID         string `json:"repo_id"`
	Agent          string `json:"agent"`
	Worktree       string `json:"worktree"`
}

// EnvCron holds scheduler identifiers; present only when Mode == "cron".
type EnvCron struct {
	JobID string `json:"job_id"`
	Name  string `json:"name"`
}

// EnvBinaries holds resolved config + trusted-binary paths.
type EnvBinaries struct {
	SettingsPath string `json:"settings_path"`
	Boss         string `json:"boss"` // trusted boss binary; "" if unresolved
	MCP          string `json:"mcp"`  // trusted mcp binary; "" if unresolved
}

// EnvDaemon describes how to reach the bossd daemon.
type EnvDaemon struct {
	Socket    string `json:"socket"`
	Reachable bool   `json:"reachable"`
}

// EnvCaps is the enumerated capability inventory.
type EnvCaps struct {
	CLI []string `json:"cli"` // command paths, e.g. "boss ls"
	MCP []string `json:"mcp"` // MCP tool names, e.g. "list_sessions"
}

func envCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Report this session's boss context and the full CLI + MCP capability inventory",
		Long: "Print the current boss session context (identifiers, paths, daemon socket) " +
			"and a complete enumerated inventory of available boss CLI commands and MCP tools. " +
			"Inside a bossanova-managed chat this reflects exactly what the agent sees via its " +
			"injected BOSS_* environment; run outside a managed session it falls back to fresh " +
			"config resolution. Use --json for a stable machine-readable schema.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runEnv(cmd)
		},
	}
	cmd.Flags().Bool("json", false, "Emit a stable JSON schema instead of human-readable text")
	return cmd
}

func runEnv(cmd *cobra.Command) error {
	rep := resolveEnvReport(osGetenv)
	asJSON, _ := cmd.Flags().GetBool("json")
	if asJSON {
		b, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal env report: %w", err)
		}
		_, _ = fmt.Fprintln(cmd.OutOrStdout(), string(b))
		return nil
	}
	_, _ = fmt.Fprint(cmd.OutOrStdout(), renderEnvHuman(rep))
	return nil
}

// osGetenv is the production getenv; tests pass a fake.
var osGetenv = func(key string) string { return config.EnvOr(key, "") }

// resolveEnvReport builds the report. It reads BOSS_* from getenv first (the
// managed agent's actual view), falling back to fresh config resolution for the
// few fields a human shell can still resolve (settings path, socket). getenv is
// injected so tests can exercise managed, cron, and standalone modes without
// mutating the process environment.
func resolveEnvReport(getenv func(string) string) EnvReport {
	rep := EnvReport{
		Profile: orDefault(getenv("BOSS_ENV"), "local"),
		Session: EnvSession{
			SessionID:      getenv("BOSS_SESSION_ID"),
			AgentSessionID: getenv("BOSS_AGENT_SESSION_ID"),
			RepoID:         getenv("BOSS_REPO_ID"),
			Agent:          getenv("BOSS_AGENT"),
			Worktree:       getenv("BOSS_WORKTREE"),
		},
		Binaries: EnvBinaries{
			SettingsPath: getenv("BOSS_SETTINGS_PATH"),
			Boss:         getenv("BOSS_BIN"),
			MCP:          getenv("BOSS_MCP_BIN"),
		},
		Daemon: EnvDaemon{Socket: getenv("BOSS_SOCKET")},
	}

	// Mode classification.
	switch {
	case getenv("BOSS_CRON") == "true":
		rep.Mode = "cron"
		rep.Cron = &EnvCron{JobID: getenv("BOSS_CRON_JOB_ID"), Name: getenv("BOSS_CRON_NAME")}
	case rep.Session.SessionID != "":
		rep.Mode = "managed"
	default:
		rep.Mode = "standalone"
	}

	// Fallbacks for fields a standalone shell can still resolve.
	if rep.Binaries.SettingsPath == "" {
		if p, err := config.Path(); err == nil {
			rep.Binaries.SettingsPath = p
		}
	}
	if rep.Binaries.Boss == "" {
		rep.Binaries.Boss = config.ResolveTrustedExecutable("boss")
	}
	if rep.Binaries.MCP == "" {
		// Prefer the distributed name "boss-mcp" (falls back to "mcp"); a
		// packaged install ships "boss-mcp", so the bare name would falsely
		// report MCP unresolved here.
		rep.Binaries.MCP = config.ResolveMcpBinary()
	}
	if rep.Daemon.Socket == "" {
		rep.Daemon.Socket = resolveSocketFallback()
	}
	if rep.Daemon.Socket != "" {
		rep.Daemon.Reachable = daemonSocketReachable(rep.Daemon.Socket)
	}

	rep.Revision = envRevisionReport(envRevisionDrift())

	rep.Capabilities = EnvCaps{CLI: cliCommandPaths(), MCP: bossmcp.ToolNames()}
	return rep
}

// envRevisionDrift is the revision-drift probe resolveEnvReport calls. A seam
// for the same reason the doctor's is: resolveEnvReport is the pure, injectable
// core driven by a fake getenv, and an unstubbed git shell-out inside it would
// make every existing env test read the developer's real checkout.
//
// Only the `boss` verdict is reported here. `boss env` describes the context
// this command is running in; the bossd file's own verdict belongs to the
// diagnostic that can also act on it.
var envRevisionDrift = func() revisiondrift.Drift {
	return bossRevisionDriftProbe(context.Background())
}

// envRevisionReport projects the classifier's verdict onto the report schema.
// It adds no judgement of its own: the relation and the reason are both the
// classifier's, so this surface cannot disagree with doctor about one binary.
// The revision is emitted RAW — the real revision when one was read, and empty
// otherwise — never through RevisionLabel. RevisionLabel's "(unstamped)" and
// "(unreadable)" are display sentinels for a human reading a terminal; putting
// them in `--json` makes a machine consumer string-match prose to learn what
// `relation` and `reason` already state as data, and makes this the only field
// in the schema whose emptiness is spelled as a parenthesised word.
func envRevisionReport(drift revisiondrift.Drift) EnvRevision {
	return EnvRevision{
		BinaryVersion:    drift.BinaryVersion,
		BinaryRevision:   envRevisionValue(drift),
		CheckoutRoot:     drift.CheckoutRoot,
		CheckoutRevision: drift.CheckoutRevision,
		Relation:         drift.Relation(),
		Reason:           string(drift.Reason),
		Detail:           drift.Detail,
	}
}

// envRevisionValue is the machine-readable binary revision: the embedded
// revision when the classifier established there is one, and empty when there
// is not. Keyed on the carried RevisionStamped flag rather than on the string,
// so the "unknown" no-ldflags default is reported as absent rather than as a
// revision named "unknown".
func envRevisionValue(drift revisiondrift.Drift) string {
	if !drift.RevisionStamped {
		return ""
	}
	return drift.BinaryRevision
}

// envHumanRevisionLabel renders the binary revision for a HUMAN, restoring the
// sentinel that envRevisionValue deliberately keeps out of the JSON schema.
//
// Keyed on the carried reason, never on the revision string being empty:
// re-deriving stamped-ness from emptiness is the conflation the revisiondrift
// package exists to prevent, and it is also what would report an unreadable
// binary as an unstamped one.
func envHumanRevisionLabel(rev EnvRevision) string {
	if rev.BinaryRevision != "" {
		return rev.BinaryRevision
	}
	switch revisiondrift.Reason(rev.Reason) {
	case revisiondrift.ReasonRevisionUnreadable:
		return "(unreadable)"
	case revisiondrift.ReasonUnstamped:
		return "(unstamped)"
	default:
		// Every other outcome either carries a revision (handled above) or has
		// no claim to make about the binary's stamp, so it renders as unknown
		// rather than borrowing one of the two labels above. `default` also
		// satisfies the exhaustive linter, which this repo configures with
		// default-signifies-exhaustive.
		return "(unknown)"
	}
}

// resolveSocketFallback mirrors ResolveSessionFacts' socket logic for the
// standalone case: configured socket path if set, else <app-data>/bossd.sock.
func resolveSocketFallback() string {
	if s, err := config.Load(); err == nil {
		if sock, ok, err := config.ConfiguredSocketPath(s); err == nil && ok {
			return sock
		}
	}
	if dir, err := config.DefaultAppDataDir(); err == nil {
		return filepath.Join(dir, "bossd.sock")
	}
	return ""
}

// cliCommandPaths flattens the live cobra command tree into sorted command
// paths via the same extractor the /boss skill generator uses, so the inventory
// can never drift from the real CLI surface. Extraction errors (an unmissed
// ungrouped command) are non-fatal here: env reporting must not hard-fail, so
// an empty list is returned and the human/JSON output simply omits CLI entries.
func cliCommandPaths() []string {
	groups, err := skillgen.Extract(rootCmd())
	if err != nil {
		return nil
	}
	var paths []string
	for _, g := range groups {
		for _, c := range g.Commands {
			if c.IsGroup {
				continue // group headings carry no runnable behavior
			}
			paths = append(paths, c.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// renderEnvHuman renders the report as readable text.
func renderEnvHuman(rep EnvReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Mode:    %s\n", rep.Mode)
	fmt.Fprintf(&b, "Profile: %s\n", rep.Profile)
	if rep.Session.SessionID != "" {
		fmt.Fprintln(&b, "\nSession:")
		fmt.Fprintf(&b, "  session ID:       %s\n", rep.Session.SessionID)
		fmt.Fprintf(&b, "  agent-session ID: %s\n", rep.Session.AgentSessionID)
		fmt.Fprintf(&b, "  repo ID:          %s\n", rep.Session.RepoID)
		fmt.Fprintf(&b, "  agent:            %s\n", rep.Session.Agent)
		fmt.Fprintf(&b, "  worktree:         %s\n", rep.Session.Worktree)
	}
	if rep.Cron != nil {
		fmt.Fprintln(&b, "\nCron:")
		fmt.Fprintf(&b, "  job ID: %s\n", rep.Cron.JobID)
		fmt.Fprintf(&b, "  name:   %s\n", rep.Cron.Name)
	}
	fmt.Fprintln(&b, "\nPaths:")
	fmt.Fprintf(&b, "  settings: %s\n", rep.Binaries.SettingsPath)
	fmt.Fprintf(&b, "  boss bin: %s\n", orDefault(rep.Binaries.Boss, "(unresolved)"))
	fmt.Fprintf(&b, "  mcp bin:  %s\n", orDefault(rep.Binaries.MCP, "(unresolved)"))
	fmt.Fprintln(&b, "\nDaemon:")
	fmt.Fprintf(&b, "  socket:    %s\n", rep.Daemon.Socket)
	fmt.Fprintf(&b, "  reachable: %t\n", rep.Daemon.Reachable)
	fmt.Fprintln(&b, "\nRevision:")
	fmt.Fprintf(&b, "  boss version:       %s\n", orDefault(rep.Revision.BinaryVersion, "(unknown)"))
	fmt.Fprintf(&b, "  boss revision:      %s\n", envHumanRevisionLabel(rep.Revision))
	fmt.Fprintf(&b, "  checkout:           %s\n", orDefault(rep.Revision.CheckoutRoot, "(none found)"))
	fmt.Fprintf(&b, "  checkout revision:  %s\n", orDefault(rep.Revision.CheckoutRevision, "(unknown)"))
	fmt.Fprintf(&b, "  relation:           %s\n", rep.Revision.Relation)
	fmt.Fprintf(&b, "  reason:             %s\n", rep.Revision.Reason)
	if rep.Revision.Detail != "" {
		fmt.Fprintf(&b, "  detail:             %s\n", rep.Revision.Detail)
	}
	fmt.Fprintf(&b, "\nCapabilities (%d CLI commands, %d MCP tools):\n",
		len(rep.Capabilities.CLI), len(rep.Capabilities.MCP))
	fmt.Fprintln(&b, "  CLI commands:")
	for _, c := range rep.Capabilities.CLI {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	fmt.Fprintln(&b, "  MCP tools:")
	for _, m := range rep.Capabilities.MCP {
		fmt.Fprintf(&b, "    %s\n", m)
	}
	return b.String()
}
