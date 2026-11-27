// Package setupcheck defines the registry of Bossanova setup checks and
// evaluates them purely from gathered facts without performing I/O.
package setupcheck

import (
	"fmt"
	"slices"
	"strings"
)

// AddedInVersion is the first release shipping this check registry.
const AddedInVersion = "v1.202.0"

// Area classifies the setup domain a check belongs to.
type Area int

const (
	// AreaUnspecified is the zero value.
	AreaUnspecified Area = iota
	// AreaMachine covers host-local daemon and tool checks.
	AreaMachine
	// AreaRepo covers repository registration and configuration checks.
	AreaRepo
	// AreaTracker covers issue tracker checks.
	AreaTracker
	// AreaCI covers continuous integration checks.
	AreaCI
	// AreaProof covers proof-publishing checks.
	AreaProof
	// AreaCrons covers scheduled pipeline and sweep checks.
	AreaCrons
	// AreaCloudsAndDevices covers cloud and device roster checks.
	AreaCloudsAndDevices
)

// Status describes the evaluated state of one setup check.
type Status int

const (
	// StatusUnspecified is the zero value.
	StatusUnspecified Status = iota
	// StatusOK indicates the check's requirement holds.
	StatusOK
	// StatusMissing indicates a required item is absent or not configured.
	StatusMissing
	// StatusOutdated indicates an installed artifact does not match the current version.
	StatusOutdated
	// StatusRecommended indicates an optional improvement is available.
	StatusRecommended
	// StatusDeclined indicates the user explicitly declined a recommendation.
	StatusDeclined
	// StatusBlocked indicates the check could not be evaluated or a prerequisite failed.
	StatusBlocked
)

// FixKind classifies how a non-OK check can be resolved.
type FixKind int

const (
	// FixKindUnspecified is the zero value.
	FixKindUnspecified FixKind = iota
	// FixKindCommand provides a concrete CLI command to run.
	FixKindCommand
	// FixKindAgentTask provides prose instructions for an agent or operator.
	FixKindAgentTask
)

// Fix describes the remediation for a non-OK setup check.
type Fix struct {
	Kind      FixKind
	Command   string
	AgentTask string
}

// Check defines a registered setup check's stable metadata.
type Check struct {
	ID             string
	Area           Area
	Title          string
	AddedInVersion string
}

// Result is the evaluated outcome of one registered setup check.
type Result struct {
	ID             string
	Area           Area
	Status         Status
	Title          string
	Evidence       string
	Recommendation string
	Fix            *Fix
	AddedInVersion string
}

// BinaryState classifies a PATH lookup result; the zero value fails closed.
type BinaryState int

const (
	// BinaryUnreadable indicates the lookup failed unexpectedly or was not performed.
	BinaryUnreadable BinaryState = iota
	// BinaryFound indicates the binary resolved on PATH.
	BinaryFound
	// BinaryMissing indicates the binary was not found on PATH.
	BinaryMissing
)

// BinaryFact holds the result of looking up one executable on the daemon PATH.
type BinaryFact struct {
	State BinaryState
	Path  string
	Err   string
}

// GHAuthState classifies the daemon-side gh CLI authentication probe; the zero value fails closed.
type GHAuthState int

const (
	// GHAuthUnreadable indicates gh auth could not be verified.
	GHAuthUnreadable GHAuthState = iota
	// GHAuthOK indicates gh authenticated to GitHub from the daemon environment.
	GHAuthOK
	// GHAuthNotInstalled indicates gh was not found on the daemon PATH.
	GHAuthNotInstalled
	// GHAuthNotLoggedIn indicates gh ran but lacks usable GitHub credentials.
	GHAuthNotLoggedIn
)

// GHAuthFact holds the daemon-side gh authentication probe result.
type GHAuthFact struct {
	State  GHAuthState
	Detail string
}

// SkillsState classifies installed skill version parity; the zero value fails closed.
type SkillsState int

const (
	// SkillsUnreadable indicates installed skill versions could not be verified.
	SkillsUnreadable SkillsState = iota
	// SkillsCurrent indicates installed skills match the running daemon.
	SkillsCurrent
	// SkillsMissing indicates no Bossanova skills are installed for any supported agent.
	SkillsMissing
	// SkillsOutdated indicates installed skills are older than the running daemon.
	SkillsOutdated
	// SkillsDaemonOutdated indicates the running daemon is older than installed skills.
	SkillsDaemonOutdated
)

// SkillsFact holds the daemon-vs-skills version comparison result.
type SkillsFact struct {
	State         SkillsState
	DaemonVersion string
	SkillsVersion string
	Err           string
}

// Facts contains already-gathered inputs for pure setup check evaluation.
// Zero-value fields fail closed so an unread source never reports StatusOK.
type Facts struct {
	GOOS string

	NoWorkspacePath bool
	WorkspacePath   string
	RepoRegistered  bool
	RepoID          string
	RepoLocalPath   string
	RepoUnreadable  string

	PluginHostUnreadable bool
	AgentRunners         []string
	// AgentCLIs maps each loaded runner's CLI binary name (AgentCLIBinary(runner))
	// to its daemon PATH lookup result.
	AgentCLIs map[string]BinaryFact

	GHAuth GHAuthFact
	Node   BinaryFact
	Tmux   BinaryFact
	Skills SkillsFact
}

var registry = []Check{
	{
		ID:             "daemon-running",
		Area:           AreaMachine,
		Title:          "Bossanova daemon running",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "repo-registered",
		Area:           AreaRepo,
		Title:          "Repository registered with bossd",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "agent-runner",
		Area:           AreaMachine,
		Title:          "Agent runner plugin loaded",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "agent-cli-on-path",
		Area:           AreaMachine,
		Title:          "Agent CLI on daemon PATH",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "gh-auth",
		Area:           AreaMachine,
		Title:          "GitHub CLI authenticated in daemon environment",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "node-on-daemon-path",
		Area:           AreaMachine,
		Title:          "Node.js on daemon PATH",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "tmux-on-daemon-path",
		Area:           AreaMachine,
		Title:          "tmux on daemon PATH",
		AddedInVersion: AddedInVersion,
	},
	{
		ID:             "skills-current",
		Area:           AreaMachine,
		Title:          "Installed skills match daemon version",
		AddedInVersion: AddedInVersion,
	},
}

// Definitions returns a copy of all registered setup check definitions in registry order.
func Definitions() []Check {
	return slices.Clone(registry)
}

// IDs returns a fresh slice of registered check IDs in registry order.
func IDs() []string {
	out := make([]string, len(registry))
	for i, c := range registry {
		out[i] = c.ID
	}
	return out
}

// AgentCLIBinary returns the executable name expected on PATH for a loaded agent runner.
func AgentCLIBinary(runner string) string {
	switch runner {
	case "claude":
		return "claude"
	case "codex":
		return "codex"
	case "antigravity":
		return "agy"
	case "hermes":
		return "hermes"
	default:
		return runner
	}
}

// Evaluate computes deterministic results for every registered check in registry order.
func Evaluate(f Facts) []Result {
	return []Result{
		evaluateDaemonRunning(registry[0]),
		evaluateRepoRegistered(registry[1], f),
		evaluateAgentRunner(registry[2], f),
		evaluateAgentCLIOnPath(registry[3], f),
		evaluateGHAuth(registry[4], f),
		evaluateNodeOnDaemonPath(registry[5], f),
		evaluateTmuxOnDaemonPath(registry[6], f),
		evaluateSkillsCurrent(registry[7], f),
	}
}

func baseResult(c Check) Result {
	return Result{
		ID:             c.ID,
		Area:           c.Area,
		Title:          c.Title,
		AddedInVersion: c.AddedInVersion,
	}
}

func commandFix(cmd string) *Fix {
	return &Fix{Kind: FixKindCommand, Command: cmd}
}

func agentTaskFix(task string) *Fix {
	return &Fix{Kind: FixKindAgentTask, AgentTask: task}
}

func registerRepoCommand(workspacePath string) string {
	quoted := shellQuotePath(workspacePath)
	if strings.HasPrefix(workspacePath, "-") {
		return "boss init --register --merge --dir=" + quoted
	}
	return "boss init --register --merge --dir " + quoted
}

func shellQuotePath(s string) string {
	if s == "" {
		return "''"
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_./:=@", r):
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

func nonEmpty(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

func sortedUniqueStrings(in []string) []string {
	var out []string
	for _, s := range in {
		trimmed := strings.TrimSpace(s)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func evaluateDaemonRunning(c Check) Result {
	r := baseResult(c)
	r.Status = StatusOK
	r.Evidence = "bossd daemon is running and answering RPCs"
	return r
}

func evaluateRepoRegistered(c Check, f Facts) Result {
	r := baseResult(c)
	switch {
	case f.RepoUnreadable != "":
		r.Status = StatusBlocked
		r.Evidence = fmt.Sprintf("could not inspect repository registration: %s", f.RepoUnreadable)
		r.Recommendation = "Verify the bossd repository store is healthy and re-run boss init check."
		r.Fix = agentTaskFix("Inspect bossd repository storage errors and retry boss init check.")
	case f.NoWorkspacePath || (!f.RepoRegistered && strings.TrimSpace(f.WorkspacePath) == ""):
		r.Status = StatusBlocked
		r.Evidence = "no repository path or repo_id provided"
		r.Recommendation = "Pass --dir <path> or run boss init check inside a git repository to check registration."
		r.Fix = agentTaskFix("Run boss init check from a repository checkout or pass --dir <path>.")
	case !f.RepoRegistered:
		r.Status = StatusMissing
		r.Evidence = fmt.Sprintf("workspace %q is not registered with bossd", f.WorkspacePath)
		r.Recommendation = "Register this repository with bossd so Bossanova can manage sessions, worktrees and checks."
		r.Fix = commandFix(registerRepoCommand(f.WorkspacePath))
	default:
		r.Status = StatusOK
		if f.RepoLocalPath != "" {
			r.Evidence = fmt.Sprintf("registered as %s (%s)", f.RepoID, f.RepoLocalPath)
		} else {
			r.Evidence = fmt.Sprintf("registered as %s", f.RepoID)
		}
	}
	return r
}

func evaluateAgentRunner(c Check, f Facts) Result {
	r := baseResult(c)
	if f.PluginHostUnreadable {
		r.Status = StatusBlocked
		r.Evidence = "plugin host unavailable in bossd"
		r.Recommendation = "Check daemon plugin loading with boss daemon doctor."
		r.Fix = commandFix("boss daemon doctor")
		return r
	}
	runners := sortedUniqueStrings(f.AgentRunners)
	if len(runners) == 0 {
		r.Status = StatusMissing
		r.Evidence = "no agent runner plugins are loaded in bossd"
		r.Recommendation = "Install or enable at least one agent runner plugin and restart bossd."
		r.Fix = commandFix("boss daemon doctor")
		return r
	}
	r.Status = StatusOK
	r.Evidence = fmt.Sprintf("loaded agent runners: %s", strings.Join(runners, ", "))
	return r
}

func evaluateAgentCLIOnPath(c Check, f Facts) Result {
	r := baseResult(c)
	runners := sortedUniqueStrings(f.AgentRunners)
	if f.PluginHostUnreadable || len(runners) == 0 {
		r.Status = StatusBlocked
		r.Evidence = "cannot verify agent CLIs without loaded agent runner plugins"
		r.Recommendation = "Load at least one agent runner plugin before verifying agent CLIs on the daemon PATH."
		r.Fix = commandFix("boss daemon doctor")
		return r
	}

	var (
		unreadable []string
		missing    []string
		found      []string
		seenBins   = make(map[string]bool, len(runners))
	)
	for _, runner := range runners {
		bin := AgentCLIBinary(runner)
		if seenBins[bin] {
			continue
		}
		seenBins[bin] = true
		fact := f.AgentCLIs[bin]
		switch fact.State {
		case BinaryFound:
			if fact.Path != "" {
				found = append(found, fmt.Sprintf("%s (%s)", bin, fact.Path))
			} else {
				found = append(found, bin)
			}
		case BinaryMissing:
			missing = append(missing, bin)
		default:
			if fact.Err != "" {
				unreadable = append(unreadable, fmt.Sprintf("%s (%s)", bin, fact.Err))
			} else {
				unreadable = append(unreadable, bin)
			}
		}
	}

	switch {
	case len(unreadable) > 0:
		r.Status = StatusBlocked
		r.Evidence = fmt.Sprintf("unable to verify agent CLI(s) on daemon PATH: %s", strings.Join(unreadable, ", "))
		r.Recommendation = "Verify that bossd can search its service PATH for each loaded agent runner CLI."
		r.Fix = agentTaskFix(fmt.Sprintf("Inspect the daemon service PATH and daemon_path_extra so bossd can resolve %s.", strings.Join(unreadable, ", ")))
	case len(missing) > 0:
		r.Status = StatusMissing
		r.Evidence = fmt.Sprintf("agent CLI(s) missing from daemon PATH: %s", strings.Join(missing, ", "))
		r.Recommendation = "Make each loaded agent runner's CLI visible on the daemon service PATH."
		r.Fix = agentTaskFix(fmt.Sprintf("Install %s or add its directory to daemon_path_extra in Bossanova settings and restart bossd so the daemon PATH resolves it.", strings.Join(missing, ", ")))
	default:
		r.Status = StatusOK
		r.Evidence = strings.Join(found, ", ")
	}
	return r
}

func evaluateGHAuth(c Check, f Facts) Result {
	r := baseResult(c)
	switch f.GHAuth.State {
	case GHAuthOK:
		r.Status = StatusOK
		r.Evidence = nonEmpty(f.GHAuth.Detail, "bossd's gh CLI authenticates to GitHub.")
	case GHAuthNotInstalled:
		r.Status = StatusMissing
		r.Evidence = nonEmpty(f.GHAuth.Detail, "gh CLI not found on bossd PATH")
		r.Recommendation = "Install the GitHub CLI (gh) on the daemon PATH and authenticate it for GitHub operations."
		r.Fix = agentTaskFix("Install the GitHub CLI (gh) or add its directory to daemon_path_extra in Bossanova settings, restart bossd, and run gh auth login.")
	case GHAuthNotLoggedIn:
		r.Status = StatusMissing
		r.Evidence = nonEmpty(f.GHAuth.Detail, "bossd's gh CLI is not authenticated to GitHub")
		r.Recommendation = "Authenticate gh in the daemon environment so PR creation, checks and merges succeed."
		r.Fix = commandFix("gh auth login")
	default:
		r.Status = StatusBlocked
		r.Evidence = nonEmpty(f.GHAuth.Detail, "unable to verify gh authentication in bossd environment")
		r.Recommendation = "Diagnose daemon environment and GitHub connectivity with boss daemon doctor."
		r.Fix = commandFix("boss daemon doctor")
	}
	return r
}

func evaluateNodeOnDaemonPath(c Check, f Facts) Result {
	r := baseResult(c)
	switch f.Node.State {
	case BinaryFound:
		r.Status = StatusOK
		if f.Node.Path != "" {
			r.Evidence = fmt.Sprintf("resolved node at %s", f.Node.Path)
		} else {
			r.Evidence = "node found on daemon PATH"
		}
	case BinaryMissing:
		r.Status = StatusMissing
		r.Evidence = nonEmpty(f.Node.Err, "node not found on bossd PATH")
		r.Recommendation = "Make Node.js visible on the daemon service PATH so Bossanova skills and helper scripts can run."
		r.Fix = agentTaskFix("Install Node.js or add the directory containing node to daemon_path_extra in Bossanova settings and restart bossd.")
	default:
		r.Status = StatusBlocked
		r.Evidence = nonEmpty(f.Node.Err, "unable to probe node on bossd PATH")
		r.Recommendation = "Verify that bossd can search its service PATH for node."
		r.Fix = agentTaskFix("Inspect the daemon service PATH and daemon_path_extra so bossd can resolve node.")
	}
	return r
}

func evaluateTmuxOnDaemonPath(c Check, f Facts) Result {
	r := baseResult(c)
	switch f.Tmux.State {
	case BinaryFound:
		r.Status = StatusOK
		if f.Tmux.Path != "" {
			r.Evidence = fmt.Sprintf("resolved tmux at %s", f.Tmux.Path)
		} else {
			r.Evidence = "tmux found on daemon PATH"
		}
	case BinaryMissing:
		r.Status = StatusMissing
		r.Evidence = nonEmpty(f.Tmux.Err, "tmux not found on bossd PATH")
		r.Recommendation = "Install tmux on the daemon service PATH so bossd can manage interactive agent sessions."
		if f.GOOS == "darwin" {
			r.Fix = commandFix("brew install tmux")
		} else {
			r.Fix = agentTaskFix("Install tmux and ensure it is visible on the daemon service PATH or daemon_path_extra, then restart bossd.")
		}
	default:
		r.Status = StatusBlocked
		r.Evidence = nonEmpty(f.Tmux.Err, "unable to probe tmux on bossd PATH")
		r.Recommendation = "Verify that bossd can search its service PATH for tmux."
		r.Fix = agentTaskFix("Inspect the daemon service PATH and daemon_path_extra so bossd can resolve tmux.")
	}
	return r
}

func evaluateSkillsCurrent(c Check, f Facts) Result {
	r := baseResult(c)
	switch f.Skills.State {
	case SkillsCurrent:
		r.Status = StatusOK
		if f.Skills.DaemonVersion != "" {
			r.Evidence = fmt.Sprintf("installed skills match daemon %s", f.Skills.DaemonVersion)
		} else {
			r.Evidence = "installed skills match running daemon version"
		}
	case SkillsMissing:
		r.Status = StatusMissing
		r.Evidence = "no Bossanova skills are installed for any supported agent"
		r.Recommendation = "Install Bossanova skills so coding agents can run Bossanova workflows."
		r.Fix = commandFix("boss skills install")
	case SkillsOutdated:
		r.Status = StatusOutdated
		r.Evidence = fmt.Sprintf("installed skills %s are older than daemon %s", nonEmpty(f.Skills.SkillsVersion, "unknown"), nonEmpty(f.Skills.DaemonVersion, "unknown"))
		r.Recommendation = "Sync installed skills so they match the running daemon version."
		r.Fix = commandFix("boss skills sync")
	case SkillsDaemonOutdated:
		r.Status = StatusOutdated
		r.Evidence = fmt.Sprintf("running daemon %s is older than installed skills %s", nonEmpty(f.Skills.DaemonVersion, "unknown"), nonEmpty(f.Skills.SkillsVersion, "unknown"))
		r.Recommendation = "Restart bossd so the running daemon matches the installed skills."
		r.Fix = commandFix("boss daemon restart")
	default:
		r.Status = StatusBlocked
		r.Evidence = nonEmpty(f.Skills.Err, "unable to verify installed skill versions against daemon")
		r.Recommendation = "Sync skills so installed skill metadata is readable and matches the daemon."
		r.Fix = commandFix("boss skills sync")
	}
	return r
}
