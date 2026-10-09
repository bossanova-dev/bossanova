package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/recurser/boss/internal/skillinstall"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

// skillConfigModulePath is the embedded copy of the skill-config convention
// module inside skillinstall.SkillsFS. It is deliberately read with a direct
// fs.ReadFile rather than through skillinstall.Extract: Extract os.RemoveAll's
// its destination and rewrites a whole skill namespace, which is destructive
// and pointless when one self-contained module is all that is wanted.
const skillConfigModulePath = "skills/boss-plan/toolbox/skill-config.mjs"

// skillConfigSiblingModules are the toolbox modules skill-config.mjs imports by
// relative path. They are extracted next to it, or the bridge's import fails
// with ERR_MODULE_NOT_FOUND before any detector code runs.
var skillConfigSiblingModules = []string{"selection.mjs", "main-module.mjs"}

// nodeBridgeTimeout bounds each short-lived node subprocess. The detector reads
// a handful of marker files and returns; anything slower is a hang, not work.
const nodeBridgeTimeout = 60 * time.Second

const nodeBridgeWaitDelay = 5 * time.Second

// The Go->Node seam is fail-open by default, so every way it can fail to
// deliver an answer is its own named error. In particular a zero exit with
// empty stdout is NEVER "detected nothing": node prints nothing when a module's
// body never runs, which historically happened whenever a main-module predicate
// sat on the invocation path and the invoking path went through a symlink (every
// os.MkdirTemp on macOS). Reading that as an empty detection would silently
// write a config the repo never declared.
var (
	errNodeMissing     = errors.New("node runtime not found")
	errNodeExit        = errors.New("node exited non-zero")
	errNodeTimeout     = errors.New("node timed out")
	errNodeEmptyOutput = errors.New("node produced no output")
	errNodeBadJSON     = errors.New("node produced unparseable output")
	errDetectionShape  = errors.New("detector returned an unexpected shape")
)

func initCmd() *cobra.Command {
	var force, merge, assigneeMe, createLabels bool
	var states, labels []string
	var dir string
	var team string
	var repoOpts initRepoOptions
	var cronOpts initCronRequest

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a detected .boss-skills.json for this repository",
		Long: "Write a detected .boss-skills.json for this repository.\n\n" +
			"Detection reuses the boss skills' own convention module, so a repo initialised by\n" +
			"this command and a repo with no config file at all resolve to the same commands.\n" +
			"Only blocks that were actually detected are written; anything undetected is left\n" +
			"out and reported, so the built-in defaults keep supplying it.\n\n" +
			"The report also prints the MCP server declaration each coding-agent harness needs.\n" +
			"Those files belong to the harness, so this command never writes them -- but a repo\n" +
			"that names a tracker MCP server in .boss-skills.json and never declares it to the\n" +
			"harness is broken. Declare it under the trackerConfig.<tracker>.mcpServer name;\n" +
			"the skills also find it under another spelling (case, hyphens, underscores) or\n" +
			"another name, as long as it publishes the tracker's tools.\n\n" +
			"With LINEAR_API_KEY set, the Linear teams the key can see are listed once: exactly\n" +
			"one team (or a --team naming a listed one) is pinned as trackerConfig.linear.team.\n" +
			"Several teams write no tracker block and say to re-run with --team. With no key,\n" +
			"nothing is listed: --team is then written unverified, and without it no tracker\n" +
			"block is written -- the skills still auto-detect a single visible team at run time.\n" +
			"A listing failure never fails this command and no response body is printed.\n\n" +
			"Once a team is verified, its workflow states and the pipeline labels are looked up\n" +
			"and each state and label role is matched case-insensitively to the team's real\n" +
			"names; a name that is not byte-identical to the default is written to\n" +
			"trackerConfig.linear.states/labels. --state/--label choices win and are checked\n" +
			"against the team. Mapping unplanned and planned to one state, a state role with\n" +
			"no match, a truncated listing, or any lookup failure fails the command with\n" +
			"nothing written. Missing labels are reported; --create-labels creates them in\n" +
			"the team (labels are the only thing this command ever creates in Linear).\n\n" +
			"With --cron plan,build,verify (any subset) the factory cron jobs from\n" +
			"docs/skills/factory.md are created enabled for a daemon-loaded agent (--agent claude|codex;\n" +
			"Claude when both qualify), after a gate preflight in the daemon's own environment: node on\n" +
			"the daemon's service PATH, the installed gate script, and the installed skill-config module\n" +
			"accepting this config. A failed preflight creates that job disabled. An existing stage job\n" +
			"(by name, or by its prompt's leading skill) is never duplicated; it is updated only with\n" +
			"--update-crons, and its schedule, timezone and enabled state are never changed.\n\n" +
			"On a terminal, boss init asks for whatever the flags left open, in order: repository\n" +
			"registration, the Linear key, Sentry, the Linear team and state/label mapping, the\n" +
			"\"only my tickets\" filter, merging an existing config, and the cron jobs. Each\n" +
			"question shows its default and the flag that answers it; a flag already passed\n" +
			"answers its question. If the run stops early it lists what it already applied, and a\n" +
			"re-run finishes the job.\n\n" +
			"Not to be confused with `boss config init`, which initialises bossd plugin\n" +
			"settings in settings.json and has nothing to do with .boss-skills.json.",
		// Reject positional operands: the target directory is selected via --dir,
		// and a bare `boss init ../other-repo` must error rather than silently
		// initialise the working directory instead.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			ov, err := parseLinearOverrides(states, labels, assigneeMe)
			if err != nil {
				return err
			}
			if err := cronOpts.normalize(); err != nil {
				return err
			}
			// Usage errors must precede stdin reads, config detection, and daemon writes.
			if remoteURL(c) == "" && hostDestination(c) == "" {
				checked := repoOpts
				if err := checked.validateSources(os.Getenv); err != nil {
					return err
				}
			}
			opts := initOptions{
				mergeExisting: merge, overrides: ov,
				team:         team,
				command:      c,
				repo:         repoOpts,
				cron:         cronOpts,
				applied:      &initAppliedSteps{},
				createLabels: createLabels,
			}
			// On a terminal, boss init interviews: every question has a flag, a
			// flag already passed answers its question, and the answers drive
			// the same runInit the flags do. Elsewhere the flags alone decide.
			if initIsTerminal() {
				return runInitInteractive(c, dir, force, opts)
			}
			opts.admin = linearAdminFromEnv(os.Getenv)
			return runInit(c.OutOrStdout(), dir, force, opts)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing .boss-skills.json instead of refusing")
	cmd.Flags().StringVar(&dir, "dir", "", "Repository directory to inspect and write into (default: the working directory)")
	cmd.Flags().StringVar(&team, "team", "", "Linear team name to pin as trackerConfig.linear.team (a key or id resolves to the name only when LINEAR_API_KEY lists it)")
	cmd.Flags().BoolVar(&merge, "merge", false, "Merge into an existing .boss-skills.json")
	cmd.Flags().StringArrayVar(&states, "state", nil, "Linear state override role=name (repeatable)")
	cmd.Flags().StringArrayVar(&labels, "label", nil, "Linear label override role=name (repeatable)")
	cmd.Flags().BoolVar(&createLabels, "create-labels", false, "Create missing pipeline labels in the resolved Linear team (needs LINEAR_API_KEY)")
	cmd.Flags().BoolVar(&assigneeMe, "assignee-me", false, "Include only my tickets when no shared assignee filter exists")
	cmd.Flags().BoolVar(&repoOpts.register, "register", false, "Find or register this repository with local bossd")
	cmd.Flags().BoolVar(&repoOpts.noRegister, "no-register", false, "Require an existing repository registration")
	cmd.Flags().BoolVar(&repoOpts.linearStdin, "linear-key-stdin", false, "Read the Linear API key from stdin")
	cmd.Flags().BoolVar(&repoOpts.sentryStdin, "sentry-token-stdin", false, "Read the Sentry token from stdin")
	cmd.Flags().StringVar(&repoOpts.sentryOrg, "sentry-org", "", "Sentry organization slug (requires a token)")
	cmd.Flags().BoolVar(&repoOpts.storeEnv, "store-env-keys", false, "Store LINEAR_API_KEY and SENTRY_AUTH_TOKEN from the environment")
	cmd.Flags().StringSliceVar(&cronOpts.stages, "cron", nil, "Create or converge the factory cron jobs for these stages (plan,build,verify)")
	cmd.Flags().StringVar(&cronOpts.agent, "agent", "", "Agent for the factory cron jobs: claude or codex (default: claude when both qualify)")
	cmd.Flags().BoolVar(&cronOpts.updateConsent, "update-crons", false, "Update an existing factory job's prompt, gate and agent when they differ")
	return cmd
}

// initOptions carries the tracker-team inputs. admin is nil when no Linear
// credential is available; it also serves as the team lister when lister is
// nil. A lister alone resolves the team without any role mapping. Tests inject
// fakes and never reach the network.
type initOptions struct {
	mergeExisting     bool
	overrides         linearOverrides
	team              string
	lister            teamLister
	admin             linearAdmin
	createLabels      bool
	validateLinearKey func(context.Context, string) (string, error)
	command           *cobra.Command
	repo              initRepoOptions
	// resolvedRepo is a registration the caller already resolved (and stored
	// credentials for); runInit then skips its own registration step.
	resolvedRepo *pb.Repo
	cron         initCronRequest
	applied      *initAppliedSteps
}

// detectedConfig is the emitted config: exactly what detection produced and
// nothing else. DisallowUnknownFields is used when decoding into it, so a
// detector that grows a new block fails loudly here instead of having that
// block silently dropped from every config this command writes.
type detectedConfig struct {
	Commands map[string]string `json:"commands,omitempty"`
	// TrackerConfig is filled in Go after the detect decode (never decoded from
	// node), and only when a Linear team was resolved.
	TrackerConfig *trackerBlock `json:"trackerConfig,omitempty"`
}

// trackerBlock is the emitted trackerConfig. Only the team is written:
// mcpServer, states and labels come from the skill module's defaults.
type trackerBlock struct {
	Linear *linearTrackerBlock `json:"linear,omitempty"`
}

type linearTrackerBlock struct {
	Team    string `json:"team"`
	TeamKey string `json:"teamKey,omitempty"`
}

// bridgeResult is the detect invocation's payload. CONFIG_FILENAME comes back
// from the module rather than being hard-coded a second time in Go.
type bridgeResult struct {
	ConfigFilename string         `json:"configFilename"`
	Detected       detectedConfig `json:"detected"`
}

func runInit(out io.Writer, dir string, force bool, opts initOptions) error {
	if force && opts.mergeExisting {
		return fmt.Errorf("--merge and --force cannot be used together")
	}
	if err := opts.overrides.validate(); err != nil {
		return err
	}
	if err := opts.cron.normalize(); err != nil {
		return err
	}
	repoDir := dir
	if repoDir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve working directory: %w", err)
		}
		repoDir = wd
	}
	repoDir, err := filepath.Abs(repoDir)
	if err != nil {
		return fmt.Errorf("resolve --dir: %w", err)
	}
	info, err := os.Stat(repoDir)
	if err != nil {
		return fmt.Errorf("cannot access %s: %w", repoDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--dir must be a directory: %s", repoDir)
	}

	bridge, cleanup, err := newNodeBridge()
	if err != nil {
		return err
	}
	defer cleanup()

	result, err := bridge.detect(repoDir)
	if err != nil {
		return err
	}

	// Both separators are rejected on every platform, not just the one this
	// binary was built for. Windows accepts '/' as a separator as readily as
	// '\\', so a filepath.Separator test compiled there would pass "a/b" straight
	// through to filepath.Join and write outside the directory the operator
	// named. Checking the pair unconditionally also means the Unix test run
	// proves the Windows behaviour, rather than leaving it to a platform no CI
	// job here exercises.
	filename := result.ConfigFilename
	if filename == "" || strings.ContainsAny(filename, `/\`) || filename == "." || filename == ".." {
		return fmt.Errorf("%w: configFilename %q is not a plain file name", errDetectionShape, filename)
	}
	target := filepath.Join(repoDir, filename)

	// Refuse before doing anything else that could be mistaken for progress.
	// Lstat, not Stat: a symlink named .boss-skills.json is an existing entry
	// whether or not it resolves, and a DANGLING one is exactly the case Stat
	// reports as "nothing here" — after which a plain write would follow the
	// link and land outside repoDir entirely.
	var existing []byte
	if info, statErr := os.Lstat(target); statErr == nil && !force && !opts.mergeExisting {
		return fmt.Errorf("%s already exists; re-run with --force to replace it", target)
	} else if statErr == nil && opts.mergeExisting {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to merge symlink %s", target)
		}
		existing, err = os.ReadFile(target)
		if err != nil {
			return fmt.Errorf("read %s: %w", target, err)
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return fmt.Errorf("cannot access %s: %w", target, statErr)
	}

	if opts.applied == nil {
		opts.applied = &initAppliedSteps{}
	}
	repo := opts.resolvedRepo
	if repo == nil && opts.command != nil {
		if repo, err = prepareInitRepo(opts.command, repoDir, opts.repo, opts.validateLinearKey, opts.applied); err != nil {
			return err
		}
	}

	// Resolved after the refusal and before anything is encoded, so a --team that
	// names no visible team fails with nothing on disk.
	ctx := context.Background()
	if opts.lister == nil && opts.admin != nil {
		opts.lister = opts.admin
	}
	tracker, err := resolveInitTracker(ctx, opts)
	if err != nil {
		return err
	}
	ov := opts.overrides
	if existing != nil {
		ov = seedOverridesFromExisting(existing, ov)
	}
	// Role mapping needs a team the listing verified. Every mapping failure is
	// fatal and precedes the config write, so a partial mapping never lands.
	if opts.admin != nil && tracker.teamID != "" {
		team := linearTeam{ID: tracker.teamID, Name: tracker.team, Key: tracker.teamKey}
		if ov, err = runLinearMapping(ctx, out, opts.admin, team, ov, opts.createLabels, opts.applied); err != nil {
			return fmt.Errorf("%w; nothing written", err)
		}
	} else if opts.createLabels {
		_, _ = fmt.Fprintln(out, "--create-labels: no Linear team was verified with LINEAR_API_KEY, so no labels were looked up or created")
	}
	ov.Team, ov.TeamKey = tracker.team, tracker.teamKey
	opts.applied.Overrides = ov
	if tracker.team != "" {
		result.Detected.TrackerConfig = &trackerBlock{Linear: &linearTrackerBlock{Team: tracker.team, TeamKey: tracker.teamKey}}
	}

	if opts.mergeExisting || ov.hasRoleOverrides() {
		merged, err := mergeInitConfigMode(target, existing, result.Detected, ov, force, force || opts.mergeExisting)
		if err != nil {
			return err
		}
		if !merged.unchanged {
			opts.applied.ConfigPath = target
		}
		if existing != nil {
			_, _ = io.WriteString(out, merged.reportText(target))
		} else {
			writeInitReport(out, target, result.Detected, tracker, findAncestorConfig(repoDir, filename))
		}
		return runInitCronStep(out, opts, repoDir, target, repo)
	}

	encoded, err := encodeDetectedConfig(result.Detected)
	if err != nil {
		return err
	}

	// Validate the way the real consumer validates — merged over the defaults,
	// which is exactly what loadSkillConfig() does — and do it BEFORE the target
	// file is opened, so a config that fails validation leaves nothing on disk.
	if err := bridge.validate(encoded, target); err != nil {
		return err
	}

	if err := writeConfigFile(target, encoded, force); err != nil {
		return err
	}
	opts.applied.ConfigPath = target

	writeInitReport(out, target, result.Detected, tracker, findAncestorConfig(repoDir, filename))
	return runInitCronStep(out, opts, repoDir, target, repo)
}

// --- Linear team resolution ----------------------------------------------

// initTracker is the resolved tracker outcome for one boss init run. team is
// empty when no tracker block is written; guidance then says why and what to
// do, and replaces the trackerConfig reason under "Blocks skipped".
type initTracker struct {
	team, teamKey string
	teamID        string // set only when the listing verified the team
	source        string // "detected", "--team" or "--team, unverified"
	note          string
	guidance      string
}

// errUnknownTeam is the one tracker outcome that fails boss init: the operator
// named a team and the listing proves it is not visible.
var errUnknownTeam = errors.New("--team names no visible Linear team")

// autoDetectStillApplies is appended to every no-listing outcome.
const autoDetectStillApplies = "the skills still auto-detect the team at run time when exactly one is visible to the tracker MCP server; or re-run with --team <name>"

// resolveInitTracker decides what boss init writes for trackerConfig. Only a
// --team that a complete listing proves absent is an error; every network
// problem degrades to guidance. With an admin client the listing feeds role
// mapping, so a truncated listing that does not name the team fails closed.
func resolveInitTracker(ctx context.Context, opts initOptions) (initTracker, error) {
	flag := strings.TrimSpace(opts.team)
	if opts.lister == nil {
		if flag != "" {
			return initTracker{team: flag, source: "--team, unverified", note: "team not verified (no Linear listing available); written as given, and the skills match the exact team name"}, nil
		}
		return initTracker{guidance: "LINEAR_API_KEY is not set, so no Linear teams were listed and no team is pinned; " + autoDetectStillApplies}, nil
	}
	teams, hasNextPage, err := opts.lister.ListTeams(ctx)
	if err != nil {
		class := "listing failed"
		class = linearErrorClass(err, class)
		if flag != "" {
			return initTracker{team: flag, source: "--team, unverified", note: "team not verified (no Linear listing available: " + class + "); written as given, and the skills match the exact team name"}, nil
		}
		return initTracker{guidance: "Linear teams could not be listed (" + class + "), so no team is pinned; " + autoDetectStillApplies}, nil
	}
	if flag != "" {
		for _, t := range teams {
			if strings.EqualFold(flag, t.Name) || (t.Key != "" && strings.EqualFold(flag, t.Key)) || flag == t.ID {
				return initTracker{team: t.Name, teamKey: t.Key, teamID: t.ID, source: "--team"}, nil
			}
		}
		if hasNextPage && opts.admin != nil {
			return initTracker{}, fmt.Errorf("%w; re-run with --team naming a team among the first %d, or unset LINEAR_API_KEY; nothing written", &linearAPIError{op: "list Linear teams", class: "truncated"}, len(teams))
		}
		if hasNextPage {
			// More teams exist than one page shows, so absence from this page proves
			// nothing: trust the operator's explicit choice, flagged as unverified.
			return initTracker{team: flag, source: "--team, unverified", note: fmt.Sprintf("team not verified (not among the first %d of more Linear teams); written as given, and the skills match the exact team name", len(teams))}, nil
		}
		return initTracker{}, fmt.Errorf("%w: %q (visible: %s); nothing written", errUnknownTeam, flag, teamNames(teams, false))
	}
	switch {
	case hasNextPage && opts.admin != nil:
		return initTracker{}, fmt.Errorf("%w; re-run with --team naming a team among the first %d, or unset LINEAR_API_KEY; nothing written", &linearAPIError{op: "list Linear teams", class: "truncated"}, len(teams))
	case hasNextPage || len(teams) >= 2:
		count := strconv.Itoa(len(teams))
		if hasNextPage {
			count = "more than " + count
		}
		return initTracker{guidance: fmt.Sprintf("%s Linear teams are visible (%s) — re-run with --team <name> or set trackerConfig.linear.team", count, teamNames(teams, hasNextPage))}, nil
	case len(teams) == 1:
		return initTracker{team: teams[0].Name, teamKey: teams[0].Key, teamID: teams[0].ID, source: "detected"}, nil
	default:
		return initTracker{guidance: "no Linear teams are visible to LINEAR_API_KEY, so no team is pinned; re-run with --team <name> or set trackerConfig.linear.team once one exists"}, nil
	}
}

// teamNames renders listed team names in listing order, eliding past ten.
func teamNames(teams []linearTeam, more bool) string {
	if len(teams) == 0 {
		return "none"
	}
	const shown = 10
	names := make([]string, 0, shown)
	for i, t := range teams {
		if i == shown {
			more = true
			break
		}
		names = append(names, t.Name)
	}
	out := strings.Join(names, ", ")
	if more {
		out += ", …"
	}
	return out
}

// writeConfigFile performs the write the refusal above guarded. The two are
// separated by a whole node subprocess, so a stat alone cannot be the guarantee:
// without --force the create is O_EXCL, which makes "must not already exist" the
// filesystem's answer at the moment of writing rather than one that was true a
// second earlier. O_EXCL declines to follow a symlink too, so the dangling-link
// write-through the Lstat rejects cannot come back through the race. With
// --force the operator has asked for whatever is there to be replaced.
func writeConfigFile(target string, encoded []byte, force bool) error {
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		// --force replaces the ENTRY, and a symlink is an entry. O_TRUNC would
		// follow it and truncate whatever it points at — the exact write-through
		// the refusal above exists to prevent, reached through the very flag that
		// refusal's message recommends. Unlink the link and then create
		// exclusively, so a link re-created in the gap fails the open rather than
		// being written through. (O_NOFOLLOW would say this directly, but it is
		// undefined on Windows, which this binary is built for.)
		if info, lerr := os.Lstat(target); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
			if rerr := os.Remove(target); rerr != nil {
				return fmt.Errorf("replace symlink %s: %w", target, rerr)
			}
			flags = os.O_WRONLY | os.O_CREATE | os.O_EXCL
		}
	}
	f, err := os.OpenFile(target, flags, 0o644)
	if err != nil {
		if !force && errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s already exists; re-run with --force to replace it", target)
		}
		return fmt.Errorf("write %s: %w", target, err)
	}
	if _, err := f.Write(encoded); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", target, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", target, err)
	}
	return nil
}

// findAncestorConfig reports an existing config file in a PARENT directory, if
// there is one. The loader discovers its config by walking UP from the working
// directory and taking the first file it finds, so a config written in a
// subdirectory does not sit alongside an ancestor's — it replaces it outright
// for every skill run at or below that path. That is a legitimate thing to want
// in a monorepo, so it is reported rather than refused; what it must not be is
// silent, because nothing else in the run would ever mention it.
func findAncestorConfig(repoDir, filename string) string {
	repoDir = filepath.Clean(repoDir)
	dir := filepath.Dir(repoDir)
	// A filesystem root is its own parent, so it has no ancestor to shadow.
	// Without this the first candidate examined is the file runInit wrote a
	// moment ago, and the report warns that the config shadows itself.
	if dir == repoDir {
		return ""
	}
	for {
		candidate := filepath.Join(dir, filename)
		// Stat, not Lstat: this asks the question the LOADER asks — "would it
		// find a config here?" — and the loader's existsSync follows symlinks. A
		// dangling link is not a config it would ever load, so it must not be
		// reported as one being shadowed. The refusal above asks the opposite
		// question about the write target and correctly uses Lstat.
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// encodeDetectedConfig renders the detection result with stable key ordering and
// a trailing newline, so two runs over the same repo are byte-identical.
func encodeDetectedConfig(cfg detectedConfig) ([]byte, error) {
	// encoding/json emits struct fields in declaration order and map keys sorted,
	// so the ordering is a property of the encoder rather than of iteration luck.
	encoded, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode detected config: %w", err)
	}
	return append(encoded, '\n'), nil
}

// buildSystemOrder fixes the report's ordering so it never depends on map
// iteration. Labels are derived from the detector's OWN returned command
// strings — this command sniffs no marker files of its own, because a second
// detector in Go is the duplication the whole design exists to avoid.
var buildSystemOrder = []struct {
	label    string
	prefixes []string
}{
	{"Makefile", []string{"make "}},
	{"package.json", []string{"pnpm run ", "npm run ", "yarn run ", "bun run "}},
	{"Cargo.toml", []string{"cargo "}},
	{"go.mod", []string{"go "}},
}

func detectedBuildSystems(commands map[string]string) []string {
	var out []string
	for _, sys := range buildSystemOrder {
		for _, value := range commands {
			matched := false
			for _, prefix := range sys.prefixes {
				if strings.HasPrefix(value, prefix) {
					matched = true
					break
				}
			}
			if matched {
				out = append(out, sys.label)
				break
			}
		}
	}
	return out
}

// skippedBlocks are the config blocks this command deliberately never emits,
// each with the reason it is absent. They are printed so a reader can see why a
// block is missing rather than assuming detection failed. This list covers every
// key of the skill module's DEFAULT_CONFIG, so the report accounts for the whole
// default surface rather than the subset that happened to seem interesting.
var skippedBlocks = []struct{ name, reason string }{
	{"trackerConfig", "written only when a Linear team is resolved (one team visible to LINEAR_API_KEY, or --team); otherwise the skills resolve the team at run time and self-disable cleanly when none or several are visible"},
	{"lensMap", "not detectable from a repo's files; the built-in defaults supply it when the config is loaded"},
	{"adapters", "not detectable from a repo's files; the built-in defaults supply it when the config is loaded"},
	{"planStorage", "not detectable from a repo's files; the built-in defaults supply it when the config is loaded"},
	{"reviewLedger", "not detectable from a repo's files; the built-in defaults supply it when the config is loaded"},
	{"planContract", "not detectable from a repo's files; the built-in defaults supply it when the config is loaded"},
	{"reviewDefaults", "not detectable from a repo's files; the built-in defaults supply it when the config is loaded"},
	{"planDependencies", "which shared files every ticket appends to is a fact about this repo's history, not its files; the built-in block is empty and the dependency scan uses only its per-run payload until an operator declares them"},
	{"epicDefaults", "how long a child of an epic typically runs is a fact about this repo's work, not about its files, so it cannot be detected; the built-in child wall clock applies until an operator measures otherwise"},
	{"notesDefaults", "a reporting-cadence preference rather than a repository property; the built-in default runs the post-terminal notes phase (and the pre-PR knowledge phase) on every run, and an operator who wants fewer of them declares the block and turns sampleRate down"},
	{"extensionRoots", "not detectable from a repo's files; the built-in defaults scan the supported agent skill roots"},
	{"env", "describes the harness a skill runs under, not the repository; the built-in headless-detection signals apply unchanged"},
	{"publishConfig", "empty by default and validated only per existing entry, so writing an empty block would add nothing the defaults do not already supply"},
}

// --- Harness MCP declarations --------------------------------------------

// mcpServerPlaceholder stands in for the server name in every printed
// declaration when this run wrote no trackerConfig and so has no name to use:
// the operator picks one, and the same string then has to appear in both files.
const mcpServerPlaceholder = "<mcpServer>"

// defaultLinearMCPServer is the mcpServer the skill module's tracker defaults
// supply when a written Linear block names none. Declarations printed for a
// written block use it as the key, so the key is byte-identical to the
// effective mcpServer.
const defaultLinearMCPServer = "linear"

// mcpKeyRule is printed under each declaration the report shows.
const mcpKeyRule = "Use the trackerConfig.<tracker>.mcpServer name from .boss-skills.json as the server\n" +
	"key above. The skills also resolve the server under another spelling or name, as\n" +
	"long as it publishes the tracker's tools.\n"

// harness is a coding-agent harness that discovers MCP servers its own native
// way, from a file this command deliberately never writes: the file belongs to
// the harness and is routinely hand-maintained, so writing it would clobber
// declarations boss knows nothing about. Printing the exact declaration is the
// part boss can do safely.
type harness struct {
	name string
	file string
	// markers are repo paths whose presence means this harness is in use. They
	// are harness evidence only — build-system detection stays in the shared
	// convention module, and nothing here sniffs for one.
	markers []string
	render  func(server string) string
}

var supportedHarnesses = []harness{
	{
		name:    "Claude Code",
		file:    ".mcp.json",
		markers: []string{".mcp.json", ".claude", "CLAUDE.md"},
		render:  renderClaudeMCPDeclaration,
	},
	{
		name:    "Codex",
		file:    ".codex/config.toml",
		markers: []string{".codex", "AGENTS.md"},
		render:  renderCodexMCPDeclaration,
	},
}

// quoteKey renders server as a quoted string for both declaration formats. It
// is the single point where the name is turned into a key, so there is exactly
// one place that could ever transform it — and it does not: HTML escaping is
// switched off (encoding/json would otherwise turn the placeholder's angle
// brackets into <), and nothing else touches the bytes. TOML basic-string
// keys share JSON's quoting rules, so one function serves both.
func quoteKey(server string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(server); err != nil {
		return `"` + server + `"`
	}
	return strings.TrimRight(buf.String(), "\n")
}

func renderClaudeMCPDeclaration(server string) string {
	return "{\n" +
		"  \"mcpServers\": {\n" +
		"    " + quoteKey(server) + ": {\n" +
		"      \"type\": \"http\",\n" +
		"      \"url\": \"https://mcp.example.com/mcp\",\n" +
		"      \"headers\": { \"Authorization\": \"Bearer ${TRACKER_API_KEY}\" }\n" +
		"    }\n" +
		"  }\n" +
		"}\n"
}

// renderCodexMCPDeclaration always quotes the table key. Quoting is legal for
// every key and required for a hyphenated one, so quoting unconditionally is
// what lets the key stay byte-identical to the .boss-skills.json value instead
// of being rewritten into the underscore form a bare key would demand.
func renderCodexMCPDeclaration(server string) string {
	return "[mcp_servers." + quoteKey(server) + "]\n" +
		"url = \"https://mcp.example.com/mcp\"\n" +
		"bearer_token_env_var = \"TRACKER_API_KEY\"\n" +
		"required = false\n"
}

// detectHarnesses reports which harnesses this repo shows evidence of. With no
// evidence it returns all of them rather than none: the operator who has not
// set a harness up yet is precisely the one who must not finish `boss init`
// believing there was nothing left to declare.
func detectHarnesses(repoDir string) []harness {
	var found []harness
	for _, h := range supportedHarnesses {
		for _, marker := range h.markers {
			if _, err := os.Stat(filepath.Join(repoDir, marker)); err == nil {
				found = append(found, h)
				break
			}
		}
	}
	if len(found) == 0 {
		return supportedHarnesses
	}
	return found
}

// harnessDeclarations renders every declaration from ONE server value, so the
// key cannot drift between formats: there is no per-harness name to keep in
// sync, only one argument threaded through every renderer.
func harnessDeclarations(harnesses []harness, server string) string {
	var b strings.Builder
	b.WriteString("Harness MCP declarations (not written; these files belong to the harness).\n")
	if server == mcpServerPlaceholder {
		b.WriteString("If you add a trackerConfig, declare its MCP server to each harness you use:\n")
	} else {
		b.WriteString("Declare the tracker MCP server to each harness you use:\n")
	}
	for _, h := range harnesses {
		fmt.Fprintf(&b, "  %s -> %s\n", h.name, h.file)
		for line := range strings.SplitSeq(strings.TrimRight(h.render(server), "\n"), "\n") {
			b.WriteString("    " + line + "\n")
		}
	}
	for line := range strings.SplitSeq(strings.TrimRight(mcpKeyRule, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}

// writeInitReport renders the report into a string first and emits it with a
// single write. The report is advisory output on an already-successful run, so
// a failing stdout is not worth failing the command over — but the one write is
// where that judgement is made, rather than eight separate unchecked calls.
func writeInitReport(out io.Writer, target string, cfg detectedConfig, tracker initTracker, shadowed string) {
	_, _ = io.WriteString(out, initReport(target, cfg, tracker, detectHarnesses(filepath.Dir(target)), shadowed))
}

// reportWrapWidth is the column every boss init report line wraps at.
const reportWrapWidth = 72

// wrapText breaks s onto lines of at most reportWrapWidth columns, splitting
// only between words. A reason printed as one 200-column line hard-wraps in the terminal and
// takes the report's indentation with it, which is what makes a wrapped-here
// version more readable rather than merely shorter. Width is counted in runes,
// not bytes, so a non-ASCII reason wraps where it looks like it should.
func wrapText(s string) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var lines []string
	line := words[0]
	for _, w := range words[1:] {
		if utf8.RuneCountInString(line)+1+utf8.RuneCountInString(w) > reportWrapWidth {
			lines = append(lines, line)
			line = w
			continue
		}
		line += " " + w
	}
	return append(lines, line)
}

func initReport(target string, cfg detectedConfig, tracker initTracker, harnesses []harness, shadowed string) string {
	var b strings.Builder

	systems := detectedBuildSystems(cfg.Commands)
	if len(systems) == 0 {
		b.WriteString("Detected build system: none\n")
	} else {
		b.WriteString("Detected build system: " + strings.Join(systems, ", ") + "\n")
	}

	if len(cfg.Commands) == 0 {
		b.WriteString("Detected commands: none\n")
	} else {
		keys := make([]string, 0, len(cfg.Commands))
		for k := range cfg.Commands {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("Detected commands:\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "  %-8s %s\n", k, cfg.Commands[k])
		}
	}

	written := cfg.TrackerConfig != nil && cfg.TrackerConfig.Linear != nil
	if written {
		line := "Tracker team: " + cfg.TrackerConfig.Linear.Team
		if cfg.TrackerConfig.Linear.TeamKey != "" {
			line += " (" + cfg.TrackerConfig.Linear.TeamKey + ")"
		}
		if tracker.source != "" {
			line += " — " + tracker.source
		}
		b.WriteString(line + "\n")
		if tracker.note != "" {
			b.WriteString("  " + tracker.note + "\n")
		}
	}

	// The trackerConfig entry stays in skippedBlocks so every default block is
	// accounted for; only whether it renders, and its reason, depend on the run.
	blocks := make([]struct{ name, reason string }, 0, len(skippedBlocks))
	for _, block := range skippedBlocks {
		if block.name == "trackerConfig" {
			if written {
				continue
			}
			if tracker.guidance != "" {
				block.reason = tracker.guidance
			}
		}
		blocks = append(blocks, block)
	}

	b.WriteString("\nBlocks skipped (not written; the built-in defaults supply them):\n")
	// Consecutive blocks sharing a reason are named together, so the reason is
	// stated once instead of repeated verbatim under four separate names.
	for i := 0; i < len(blocks); {
		j := i
		var names []string
		for ; j < len(blocks) && blocks[j].reason == blocks[i].reason; j++ {
			names = append(names, blocks[j].name)
		}
		b.WriteString("  " + strings.Join(names, ", ") + "\n")
		for _, line := range wrapText(blocks[i].reason) {
			b.WriteString("    " + line + "\n")
		}
		i = j
	}

	b.WriteString("\n")
	server := mcpServerPlaceholder
	if written {
		server = defaultLinearMCPServer
	}
	b.WriteString(harnessDeclarations(harnesses, server))

	if shadowed != "" {
		b.WriteString("\nWarning: this config shadows " + shadowed + "\n")
		for _, line := range wrapText("Skills load the first " + filepath.Base(target) +
			" found walking up from the working directory, so every skill run at or below " +
			filepath.Dir(target) + " now reads this file and not that one. Remove this file to fall " +
			"back to the ancestor's.") {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\nWrote " + target + "\n")
	return b.String()
}

// --- Node bridge ---------------------------------------------------------

// nodeBridge is one skill-config module plus the resolved scratch directory its
// node invocations run from. The module is usually the embedded copy extracted
// into dir, but any module path works: cron setup validates against the
// INSTALLED skill tree's copy, which is what the cron gate will actually load.
type nodeBridge struct {
	dir    string // scratch dir, symlink-resolved
	module string // skill-config module the scripts import
	node   string // node executable; empty means look `node` up on PATH
}

func newNodeBridge() (*nodeBridge, func(), error) {
	scratch, err := os.MkdirTemp("", "boss-init-")
	if err != nil {
		return nil, nil, fmt.Errorf("create scratch directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }
	bridge, err := newNodeBridgeIn(scratch)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return bridge, cleanup, nil
}

// newModuleNodeBridge builds a bridge over an existing skill-config module at
// module, run by the node executable at nodePath (empty: `node` on PATH). The
// scratch directory only holds validation candidates; nothing is extracted.
func newModuleNodeBridge(module, nodePath string) (*nodeBridge, func(), error) {
	scratch, err := os.MkdirTemp("", "boss-init-module-")
	if err != nil {
		return nil, nil, fmt.Errorf("create scratch directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(scratch) }
	resolved, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("resolve scratch directory %s: %w", scratch, err)
	}
	return &nodeBridge{dir: resolved, module: module, node: nodePath}, cleanup, nil
}

// newNodeBridgeIn extracts the embedded module into scratch. The scratch path is
// resolved with filepath.EvalSymlinks first: os.MkdirTemp returns a path under
// the symlinked /var/folders/... on macOS, and every path handed to node must be
// the real one so nothing downstream can compare a typed path against a
// realpath'd one and silently disagree.
func newNodeBridgeIn(scratch string) (*nodeBridge, error) {
	resolved, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return nil, fmt.Errorf("resolve scratch directory %s: %w", scratch, err)
	}
	toolbox := path.Dir(skillConfigModulePath)
	for _, name := range append([]string{path.Base(skillConfigModulePath)}, skillConfigSiblingModules...) {
		embedded := path.Join(toolbox, name)
		data, err := fs.ReadFile(skillinstall.SkillsFS, embedded)
		if err != nil {
			return nil, fmt.Errorf("read embedded %s: %w", embedded, err)
		}
		if err := os.WriteFile(filepath.Join(resolved, name), data, 0o600); err != nil {
			return nil, fmt.Errorf("write %s: %w", filepath.Join(resolved, name), err)
		}
	}
	module := filepath.Join(resolved, path.Base(skillConfigModulePath))
	return &nodeBridge{dir: resolved, module: module}, nil
}

func (b *nodeBridge) detect(repoDir string) (bridgeResult, error) {
	// A plain dynamic import of the module by file URL. No main-module predicate
	// is anywhere on this path, so the module body cannot be skipped.
	script := fmt.Sprintf(`import { pathToFileURL } from 'node:url'
const mod = await import(pathToFileURL(%s).href)
const detected = mod.detectRepoDefaults({ cwd: %s })
process.stdout.write(JSON.stringify({ configFilename: mod.CONFIG_FILENAME, detected }))
`, jsString(b.module), jsString(repoDir))

	stdout, err := b.run(script, "detect repository defaults")
	if err != nil {
		return bridgeResult{}, err
	}

	var result bridgeResult
	dec := json.NewDecoder(bytes.NewReader(stdout))
	// Fail loudly on a shape this command does not understand rather than
	// dropping a block the detector grew since this code was written.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		// Separate "not the JSON we expect" from "not JSON at all": a detector
		// that grew a field is a shape mismatch to reconcile here, whereas
		// unparseable bytes mean the bridge itself misbehaved. Deciding it by
		// re-decoding permissively keeps the distinction off the error text.
		if permissiveErr := json.Unmarshal(stdout, new(map[string]any)); permissiveErr == nil {
			return bridgeResult{}, fmt.Errorf("%w: detect repository defaults: %v (output: %s)", errDetectionShape, err, truncate(stdout))
		}
		return bridgeResult{}, fmt.Errorf("%w: detect repository defaults: %v (output: %s)", errNodeBadJSON, err, truncate(stdout))
	}
	// Decode stops at the end of the first value, so trailing bytes would pass
	// silently. validate() uses Unmarshal, which rejects them; match it here so
	// stray output can never ride along with a well-formed result.
	if dec.More() {
		return bridgeResult{}, fmt.Errorf("%w: detect repository defaults: trailing output after the JSON result (output: %s)", errNodeBadJSON, truncate(stdout))
	}
	// trackerConfig is resolved in Go after this decode, never by the detector;
	// a detector that starts emitting one is a shape to reconcile, not to accept.
	if result.Detected.TrackerConfig != nil {
		return bridgeResult{}, fmt.Errorf("%w: detect repository defaults: unexpected trackerConfig (output: %s)", errDetectionShape, truncate(stdout))
	}
	return result, nil
}

// validate runs the merge-then-validate the real consumer performs —
// validateConfig(withTrackerDefaults(mergeConfig(DEFAULT_CONFIG, user)), source),
// in that argument order, exactly as loadSkillConfig() composes it — over the
// candidate bytes, without writing them to the target path. Without the
// tracker defaults a team-only block would fail here on its missing mcpServer
// while loading cleanly for every skill.
func (b *nodeBridge) validate(encoded []byte, source string) error {
	candidate := filepath.Join(b.dir, "candidate.json")
	if err := os.WriteFile(candidate, encoded, 0o600); err != nil {
		return fmt.Errorf("write validation candidate: %w", err)
	}
	script := fmt.Sprintf(`import { readFileSync } from 'node:fs'
import { pathToFileURL } from 'node:url'
const mod = await import(pathToFileURL(%s).href)
const user = JSON.parse(readFileSync(%s, 'utf8'))
mod.validateConfig(mod.withTrackerDefaults(mod.mergeConfig(mod.DEFAULT_CONFIG, user)), %s)
process.stdout.write(JSON.stringify({ ok: true }))
`, jsString(b.module), jsString(candidate), jsString(source))

	stdout, err := b.run(script, "validate composed config")
	if err != nil {
		return err
	}
	var ack struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(stdout, &ack); err != nil || !ack.OK {
		return fmt.Errorf("%w: validate composed config: %s", errNodeBadJSON, truncate(stdout))
	}
	return nil
}

// run executes one short-lived node process and returns its stdout, mapping
// every failure mode onto its own named error. Empty stdout with a zero exit is
// fatal, never an empty result.
func (b *nodeBridge) run(script, what string) ([]byte, error) {
	nodePath := b.node
	if nodePath == "" {
		found, err := exec.LookPath("node")
		if err != nil {
			return nil, fmt.Errorf("%w: %s requires the `node` executable on PATH (the boss skills need Node to run at all); install Node or add it to PATH: %v", errNodeMissing, what, err)
		}
		nodePath = found
	}

	ctx, cancel := context.WithTimeout(context.Background(), nodeBridgeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, nodePath, "--input-type=module", "-e", script)
	// Run from the resolved scratch dir so nothing depends on the caller's cwd.
	cmd.Dir = b.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = nodeBridgeWaitDelay

	if err := cmd.Run(); err != nil {
		// A hung bridge and a bridge that ran and failed call for different
		// responses, so the deadline gets its own sentinel rather than being
		// reported as a non-zero exit it never produced.
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%w: %s timed out after %s: %s", errNodeTimeout, what, nodeBridgeTimeout, truncate(stderr.Bytes()))
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, fmt.Errorf("%w: %s failed with exit status %d: %s", errNodeExit, what, exitErr.ExitCode(), truncate(stderr.Bytes()))
		}
		return nil, fmt.Errorf("%w: %s could not be run: %v: %s", errNodeExit, what, err, truncate(stderr.Bytes()))
	}

	out := bytes.TrimSpace(stdout.Bytes())
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: %s exited 0 but printed nothing; this is a broken bridge, not an empty result, and no config has been written: %s", errNodeEmptyOutput, what, truncate(stderr.Bytes()))
	}
	return out, nil
}

// jsString renders a Go string as a JavaScript string literal. json.Marshal is
// the encoder rather than hand-rolled quoting so no path can escape the literal.
func jsString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		// json.Marshal of a string cannot fail; keep the bridge total anyway.
		return `""`
	}
	return string(encoded)
}

func truncate(b []byte) string {
	const limit = 2000
	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		return s[:limit] + "… (truncated)"
	}
	return s
}
