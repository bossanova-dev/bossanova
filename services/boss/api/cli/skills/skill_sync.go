package skills

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

	libskillinstall "github.com/recurser/bossalib/contract/plugin/skillinstall"
	"github.com/recurser/bossalib/contract/plugin/skillpayload"
	"github.com/recurser/bossalib/kit/config"
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
	skillSyncSetAside
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

func (g *Group) skillPayload() (selectedSkillPayload, error) {
	return g.skillPayloadWithCheckoutTrust(os.Getenv(trustCheckoutSkillSourcesEnv) == "1")
}

// startupSkillPayload selects a checkout only after the operator explicitly
// trusts its bytes. This prevents a canonical-looking origin from authorizing a
// local PR or dirty worktree to overwrite globally installed agent instructions.
func (g *Group) startupSkillPayload() (selectedSkillPayload, error) {
	return g.skillPayloadWithCheckoutTrust(os.Getenv(trustCheckoutSkillSourcesEnv) == "1")
}

func (g *Group) skillPayloadWithCheckoutTrust(trustCheckout bool) (selectedSkillPayload, error) {
	return g.skillPayloadWithCheckoutTrustMode(trustCheckout, false)
}

// skillCheckPayload selects a checkout payload without creating any source-side
// coordination files, so `boss skills check` remains safe on read-only checkouts.
func (g *Group) skillCheckPayload() (selectedSkillPayload, error) {
	return g.skillPayloadWithCheckoutTrustMode(true, true)
}

func (g *Group) skillPayloadWithCheckoutTrustMode(trustCheckout, readOnly bool) (selectedSkillPayload, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return selectedSkillPayload{}, fmt.Errorf("get working directory: %w", err)
	}
	if root, ok := libskillinstall.FindSourceRoot(cwd); ok && trustCheckout {
		trusted, err := TrustedSkillSourceRoot(root)
		if err != nil {
			return selectedSkillPayload{}, fmt.Errorf("authenticate checkout skill source: %w", err)
		}
		if trusted {
			snapshot := g.snapshotCheckoutSkillFS
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
	return selectedSkillPayload{fsys: skillpayload.SkillsFS}, nil
}

const canonicalBossanovaRepo = "recurser/bossanova"

const canonicalBossanovaURL = "https://github.com/" + canonicalBossanovaRepo

// checkoutSkillSourceRewriteLockBeforeReclaim keeps the stale-reclaim race
// reproducible without exposing the coordination protocol to callers.

// checkoutSkillSourceRewriteLockClaimPublished keeps claim publication
// atomicity reproducible without exposing the coordination protocol to callers.

// checkoutSkillSourceRewriteLockClaimRecoveryAcquired keeps sidecar recovery
// ownership races reproducible without exposing the coordination protocol.

// checkoutSkillSourceRewriteLockBeforeRecoveryReclaim makes recovery sidecar
// successor races reproducible without exposing the coordination protocol.

// skillsCmd exposes the non-interactive skill-refresh surface. Unlike the
// interactive startup prompt (maybeInstallSkills), these subcommands never check
// for a TTY and never emit a [Y/n] prompt, so they are safe to call from scripts,
// cron jobs, or a setup hook to close the dogfood loop after a skill edit + build.
func (g *Group) skillsCmd() *cobra.Command {
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
			return g.runSkillSync(c.OutOrStdout(), skillSyncUpdateOnly, syncAgent)
		},
	}
	sync.Flags().StringVar(&syncAgent, "agent", "", "Restrict to one agent: "+g.skillInstallAgentChoices()+" (default: all on PATH)")

	var force, setAside bool
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
			if setAside {
				mode = skillSyncSetAside
			}
			return g.runSkillSync(c.OutOrStdout(), mode, installAgent)
		},
	}
	install.Flags().BoolVar(&setAside, "set-aside", false, "Preserve installed drift in skill-backups before reinstalling (implies force)")
	install.Flags().BoolVar(&force, "force", false, "Reinstall (Extract) unconditionally, even when current")
	install.Flags().StringVar(&installAgent, "agent", "", "Restrict to one agent: "+g.skillInstallAgentChoices()+" (default: all on PATH)")

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
				return g.runSkillGateWithHeal(c.OutOrStdout(), checkAgent, heal)
			}
			return g.runSkillCheck(c.OutOrStdout(), checkAgent)
		},
	}
	check.Flags().BoolVar(&heal, "heal", false, "With gate, repair unedited lossless or behind drift from authenticated default-branch bytes")
	check.Flags().BoolVar(&gate, "gate", false, "Fail only when installed skills drift from checkout source for paths not edited by this branch")
	check.Flags().StringVar(&checkAgent, "agent", "", "Restrict to one agent: "+g.skillInstallAgentChoices()+" (default: all on PATH)")

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
	embedManifest, err := libskillinstall.Manifest(skillpayload.SkillsFS)
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
func (g *Group) runSkillCheck(out io.Writer, only string) error {
	if err := g.validateSkillAgentFilter(only); err != nil {
		return err
	}

	// Checks are read-only, so a canonical authenticated checkout is safe to use
	// without the write opt-in required by install and sync commands.
	payload, err := g.skillCheckPayload()
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
	for _, target := range g.skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		_, pathErr := g.StartupLookPath(target.command)
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
		g.reportSkillCheckHold(out, target, dir, installed)

		if payloadStale {
			drift = true
			advice, adviceErr := g.skillCheckRemedyAdvice(payload, dir, installed, target.command)
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

func (g *Group) validateSkillAgentFilter(only string) error {
	if only == "" {
		return nil
	}
	for _, target := range g.skillInstallAgents {
		if target.command == only {
			return nil
		}
	}
	return fmt.Errorf("unknown agent %q (want %s)", only, g.skillInstallAgentChoices())
}

// skillInstallAgentChoices renders the --agent values from skillInstallAgents
// ("claude, codex or hermes") so help and errors cannot drift from the table.
func (g *Group) skillInstallAgentChoices() string {
	names := make([]string, 0, len(g.skillInstallAgents))
	for _, target := range g.skillInstallAgents {
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

func (g *Group) runSkillGate(out io.Writer, only string) error {
	return g.runSkillGateWithHeal(out, only, false)
}

func (g *Group) runSkillGateWithHeal(out io.Writer, only string, heal bool) error {
	if err := g.validateSkillAgentFilter(only); err != nil {
		return err
	}
	only = g.skillGateAgentFilter(only)
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
		before := g.skillPayloadRecord(payload, libskillinstall.WriterUnattended)
		frozen, err := snapshotCheckoutSkillFSReadOnly(srcRoot)
		if err != nil {
			return err
		}
		payload.fsys = frozen
		healRecord = g.skillPayloadRecord(payload, libskillinstall.WriterUnattended)
		if before != healRecord {
			healRecord.Dirty = true
		}
	}
	repoRoot := repoRootFromSourceRoot(srcRoot)
	var tip skillOriginTip
	if _, _, fallback := classifySkillGateDrift(repoRoot, nil); !fallback {
		if loaded, ok := g.loadSkillOriginTip(repoRoot); ok {
			tip = loaded
		}
	}
	var gateErr error
	behindHeaderDone := false
	for _, target := range g.skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		dir, err := libskillinstall.DirForAgent(target.agent)
		if err != nil {
			return fmt.Errorf("%s skills dir: %w", target.command, err)
		}
		var report libskillinstall.DriftReport
		if heal {
			report, err = g.healSkillGateTree(out, target.command, dir, payload, healRecord, tip)
		} else {
			report, err = libskillinstall.SourceDriftReport(dir, srcRoot)
		}
		if err != nil {
			gateErr = errors.Join(gateErr, fmt.Errorf("check %s skills: %w", target.command, err))
			continue
		}
		gateErr = errors.Join(gateErr, g.renderSkillGateTree(out, target.command, dir, repoRoot, payload, report, tip, &behindHeaderDone))
	}
	return gateErr
}

func (g *Group) skillGateAgentFilter(only string) string {
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
	for _, target := range g.skillInstallAgents {
		if dir, err := libskillinstall.DirForAgent(target.agent); err == nil && filepath.Clean(dir) == skillsHome {
			return target.command
		}
	}
	// Otherwise fall back to the conventional ~/.<agent>/skills layout, which
	// still names the agent for a tree under another user's home.
	parent := filepath.Base(filepath.Dir(skillsHome))
	for _, target := range g.skillInstallAgents {
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
// this checkout's recorded skill history. It decides
// whether a reinstall of that path is a lossless forward fix.
type skillDriftDirection string

const (
	// skillDriftLossless replaces no installed content, so no history is needed.
	skillDriftLossless skillDriftDirection = "lossless"
	// skillDriftBehind is a reachable revision in skill history.
	skillDriftBehind skillDriftDirection = "behind"
	// skillDriftUnrecoverable is content no reachable skill revision contains,
	// so a reinstall would replace bytes this checkout cannot restore.
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
func (g *Group) skillCheckoutHasAncestry(repoRoot string) bool {
	head, err := g.skillDriftHistoryGit(repoRoot, "rev-parse", "--verify", "--quiet", "HEAD")
	return err == nil && strings.TrimSpace(head) != ""
}

// resolveSkillDriftDirection reports how one drifted path's installed copy
// relates to this checkout's history of the matching source file.
//
// Kinds that replace no installed content are lossless without a git call: a
// reinstall that adds an absent file, restores an executable bit onto bytes
// that already match the payload, or relinks a top-level symlink destroys
// nothing.
func (g *Group) resolveSkillDriftDirection(repoRoot, dir, rel string, kind libskillinstall.DriftKind, history *skillDriftHistory) skillDriftDirection {
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
	return g.resolveSkillDriftAncestry(repoRoot, installedPath, history)
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
// whether that blob is recorded in reachable skill history, sharing a bounded
// walk across every entry in the batch.
//
// A blob HEAD never reached may still be one origin/HEAD reaches: the checkout
// itself is stale and the installed copy is newer. That second walk runs only
// for an otherwise-unrecoverable answer, and any failure in it (origin/HEAD
// unresolvable, git erroring) leaves the answer unrecoverable — the stricter
// refusal is the floor, never weakened by a lookup that did not complete.
func (g *Group) resolveSkillDriftAncestry(repoRoot, installedPath string, history *skillDriftHistory) skillDriftDirection {
	blob, err := g.skillDriftHistoryGit(repoRoot, "hash-object", "--", installedPath)
	if err != nil || blob == "" {
		return skillDriftUnknown
	}
	return g.resolveSkillDriftBlobAncestry(repoRoot, blob, history)
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

func (g *Group) skillDriftEntriesWithDirection(repoRoot, dir string, entries []libskillinstall.DriftEntry) []skillDriftEntry {
	history := &skillDriftHistory{}
	resolved := make([]skillDriftEntry, 0, len(entries))
	for _, entry := range entries {
		resolved = append(resolved, skillDriftEntry{
			path:      entry.Path,
			kind:      entry.Kind,
			direction: g.resolveSkillDriftDirection(repoRoot, dir, entry.Path, entry.Kind, history),
		})
	}
	return resolved
}

// skillCheckRemedyAdvice resolves the same remedy decision the gate makes, from
// whatever evidence `boss skills check` has. Comparing against the binary's
// embedded payload leaves no ancestry to consult, so that surface reports the
// direction as unverified instead of asserting the reinstall is correct.
func (g *Group) skillCheckRemedyAdvice(payload selectedSkillPayload, dir string, installed bool, agent ...string) (skillRemedyAdvice, error) {
	if !installed {
		return composeSkillInstallRemedy(payload, nil, false, false, agent...), nil
	}
	if !payload.fromSource {
		return composeSkillInstallRemedy(payload, nil, false, true, agent...), nil
	}
	repoRoot := repoRootFromSourceRoot(payload.srcRoot)
	if !g.skillCheckoutHasAncestry(repoRoot) {
		return composeSkillInstallRemedy(payload, nil, false, true, agent...), nil
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
	return composeSkillInstallRemedy(payload, g.skillDriftEntriesWithDirection(repoRoot, dir, report.Entries), true, true, agent...), nil
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
func (g *Group) runSkillSync(out io.Writer, mode skillSyncMode, only string) error {
	if err := g.validateSkillAgentFilter(only); err != nil {
		return err
	}

	payload, err := g.skillPayload()
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
	record := g.skillPayloadRecord(payload, libskillinstall.WriterExplicit)

	recordErr := func(err error) {
		if only != "" {
			targetErr = err
		}
	}
	extract := func(target skillInstallAgent, dir, verb string) {
		if err := extractSkillPayload(out, target.command, dir, payload, record, mode); err != nil {
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

	for _, target := range g.skillInstallAgents {
		if only != "" && target.command != only {
			continue
		}
		if _, err := g.StartupLookPath(target.command); err != nil {
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
		case mode == skillSyncForce, mode == skillSyncSetAside:
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

// reportSkillCheckHold reports the binary's guarded decision independently of
// the checkout comparison and without consulting or updating the notice cache.
func (g *Group) reportSkillCheckHold(out io.Writer, target skillInstallAgent, dir string, installed bool) {
	if !installed {
		return
	}
	_, stale, err := libskillinstall.InstalledNeedsUpdate(dir, skillpayload.SkillsFS)
	if err != nil || !stale {
		return
	}
	embedded := g.skillPayloadRecord(selectedSkillPayload{fsys: skillpayload.SkillsFS}, libskillinstall.WriterUnattended)
	decision, err := libskillinstall.InstalledRefreshDecision(dir, embedded, g.skillPayloadOrder())
	if err == nil && decision.Recorded && !decision.Refresh {
		_, _ = fmt.Fprintln(out, "  held: "+skillRefreshHoldLine(target.name, decision, embedded))
	}
}
