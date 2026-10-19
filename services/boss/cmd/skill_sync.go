package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/recurser/boss/internal/skillinstall"
	"github.com/recurser/bossalib/config"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
)

// skillSyncMode selects how runSkillSync treats each agent's skill dir.
type skillSyncMode int

const (
	// skillSyncUpdateOnly (`boss skills sync`): EnsureUpdated per agent — a silent
	// no-op on a current tree and NEVER a fresh install into an empty dir.
	skillSyncUpdateOnly skillSyncMode = iota
	// skillSyncInstall (`boss skills install`): refresh a stale tree AND fresh-install
	// an empty one, so it doubles as first-time setup; a no-op when already current.
	skillSyncInstall
	// skillSyncForce (`boss skills install --force`): Extract unconditionally
	// (repair/overwrite), even when current.
	skillSyncForce
)

// skillPayload selects the payload a skill install writes. A trusted Bossanova
// checkout may supply agent-global skill bytes: another repository can reproduce
// the source directory layout, but must never gain write access to ~/.claude/skills
// or ~/.codex/skills by doing so. Both startup and direct skill commands require
// explicit opt-in because an origin URL alone does not authenticate checked-out
// bytes.
type selectedSkillPayload struct {
	fsys       fs.FS
	srcRoot    string
	fromSource bool
}

// trustCheckoutSkillSourcesEnv requires a deliberate opt-in before checkout
// files can replace an agent's globally installed instructions. A canonical
// remote alone is insufficient: a clone can check out an untrusted fork or PR
// revision without changing its origin URL.
const trustCheckoutSkillSourcesEnv = "BOSS_TRUST_CHECKOUT_SKILLS"

func skillPayload() (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrust(os.Getenv(trustCheckoutSkillSourcesEnv) == "1")
}

// startupSkillPayload selects a checkout only after the operator explicitly
// trusts its bytes. This prevents a canonical-looking origin from authorizing a
// local PR or dirty worktree to overwrite globally installed agent instructions.
func startupSkillPayload() (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrust(os.Getenv(trustCheckoutSkillSourcesEnv) == "1")
}

func skillPayloadWithCheckoutTrust(trustCheckout bool) (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrustMode(trustCheckout, false)
}

// skillCheckPayload selects a checkout payload without creating any source-side
// coordination files, so `boss skills check` remains safe on read-only checkouts.
func skillCheckPayload() (selectedSkillPayload, error) {
	return skillPayloadWithCheckoutTrustMode(true, true)
}

func skillPayloadWithCheckoutTrustMode(trustCheckout, readOnly bool) (selectedSkillPayload, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return selectedSkillPayload{}, fmt.Errorf("get working directory: %w", err)
	}
	if root, ok := libskillinstall.FindSourceRoot(cwd); ok && trustCheckout {
		trusted, err := trustedSkillSourceRoot(root)
		if err != nil {
			return selectedSkillPayload{}, fmt.Errorf("authenticate checkout skill source: %w", err)
		}
		if trusted {
			snapshot := snapshotCheckoutSkillFS
			if readOnly {
				snapshot = snapshotCheckoutSkillFSReadOnly
			}
			frozen, err := snapshot(root)
			if err != nil {
				return selectedSkillPayload{}, err
			}
			return selectedSkillPayload{fsys: frozen, srcRoot: root, fromSource: true}, nil
		}
	}
	return selectedSkillPayload{fsys: skillinstall.SkillsFS}, nil
}

const canonicalBossanovaRepo = "recurser/bossanova"

const canonicalBossanovaURL = "https://github.com/" + canonicalBossanovaRepo

// checkoutSkillSourceRewriteLockBeforeReclaim keeps the stale-reclaim race
// reproducible without exposing the coordination protocol to callers.
var checkoutSkillSourceRewriteLockBeforeReclaim = func() {}

// checkoutSkillSourceRewriteLockClaimPublished keeps claim publication
// atomicity reproducible without exposing the coordination protocol to callers.
var checkoutSkillSourceRewriteLockClaimPublished = func(string) {}

// checkoutSkillSourceRewriteLockClaimRecoveryAcquired keeps sidecar recovery
// ownership races reproducible without exposing the coordination protocol.
var checkoutSkillSourceRewriteLockClaimRecoveryAcquired = func() {}

// checkoutSkillSourceRewriteLockBeforeRecoveryReclaim makes recovery sidecar
// successor races reproducible without exposing the coordination protocol.
var checkoutSkillSourceRewriteLockBeforeRecoveryReclaim = func() {}

// skillsCmd exposes the non-interactive skill-refresh surface. Unlike the
// interactive startup prompt (maybeInstallSkills), these subcommands never check
// for a TTY and never emit a [Y/n] prompt, so they are safe to call from scripts,
// cron jobs, or a setup hook to close the dogfood loop after a skill edit + build.
func skillsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Manage installed boss skills",
	}

	var syncAgent string
	sync := &cobra.Command{
		Use:   "sync",
		Short: "Refresh installed boss skills from the selected checkout or embedded payload (update-only, no prompt)",
		// Reject positional operands: the agent is selected via --agent, and a bare
		// `boss skills sync codex` must error rather than silently ignore "codex" and
		// mutate every agent's global skill dir on PATH.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runSkillSync(c.OutOrStdout(), skillSyncUpdateOnly, syncAgent)
		},
	}
	sync.Flags().StringVar(&syncAgent, "agent", "", "Restrict to one agent: "+skillInstallAgentChoices()+" (default: all on PATH)")

	var force bool
	var installAgent string
	install := &cobra.Command{
		Use:   "install",
		Short: "Install or refresh boss skills (fresh-installs missing trees); --force reinstalls even when current",
		// Reject positional operands: the agent is selected via --agent, and a bare
		// `boss skills install codex` must error rather than silently ignore "codex"
		// and mutate every agent's global skill dir on PATH.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			mode := skillSyncInstall
			if force {
				mode = skillSyncForce
			}
			return runSkillSync(c.OutOrStdout(), mode, installAgent)
		},
	}
	install.Flags().BoolVar(&force, "force", false, "Reinstall (Extract) unconditionally, even when current")
	install.Flags().StringVar(&installAgent, "agent", "", "Restrict to one agent: "+skillInstallAgentChoices()+" (default: all on PATH)")

	var checkAgent string
	var gate bool
	var heal bool
	check := &cobra.Command{
		Use:   "check",
		Short: "Check installed boss skills against this binary and checkout sources",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if heal && !gate {
				return fmt.Errorf("heal requires the gate option")
			}
			if gate {
				return runSkillGateWithHeal(c.OutOrStdout(), checkAgent, heal)
			}
			return runSkillCheck(c.OutOrStdout(), checkAgent)
		},
	}
	check.Flags().BoolVar(&heal, "heal", false, "With gate, repair unedited lossless or behind drift from authenticated default-branch bytes")
	check.Flags().BoolVar(&gate, "gate", false, "Fail only when installed skills drift from checkout source for paths not edited by this branch")
	check.Flags().StringVar(&checkAgent, "agent", "", "Restrict to one agent: "+skillInstallAgentChoices()+" (default: all on PATH)")

	cmd.AddCommand(sync, install, check)
	return cmd
}

// binarySkillsDriftWarning names the COMPLETE remedy, and neither half is
// optional.
//
// `plugins`: bin/bossd-plugin-claude carries its own mirror of the skill payload
// and runs a guarded refresh of the installed tree at daemon startup. The guard
// holds over a stamped explicit or checkout install, but an unstamped (legacy)
// tree or a provably newer plugin release is still rewritten from that mirror,
// so rebuilding only the CLI leaves the plugin able to restore stale skills.
//
// `./bin/boss`: this warning fires whenever the RUNNING binary's payload is older
// than the source tree, and that binary is often a globally installed one (say
// /opt/homebrew/bin/boss) invoked from inside the checkout. `make build plugins`
// writes the rebuilt CLI to ./bin/boss and leaves the global copy untouched.
// Naming the rebuilt binary explicitly exercises the CLI that was just rebuilt,
// while the checkout source remains the selected install payload from any boss
// binary invoked inside the checkout.
//
// Both commands are anchored at the repository root rather than the cwd, because
// FindSourceRoot searches PARENT directories: the warning fires just as readily
// from services/boss/cmd as from the root, and there a bare `make build plugins`
// would run against the nested directory's Makefile (or none) while `./bin/boss`
// would name a binary that does not exist. Deriving the root from srcRoot makes
// the printed commands correct from anywhere in the checkout.
//
// Must stay a single line — it is printed verbatim to stderr and, indented, to
// `boss skills check`.
func binarySkillsDriftWarning(srcRoot string) string {
	return formatBinarySkillsDriftWarning(
		srcRoot,
		" installed skills were refreshed from source and are current;",
	)
}

// binarySkillsDriftCurrentWarning reports a verified current state without
// claiming that the read-only check command performed the refresh.
func binarySkillsDriftCurrentWarning(srcRoot string) string {
	return formatBinarySkillsDriftWarning(
		srcRoot,
		" installed skills match checkout source and are current;",
	)
}

// binarySkillsDriftCheckWarning keeps the rebuild remedy but makes no claim
// about installed state when `boss skills check` found a stale/absent payload or
// could not inspect one.
func binarySkillsDriftCheckWarning(srcRoot string) string {
	return formatBinarySkillsDriftWarning(srcRoot, "")
}

func formatBinarySkillsDriftWarning(srcRoot, installedStatus string) string {
	root := repoRootFromSourceRoot(srcRoot)
	return fmt.Sprintf(
		"⚠ drift — this running boss binary's embedded skills are behind checkout source %s/skills;"+
			"%s to make the binary shippable,"+
			" rebuild with `make -C %s build plugins`, then run `%s skills install`",
		srcRoot, installedStatus, shellQuote(root), shellQuote(filepath.Join(root, "bin", "boss")),
	)
}

// shellQuote makes an interpolated path safe to paste into a shell. The remedy
// above advertises commands built from a real checkout path, and a checkout under
// e.g. `/home/me/Boss Nova` would otherwise split into multiple arguments, so
// neither command would reach the intended directory or binary.
//
// Quoting is conditional so the overwhelmingly common ordinary path stays
// readable; the safe set is the conservative one used by POSIX shell-quoting
// helpers (shlex.quote), and anything outside it — whitespace, glob characters,
// `$`, backticks, quotes — takes the single-quoted form, where the only character
// needing escaping is a literal single quote.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	const safeExtra = "_@%+=:,./-"
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case strings.ContainsRune(safeExtra, r):
		default:
			return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
		}
	}
	return s
}

// repoRootFromSourceRoot inverts FindSourceRoot's filepath.Join(root,
// SourceRelPath): it climbs one directory per SourceRelPath segment, so it stays
// correct if that constant ever gains or loses a segment, and it uses
// filepath.Dir rather than string trimming so the platform separator is handled.
func repoRootFromSourceRoot(srcRoot string) string {
	root := srcRoot
	for range strings.Split(libskillinstall.SourceRelPath, "/") {
		root = filepath.Dir(root)
	}
	return root
}

// skillSourceStatus compares the running binary's embedded skills with the
// selected checkout snapshot. Reusing payload.fsys keeps the source comparison
// and installed-tree comparison coherent when the checkout changes mid-command.
func skillSourceStatus(payload selectedSkillPayload) (srcRoot string, found, stale bool, err error) {
	if !payload.fromSource {
		return "", false, false, nil
	}
	embedManifest, err := libskillinstall.Manifest(skillinstall.SkillsFS)
	if err != nil {
		return "", false, false, err
	}
	sourceManifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return "", false, false, err
	}
	return payload.srcRoot, true, embedManifest != sourceManifest, nil
}

// warnBinarySkillsDrift emits the non-fatal stale-binary warning. The stronger
// post-refresh wording is reserved for operations that established a current
// installed tree. A missing source tree is normal for consuming repositories.
func warnBinarySkillsDrift(payload selectedSkillPayload, postRefreshCurrent bool, installedManifest string) {
	srcRoot, found, stale, err := skillSourceStatus(payload)
	if err != nil || !found || !stale {
		return
	}
	if postRefreshCurrent {
		currentSourceManifest, manifestErr := libskillinstall.Manifest(payload.fsys)
		if manifestErr != nil || currentSourceManifest != installedManifest {
			postRefreshCurrent = false
		}
	}
	warning := binarySkillsDriftCheckWarning(srcRoot)
	if postRefreshCurrent {
		warning = binarySkillsDriftWarning(srcRoot)
	}
	_, _ = fmt.Fprintln(os.Stderr, warning)
}

// runSkillCheck reports the installed-vs-selected-payload and embed-vs-source state for
// every selected agent. It is read-only and returns an error only after every
// requested report has been written.
func runSkillCheck(out io.Writer, only string) error {
	if err := validateSkillAgentFilter(only); err != nil {
		return err
	}

	// Checks are read-only, so a canonical authenticated checkout is safe to use
	// without the write opt-in required by install and sync commands.
	payload, err := skillCheckPayload()
	if err != nil {
		return fmt.Errorf("select skill payload: %w", err)
	}
	srcRoot, found, binaryStale, err := skillSourceStatus(payload)
	if err != nil {
		return fmt.Errorf("check skill sources: %w", err)
	}
	payloadSource := "binary embed"
	if payload.fromSource {
		payloadSource = fmt.Sprintf("checkout (%s)", payload.srcRoot)
	}
	drift := binaryStale
	reported := false
	var checkErr error
	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		_, pathErr := skillInstallLookPath(target.command)
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			if pathErr != nil {
				continue
			}
			return fmt.Errorf("%s skills dir: %w", target.command, err)
		}
		installed, payloadStale, inspectErr := libskillinstall.InstalledNeedsUpdate(dir, payload.fsys)
		if pathErr != nil && !installed {
			continue
		}
		reported = true
		payloadCurrent := false
		payloadStatus := currentStale(!installed, "up to date", "missing")
		if inspectErr != nil {
			checkErr = errors.Join(checkErr, fmt.Errorf("check %s skills: %w", target.command, inspectErr))
			payloadStatus = fmt.Sprintf("unable to check (%v)", inspectErr)
		} else if installed {
			payloadStatus = currentStale(payloadStale, "up to date", "stale")
			payloadCurrent = !payloadStale
		} else {
			payloadStale = true
		}
		_, _ = fmt.Fprintf(out, "boss skills: %s\n  skills dir: %s\n  installed: %s\n  payload source: %s\n  payload: %s\n", target.command, dir, yesNo(installed), payloadSource, payloadStatus)
		if payloadStale {
			drift = true
			advice, adviceErr := skillCheckRemedyAdvice(payload, dir, installed)
			if adviceErr != nil {
				checkErr = errors.Join(checkErr, fmt.Errorf("check %s skills: %w", target.command, adviceErr))
			}
			_, _ = fmt.Fprintf(out, "  ⚠ drift — installed skills differ from the selected payload; %s\n", advice.inline())
		}
		if !found {
			_, _ = fmt.Fprintln(out, "  sources: not found in this checkout (no skill sources in this checkout)")
			continue
		}
		_, _ = fmt.Fprintf(out, "  sources: %s\n  binary: %s\n", srcRoot, currentStale(binaryStale, "current", "STALE"))
		if binaryStale {
			warning := binarySkillsDriftCheckWarning(srcRoot)
			if payloadCurrent {
				warning = binarySkillsDriftCurrentWarning(srcRoot)
			}
			_, _ = fmt.Fprintln(out, "  "+warning)
		}
	}
	if checkErr != nil {
		return checkErr
	}
	if !reported {
		_, _ = fmt.Fprintln(out, "boss skills: no supported agents found on PATH")
		if !found {
			_, _ = fmt.Fprintln(out, "  sources: not found in this checkout (no skill sources in this checkout)")
		} else {
			_, _ = fmt.Fprintf(out, "  sources: %s\n  binary: %s\n", srcRoot, currentStale(binaryStale, "current", "STALE"))
			if binaryStale {
				_, _ = fmt.Fprintln(out, "  "+binarySkillsDriftCheckWarning(srcRoot))
			}
		}
	}
	if drift {
		return fmt.Errorf("skill drift detected")
	}
	return nil
}

func validateSkillAgentFilter(only string) error {
	if only == "" {
		return nil
	}
	for _, target := range skillInstallAgents {
		if target.command == only {
			return nil
		}
	}
	return fmt.Errorf("unknown agent %q (want %s)", only, skillInstallAgentChoices())
}

// skillInstallAgentChoices renders the --agent values from skillInstallAgents
// ("claude, codex or hermes") so help and errors cannot drift from the table.
func skillInstallAgentChoices() string {
	names := make([]string, 0, len(skillInstallAgents))
	for _, target := range skillInstallAgents {
		names = append(names, target.command)
	}
	return joinOr(names)
}

// joinOr renders names as an English choice list for help and error text:
// "a", "a or b", "a, b or c".
func joinOr(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

func runSkillGate(out io.Writer, only string) error {
	return runSkillGateWithHeal(out, only, false)
}

func runSkillGateWithHeal(out io.Writer, only string, heal bool) error {
	if err := validateSkillAgentFilter(only); err != nil {
		return err
	}
	only = skillGateAgentFilter(only)
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get working directory: %w", err)
	}
	srcRoot, found := libskillinstall.FindSourceRoot(cwd)
	if !found {
		return nil
	}

	payload := selectedSkillPayload{srcRoot: srcRoot, fromSource: true}
	var healRecord libskillinstall.PayloadRecord
	if heal {
		before := skillPayloadRecord(payload, libskillinstall.WriterUnattended)
		frozen, err := snapshotCheckoutSkillFSReadOnly(srcRoot)
		if err != nil {
			return err
		}
		payload.fsys = frozen
		healRecord = skillPayloadRecord(payload, libskillinstall.WriterUnattended)
		if before != healRecord {
			healRecord.Dirty = true
		}
	}
	repoRoot := repoRootFromSourceRoot(srcRoot)
	var gateErr error
	behindHeaderDone := false
	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			return fmt.Errorf("%s skills dir: %w", target.command, err)
		}
		var report libskillinstall.DriftReport
		if heal {
			report, err = healSkillGateTree(out, target.command, dir, payload, healRecord)
		} else {
			report, err = libskillinstall.SourceDriftReport(dir, srcRoot)
		}
		if err != nil {
			gateErr = errors.Join(gateErr, fmt.Errorf("check %s skills: %w", target.command, err))
			continue
		}
		if len(report.Entries) == 0 {
			// A zero exit states its own coverage rather than being a silent
			// green a caller can only read as "everything matched".
			_, _ = fmt.Fprintf(out, "boss skills gate: %s %s — compared %d file(s) · skills dir: %s · payload source: %s\n",
				target.command, currentStale(!report.Installed, "clean", "no skills installed"), report.Compared, dir, srcRoot)
			continue
		}

		selfEdited, unexplained, fallback := classifySkillGateDrift(repoRoot, report.Entries)
		if len(selfEdited) > 0 {
			_, _ = fmt.Fprintf(out, "boss skills gate: %s self-edited drift: %s", target.command, strings.Join(selfEdited, ", "))
			if fallback {
				_, _ = fmt.Fprint(out, " (origin/HEAD unavailable; used git status fallback)")
			}
			_, _ = fmt.Fprintln(out)
		}
		if len(unexplained) > 0 {
			gateErr = errors.Join(gateErr, fmt.Errorf("skill drift detected"))
			if !behindHeaderDone {
				// Once per gate run, flush left so no consumer reads it as a row:
				// a stale checkout judges every direction against an old HEAD.
				behindHeaderDone = true
				if n := skillCheckoutBehindCount(repoRoot); n > 0 {
					_, _ = fmt.Fprintf(out, "boss skills gate: this checkout is %d commit(s) behind origin/HEAD — directions are judged against HEAD\n", n)
				}
			}
			// The count is this agent tree's own, stated as such: the gate
			// prints one section per tree, so a count taken over the whole
			// output is a multiple of any one tree's.
			_, _ = fmt.Fprintf(out, "boss skills gate: %s skill drift detected — %d path(s) in this agent tree", target.command, len(unexplained))
			if fallback {
				_, _ = fmt.Fprint(out, " (origin/HEAD unavailable; used git status fallback)")
			}
			_, _ = fmt.Fprintln(out)
			entries := skillDriftEntriesWithDirection(repoRoot, dir, unexplained)
			for _, entry := range entries {
				_, _ = fmt.Fprintf(out, "  - %s\n", entry.label())
			}
			advice := composeSkillInstallRemedy(payload, entries, skillCheckoutHasAncestry(repoRoot), true)
			for _, line := range advice.lines("  ") {
				_, _ = fmt.Fprintln(out, line)
			}
		}
	}
	return gateErr
}

func skillGateAgentFilter(only string) string {
	if only != "" {
		return only
	}
	skillsHome := os.Getenv("BOSS_SKILLS_HOME")
	if skillsHome == "" {
		return ""
	}
	skillsHome = filepath.Clean(skillsHome)
	// An exact match against each agent's resolved directory recognises a
	// tree at a custom location (e.g. $HERMES_HOME/skills).
	for _, target := range skillInstallAgents {
		if dir, err := libskillinstall.DirForAgent(target.agent); err == nil && filepath.Clean(dir) == skillsHome {
			return target.command
		}
	}
	// Otherwise fall back to the conventional ~/.<agent>/skills layout, which
	// still names the agent for a tree under another user's home.
	parent := filepath.Base(filepath.Dir(skillsHome))
	for _, target := range skillInstallAgents {
		if parent == "."+target.command {
			return target.command
		}
	}
	return ""
}

// classifySkillGateDrift splits drift entries into those explained by an edit
// to this checkout's own skill sources and those that are not. Entries stay
// whole so their kind reaches the direction lookup instead of being shredded
// into a lookup table that answers "" for a miss.
func classifySkillGateDrift(repoRoot string, entries []libskillinstall.DriftEntry) (selfEdited []string, unexplained []libskillinstall.DriftEntry, usedFallback bool) {
	branchBase, baseErr := checkoutGitOutput(repoRoot, "merge-base", "HEAD", "origin/HEAD")
	if baseErr != nil || strings.TrimSpace(branchBase) == "" {
		usedFallback = true
		branchBase = ""
	}
	for _, entry := range entries {
		rel := entry.Path
		sourcePath := filepath.Join(libskillinstall.SourceRelPath, "skills", filepath.FromSlash(strings.TrimSuffix(rel, "/")))
		diff := ""
		var err error
		if branchBase != "" {
			diff, err = checkoutGitOutput(repoRoot, "diff", "--name-only", branchBase, "--", sourcePath)
			if err != nil {
				usedFallback = true
				diff = ""
			}
		}
		status, statusErr := checkoutGitOutput(repoRoot, "status", "--porcelain", "--", sourcePath)
		if statusErr == nil && strings.TrimSpace(status) != "" {
			diff = status
			err = nil
		}
		if err == nil && strings.TrimSpace(diff) != "" {
			selfEdited = append(selfEdited, rel)
			continue
		}
		unexplained = append(unexplained, entry)
	}
	return selfEdited, unexplained, usedFallback
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func currentStale(stale bool, current, outdated string) string {
	if stale {
		return outdated
	}
	return current
}

// skillInstallRemedy is the reinstall command every drift surface prints. For a
// from-source payload it builds first, the same `make -C <root> build plugins`
// step formatBinarySkillsDriftWarning names: bin/ is gitignored, so a freshly
// provisioned worktree has no bin/boss to invoke, and rebuilding the plugins in
// the same step keeps a stale plugin embed from restoring the old payload.
func skillInstallRemedy(payload selectedSkillPayload) string {
	if payload.fromSource {
		root := repoRootFromSourceRoot(payload.srcRoot)
		return "make -C " + shellQuote(root) + " build plugins && " +
			trustCheckoutSkillSourcesEnv + "=1 " + shellQuote(filepath.Join(root, "bin", "boss")) + " skills install"
	}
	return "boss skills install"
}

// skillDriftDirection is how the installed bytes at a drifted path relate to
// this checkout's recorded history of the matching source file. It decides
// whether a reinstall of that path is a lossless forward fix.
type skillDriftDirection string

const (
	// skillDriftLossless replaces no installed content, so no history is needed.
	skillDriftLossless skillDriftDirection = "lossless"
	// skillDriftBehind is a reachable earlier revision of the source path.
	skillDriftBehind skillDriftDirection = "behind"
	// skillDriftUnrecoverable is content no reachable revision of the source
	// path contains, so a reinstall would replace bytes this checkout cannot
	// restore.
	skillDriftUnrecoverable skillDriftDirection = "unrecoverable"
	// skillDriftAhead is a revision origin/HEAD reaches and this checkout's HEAD
	// does not: the installed copy is newer than the checkout, so a reinstall
	// from here would downgrade it.
	skillDriftAhead skillDriftDirection = "ahead"
	// skillDriftUnknown is an undecided lookup: git failed, the object is not
	// present locally, or the bounded lookback ended without a match. Treated as
	// unrecoverable for the remedy decision, but labelled honestly.
	skillDriftUnknown skillDriftDirection = "unknown"
)

// skillDriftHistoryLookback bounds the per-path history walk so a pathological
// history cannot turn a preflight into a stall, at the price of an "unknown"
// verdict that fails safe toward not destroying.
const skillDriftHistoryLookback = 500

// skillDriftHistoryGit reads a drifted path's recorded history. It is a seam so
// a test can make the query fail without removing git from the machine.
var skillDriftHistoryGit = checkoutGitOutput

// skillDriftEntry pairs a library drift kind with the direction resolved here.
// Direction is a property of the checkout, so the library stays repository
// unaware and this command owns the git seam.
type skillDriftEntry struct {
	path      string
	kind      libskillinstall.DriftKind
	direction skillDriftDirection
}

func (e skillDriftEntry) label() string {
	return fmt.Sprintf("%s (%s, %s)", e.path, e.kind, e.direction)
}

// skillCheckoutHasAncestry reports whether this checkout has any history to
// consult. A repository without a resolvable HEAD cannot answer the direction
// question at all, which is distinct from answering it and not deciding.
func skillCheckoutHasAncestry(repoRoot string) bool {
	head, err := skillDriftHistoryGit(repoRoot, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil && strings.TrimSpace(head) != ""
}

// resolveSkillDriftDirection reports how one drifted path's installed copy
// relates to this checkout's history of the matching source file.
//
// Kinds that replace no installed content are lossless without a git call: a
// reinstall that adds an absent file, restores an executable bit onto bytes
// that already match the payload, or relinks a top-level symlink destroys
// nothing.
func resolveSkillDriftDirection(repoRoot, dir, rel string, kind libskillinstall.DriftKind) skillDriftDirection {
	switch kind {
	case libskillinstall.DriftAbsent, libskillinstall.DriftMode:
		return skillDriftLossless
	case libskillinstall.DriftBrokenSymlink:
		return skillTopLevelEntryDirection(dir, rel)
	case libskillinstall.DriftContent, libskillinstall.DriftUnexpected:
		// Decided from this checkout's recorded history, below.
	}
	if strings.HasSuffix(rel, "/") {
		// A stale installed skill directory. The reinstall removes a tree this
		// checkout has no source path for, so no forward revision of it exists.
		return skillDriftUnrecoverable
	}
	if isTopLevelSkillKey(rel) {
		// The library keys a leftover top-level entry on its bare skill name,
		// so it lives at <dir>/<name> rather than under the namespace. Hashing
		// the namespaced path instead would always fail and label the entry
		// unknown, where its namespaced sibling "<name>/" is correctly called
		// unrecoverable — one removed skill, two directions.
		return skillTopLevelEntryDirection(dir, rel)
	}
	installedPath := filepath.Join(dir, libskillinstall.Namespace, filepath.FromSlash(rel))
	sourcePath := filepath.Join(libskillinstall.SourceRelPath, "skills", filepath.FromSlash(rel))
	return resolveSkillDriftAncestry(repoRoot, installedPath, sourcePath)
}

// isTopLevelSkillKey reports whether a drift path is the library's bare
// top-level skill name key space — an entry directly in the agent skills dir —
// rather than a payload-relative path under the namespace. Namespaced skill
// directories carry a trailing slash and namespaced files carry their skill
// directory, so only a bare boss skill name lands here.
func isTopLevelSkillKey(rel string) bool {
	if strings.Contains(rel, "/") {
		return false
	}
	return rel == "boss" || strings.HasPrefix(rel, "boss-")
}

// skillTopLevelEntryDirection decides a drifted entry that lives directly in
// the agent skills dir rather than under the namespace.
//
// extract() os.RemoveAll's every top-level boss-* entry — its own comment says
// "(symlinks or real directories)" — so a reinstall is lossless only when there
// is nothing there or the entry is genuinely a symlink it merely relinks. A
// real directory at <dir>/<skill> is the legacy pre-namespacing layout: a tree
// the reinstall deletes and this checkout has no source path to reproduce.
func skillTopLevelEntryDirection(dir, rel string) skillDriftDirection {
	info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(strings.TrimSuffix(rel, "/"))))
	switch {
	case err != nil && os.IsNotExist(err):
		return skillDriftLossless
	case err != nil:
		return skillDriftUnknown
	case info.Mode()&os.ModeSymlink != 0:
		return skillDriftLossless
	default:
		return skillDriftUnrecoverable
	}
}

// resolveSkillDriftAncestry hashes the installed file as a git blob and asks
// whether that blob is the content of sourcePath at any commit reachable from
// HEAD, in one bounded git log per path rather than one per commit.
//
// A blob HEAD never reached may still be one origin/HEAD reaches: the checkout
// itself is stale and the installed copy is newer. That second walk runs only
// for an otherwise-unrecoverable answer, and any failure in it (origin/HEAD
// unresolvable, git erroring) leaves the answer unrecoverable — the stricter
// refusal is the floor, never weakened by a lookup that did not complete.
func resolveSkillDriftAncestry(repoRoot, installedPath, sourcePath string) skillDriftDirection {
	blob, err := skillDriftHistoryGit(repoRoot, "hash-object", "--", installedPath)
	if err != nil || blob == "" {
		return skillDriftUnknown
	}
	return resolveSkillDriftBlobAncestry(repoRoot, blob, sourcePath)
}

func resolveSkillDriftBlobAncestry(repoRoot, blob, sourcePath string) skillDriftDirection {
	found, commits, err := skillDriftHistoryHasBlob(repoRoot, blob, "HEAD", sourcePath)
	if err != nil {
		return skillDriftUnknown
	}
	if found {
		return skillDriftBehind
	}
	if commits >= skillDriftHistoryLookback {
		return skillDriftUnknown
	}
	if ahead, _, err := skillDriftHistoryHasBlob(repoRoot, blob, "HEAD..origin/HEAD", sourcePath); err == nil && ahead {
		return skillDriftAhead
	}
	return skillDriftUnrecoverable
}

// skillDriftHistoryHasBlob walks sourcePath's history over revRange, bounded by
// skillDriftHistoryLookback, and reports whether blob is any revision of it
// along with how many commits the walk visited.
func skillDriftHistoryHasBlob(repoRoot, blob, revRange, sourcePath string) (bool, int, error) {
	out, err := skillDriftHistoryGit(repoRoot, "log", "--format=%H", "--raw", "--no-renames",
		"--abbrev=40", fmt.Sprintf("-n%d", skillDriftHistoryLookback), revRange, "--", sourcePath)
	if err != nil {
		return false, 0, err
	}
	commits := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ":") {
			commits++
			continue
		}
		// ":<oldmode> <newmode> <oldblob> <newblob> <status>\t<path>"
		fields := strings.Fields(strings.ReplaceAll(line, "\t", " "))
		if len(fields) < 4 {
			continue
		}
		for _, candidate := range fields[2:4] {
			if candidate == blob {
				return true, commits, nil
			}
		}
	}
	return false, commits, nil
}

// skillCheckoutBehindCount is how many commits this checkout's HEAD lacks from
// origin/HEAD, or 0 when that cannot be read. It is informational only.
func skillCheckoutBehindCount(repoRoot string) int {
	out, err := checkoutGitOutput(repoRoot, "rev-list", "--count", "HEAD..origin/HEAD")
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func skillDriftEntriesWithDirection(repoRoot, dir string, entries []libskillinstall.DriftEntry) []skillDriftEntry {
	resolved := make([]skillDriftEntry, 0, len(entries))
	for _, entry := range entries {
		resolved = append(resolved, skillDriftEntry{
			path:      entry.Path,
			kind:      entry.Kind,
			direction: resolveSkillDriftDirection(repoRoot, dir, entry.Path, entry.Kind),
		})
	}
	return resolved
}

const skillRemedyUnverifiedNote = "reinstall direction unverified: no checkout history to compare the installed copy against"

const skillRemedyWithheldLead = "reinstall withheld [lost] — it would overwrite installed content this checkout cannot restore:"

// skillRemedyDowngradeLead is the withheld lead when every withheld path is
// newer than this checkout rather than lost to it: the content is restorable
// from origin/HEAD, but a reinstall from here would replace it with an older
// revision. It keeps the "reinstall withheld" prefix consumers key on.
const skillRemedyDowngradeLead = "reinstall withheld [downgrade] — the installed copy is newer than this checkout (it is on origin/HEAD); a reinstall from here would downgrade it:"

// skillRemedyUndecidedLead is the withheld lead for a safety decision that
// could not be computed at all. Failing to read the installed tree while
// deciding whether a reinstall destroys it is not evidence that it does not,
// so it withholds in the same direction as a known loss.
const skillRemedyUndecidedLead = "reinstall withheld [undecided] — the installed-vs-checkout comparison failed, so whether a reinstall would overwrite unrecoverable content is undecided"

// skillRemedyNextActionDestroys and skillRemedyNextActionUndecided are the
// operator's way out of a refusal. Every other outcome in this command family
// ends in a runnable command; a withheld remedy fires at a preflight that has
// just failed an unattended build, so it must not end in a dead end either.
// Neither names a command, because nothing a shell would run may be emitted on
// this path.
const skillRemedyNextActionDestroys = "next: move the listed paths out of the skills directory (or delete them if they are disposable); with nothing unrecoverable left, the reinstall command is offered again"

const skillRemedyNextActionUndecided = "next: re-run this check once the skills directory is readable and no longer changing underneath it"

const skillRemedyNextActionDowngrade = "next: update this checkout to origin/HEAD, then re-run this check"

// skillRemedyAdvice is a composed reinstall prescription: either a runnable
// command or an explicit refusal naming what the command would have destroyed.
type skillRemedyAdvice struct {
	command       string
	unverified    bool
	refusal       skillRemedyRefusal
	withheldPaths []skillDriftEntry
}

type skillRemedyRefusal int

const (
	skillRefusalNone skillRemedyRefusal = iota
	skillRefusalLost
	skillRefusalDowngrade
	skillRefusalUndecided
)

func (r skillRemedyRefusal) String() string {
	switch r {
	case skillRefusalNone:
		return "none"
	case skillRefusalLost:
		return "lost"
	case skillRefusalDowngrade:
		return "downgrade"
	case skillRefusalUndecided:
		return "undecided"
	default:
		return "undecided"
	}
}

func (d skillDriftDirection) refusal() skillRemedyRefusal {
	switch d {
	case skillDriftLossless, skillDriftBehind:
		return skillRefusalNone
	case skillDriftAhead:
		return skillRefusalDowngrade
	case skillDriftUnrecoverable, skillDriftUnknown:
		return skillRefusalLost
	default:
		return skillRefusalLost
	}
}

func (a skillRemedyAdvice) withheld() bool {
	return a.refusal != skillRefusalNone
}

// withheldLead is the headline for a refusal, naming why it refused.
func (a skillRemedyAdvice) withheldLead() string {
	switch a.refusal {
	case skillRefusalNone:
		return ""
	case skillRefusalLost:
		return skillRemedyWithheldLead
	case skillRefusalDowngrade:
		return skillRemedyDowngradeLead
	case skillRefusalUndecided:
		return skillRemedyUndecidedLead
	default:
		return skillRemedyUndecidedLead
	}
}

// nextAction is what the operator does about a refusal.
func (a skillRemedyAdvice) nextAction() string {
	switch a.refusal {
	case skillRefusalNone:
		return ""
	case skillRefusalLost:
		return skillRemedyNextActionDestroys
	case skillRefusalDowngrade:
		return skillRemedyNextActionDowngrade
	case skillRefusalUndecided:
		return skillRemedyNextActionUndecided
	default:
		return skillRemedyNextActionUndecided
	}
}

// composeSkillInstallRemedy is the single decision point every drift surface
// uses, so the surfaces cannot drift apart in their safety behaviour.
//
// overwrites reports whether an installed tree exists for the reinstall to
// write over; ancestry reports whether this checkout has history to consult.
// Without ancestry the direction is stated as unverified rather than silently
// assumed correct; with it, any path that is not a lossless forward fix
// withholds the command entirely.
func composeSkillInstallRemedy(payload selectedSkillPayload, entries []skillDriftEntry, ancestry, overwrites bool) skillRemedyAdvice {
	command := skillInstallRemedy(payload)
	if !overwrites {
		return skillRemedyAdvice{command: command}
	}
	if !ancestry {
		return skillRemedyAdvice{command: command, unverified: true}
	}
	advice := skillRemedyAdvice{command: command}
	for _, entry := range entries {
		refusal := entry.direction.refusal()
		if refusal == skillRefusalNone {
			continue
		}
		advice.withheldPaths = append(advice.withheldPaths, entry)
		// A genuine loss anywhere keeps the destructive refusal, regardless of order.
		if advice.refusal == skillRefusalNone || refusal == skillRefusalLost {
			advice.refusal = refusal
		}
	}
	return advice
}

func (a skillRemedyAdvice) runLine() string {
	line := "run `" + a.command + "`"
	if a.unverified {
		line += " (" + skillRemedyUnverifiedNote + ")"
	}
	return line
}

// inline renders the advice as a clause appended to a one-line drift warning.
func (a skillRemedyAdvice) inline() string {
	if !a.withheld() {
		return a.runLine()
	}
	if len(a.withheldPaths) == 0 {
		return a.withheldLead() + "; " + a.nextAction()
	}
	paths := make([]string, 0, len(a.withheldPaths))
	for _, entry := range a.withheldPaths {
		paths = append(paths, entry.label())
	}
	return a.withheldLead() + " " + strings.Join(paths, ", ") + "; " + a.nextAction()
}

// lines renders the advice as indented report lines under a drift verdict.
func (a skillRemedyAdvice) lines(indent string) []string {
	if !a.withheld() {
		return []string{indent + a.runLine()}
	}
	out := make([]string, 0, len(a.withheldPaths)+2)
	out = append(out, indent+a.withheldLead())
	for _, entry := range a.withheldPaths {
		out = append(out, indent+"  "+entry.label())
	}
	return append(out, indent+a.nextAction())
}

// skillCheckRemedyAdvice resolves the same remedy decision the gate makes, from
// whatever evidence `boss skills check` has. Comparing against the binary's
// embedded payload leaves no ancestry to consult, so that surface reports the
// direction as unverified instead of asserting the reinstall is correct.
func skillCheckRemedyAdvice(payload selectedSkillPayload, dir string, installed bool) (skillRemedyAdvice, error) {
	if !installed {
		return composeSkillInstallRemedy(payload, nil, false, false), nil
	}
	if !payload.fromSource {
		return composeSkillInstallRemedy(payload, nil, false, true), nil
	}
	repoRoot := repoRootFromSourceRoot(payload.srcRoot)
	if !skillCheckoutHasAncestry(repoRoot) {
		return composeSkillInstallRemedy(payload, nil, false, true), nil
	}
	report, err := libskillinstall.SourceDriftReport(dir, payload.srcRoot)
	if err != nil {
		// The failure happened while computing the safety decision, so it is
		// neither the no-ancestry case nor a clean one: surface it and refuse.
		return skillRemedyAdvice{refusal: skillRefusalUndecided}, fmt.Errorf("resolve reinstall safety: %w", err)
	}
	if len(report.Entries) == 0 {
		// The installed tree is stale against the payload yet matches the
		// checkout source, so it moved under us. Nothing enumerable means
		// nothing decided, not that the reinstall is safe.
		return skillRemedyAdvice{refusal: skillRefusalUndecided}, nil
	}
	return composeSkillInstallRemedy(payload, skillDriftEntriesWithDirection(repoRoot, dir, report.Entries), true, true), nil
}

// runSkillSync writes the embedded skill payload into the global skill dir of each
// agent found on PATH, per mode:
//
//   - skillSyncUpdateOnly (sync): EnsureUpdated — refresh a stale tree, no-op when
//     current, and never fresh-install an empty dir (points the caller at `install`).
//   - skillSyncInstall (install): additionally Extract into a not-yet-installed dir,
//     so it works as first-time setup.
//   - skillSyncForce (install --force): Extract unconditionally.
//
// A single agent may be selected via only ("claude"/"codex"/"hermes"); when set, an
// unresolvable/failed agent makes the command exit non-zero, whereas in the
// all-agents mode a per-agent failure is a non-fatal warning. Successful extracts
// record the installed manifest so a later interactive boss does not re-prompt.
func runSkillSync(out io.Writer, mode skillSyncMode, only string) error {
	if err := validateSkillAgentFilter(only); err != nil {
		return err
	}

	payload, err := skillPayload()
	if err != nil {
		return fmt.Errorf("select skill payload: %w", err)
	}
	manifest, err := libskillinstall.Manifest(payload.fsys)
	if err != nil {
		return fmt.Errorf("compute skill manifest: %w", err)
	}

	// A malformed/unreadable settings.json makes config.Load return default
	// settings alongside an error. Saving those defaults would silently replace
	// the user's real file, so on a load error we still install/refresh the skill
	// trees (the point of the command) but skip all manifest bookkeeping and the
	// save. loadErr gates every recordSkillInstall below.
	settings, loadErr := config.Load()
	if loadErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "Warning: skipping skill-manifest bookkeeping; failed to load settings: %v\n", loadErr)
	}
	settingsChanged := false
	var targetErr error
	refreshed := false
	installedTargetsCurrent := true
	// Every mode here is an explicit operator command, so it keeps its
	// overwrite contract and stamps an explicit payload record.
	record := skillPayloadRecord(payload, libskillinstall.WriterExplicit)

	recordErr := func(err error) {
		if only != "" {
			targetErr = err
		}
	}
	extract := func(target skillInstallAgent, dir, verb string) {
		if err := libskillinstall.ExtractRecorded(dir, payload.fsys, record); err != nil {
			installedTargetsCurrent = false
			_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to %s %s skills: %v\n", verb, target.command, err)
			recordErr(err)
			return
		}
		refreshed = true
		_, _ = fmt.Fprintf(out, "boss skills: %s %s (%s)\n", verb, target.command, dir)
		if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
			settingsChanged = true
		}
	}

	for _, target := range skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		if _, err := skillInstallLookPath(target.command); err != nil {
			if dir, dirErr := libskillinstall.DirForAgent(target.agent); dirErr == nil && libskillinstall.IsInstalled(dir) {
				installedTargetsCurrent = false
			}
			_, _ = fmt.Fprintf(out, "boss skills: %s not found on PATH, skipping\n", target.command)
			recordErr(fmt.Errorf("%s not found on PATH", target.command))
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			installedTargetsCurrent = false
			_, _ = fmt.Fprintf(os.Stderr, "Warning: %s skills dir: %v\n", target.command, err)
			recordErr(err)
			continue
		}

		switch {
		case mode == skillSyncForce:
			extract(target, dir, "reinstalled")
		case mode == skillSyncInstall && !libskillinstall.IsInstalled(dir):
			// install doubles as first-time setup: fresh-install an empty dir.
			extract(target, dir, "installed")
		default:
			// Update-only: refresh a stale tree, no-op when current. sync never
			// fresh-installs (falls through to the not-installed hint below).
			updated, err := libskillinstall.EnsureUpdatedRecorded(dir, payload.fsys, record)
			if err != nil {
				installedTargetsCurrent = false
				_, _ = fmt.Fprintf(os.Stderr, "Warning: failed to update %s skills: %v\n", target.command, err)
				recordErr(err)
				continue
			}
			switch {
			case updated:
				refreshed = true
				_, _ = fmt.Fprintf(out, "boss skills: updated %s (%s)\n", target.command, dir)
				if loadErr == nil && recordSkillInstall(&settings, target.agent, manifest) {
					settingsChanged = true
				}
			case !libskillinstall.IsInstalled(dir):
				// sync is update-only, so "up to date" would be misleading for an
				// empty dir. Point the caller at the fresh-install path instead.
				_, _ = fmt.Fprintf(out, "boss skills: %s not installed (%s); run 'boss skills install' to install\n", target.command, dir)
			default:
				_, _ = fmt.Fprintf(out, "boss skills: %s up to date (%s)\n", target.command, dir)
			}
		}
	}

	if settingsChanged {
		_ = config.Save(settings)
	}
	warnBinarySkillsDrift(payload, refreshed && installedTargetsCurrent, manifest)
	return targetErr
}
