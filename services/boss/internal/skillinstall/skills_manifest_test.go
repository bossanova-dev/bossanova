package skillinstall

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
)

// TestEmbeddedSkillManifestExcludesBossProof pins the exact set of skills the boss
// binary ships via the embedded skillinstall payload. BOS-271 consolidated the four
// published cores (boss-epic/implement/plan/review) onto this single-source home and
// dropped boss-proof from the publish set — boss-proof stays a repo-local dev skill
// under .claude/skills, never embedded. This test fails loudly if boss-proof (or any
// other unexpected skill) is re-added to, or an expected core drops out of, the embed.
func TestEmbeddedSkillManifestExcludesBossProof(t *testing.T) {
	entries, err := fs.ReadDir(SkillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}

	var got []string
	for _, e := range entries {
		if e.IsDir() {
			got = append(got, e.Name())
		}
	}
	sort.Strings(got)

	want := []string{
		"boss",
		"boss-build",
		"boss-epic",
		"boss-finalize",
		"boss-plan",
		"boss-repair",
		"boss-review",
		"boss-verify",
	}

	if len(got) != len(want) {
		t.Fatalf("embedded skill set = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("embedded skill set = %v; want %v", got, want)
		}
	}

	for _, name := range got {
		if name == "boss-proof" {
			t.Fatalf("boss-proof must not be embedded (dropped from the publish set in BOS-271)")
		}
	}
}

// knownCores enumerates the published/dev core skill names. It is the core-name set,
// NOT an extension-name enumeration, so it stays stable as extensions are added or
// renamed. boss-proof is included even though it is not embedded (BOS-271 dropped it
// from the publish set) because it is the parent core of the boss-proof-* extensions,
// so its prefix is needed to recognize them.
var knownCores = map[string]bool{
	"boss":          true,
	"boss-epic":     true,
	"boss-finalize": true,
	"boss-build":    true,
	"boss-plan":     true,
	"boss-proof":    true,
	"boss-repair":   true,
	"boss-review":   true,
	"boss-verify":   true,
}

// isExtensionDirName reports whether a skill directory name is a boss-<core>-<suffix>
// extension (e.g. boss-review-golang, boss-plan-draft) rather than a core. A name is an
// extension iff it is not itself a known core AND it carries a known core name plus a
// trailing "-<suffix>" segment. The "not itself a core" guard keeps a two-segment core
// like boss-plan (which prefix-matches the bare "boss" core) from being misclassified.
func isExtensionDirName(name string) bool {
	if knownCores[name] {
		return false
	}
	for core := range knownCores {
		if strings.HasPrefix(name, core+"-") {
			return true
		}
	}
	return false
}

// TestIsExtensionDirName pins the classifier's true AND false branches directly, so the
// BOS-288 hard gate cannot silently regress. TestSkillPayloadsExcludeExtensions only ever
// exercises isExtensionDirName against the embedded/mirror dirs, which today hold cores
// only — the true branch is never taken there, so a helper that regressed to always-false
// would still pass that gate green while shipping an extension. This table exercises both.
func TestIsExtensionDirName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		// Cores are never extensions (knownCores guard), including two-segment cores that
		// prefix-collide with the bare "boss" core, and boss-proof (a core not embedded).
		{"boss", false},
		{"boss-plan", false},
		{"boss-review", false},
		{"boss-build", false},
		{"boss-proof", false},
		// boss-<core>-<suffix> names are extensions.
		{"boss-plan-draft", true},
		{"boss-review-golang", true},
		{"boss-review-thermonuclear", true},
		{"boss-proof-web", true},
		{"boss-build-superpowers", true},
		// Non-boss / unrelated names are not extensions.
		{"golang-pro", false},
		{"bossnew", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := isExtensionDirName(tc.name); got != tc.want {
			t.Errorf("isExtensionDirName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSkillPayloadsExcludeExtensions guards the BOS-288 disjoint-set invariant: the
// repo-local boss-<skill>-* extensions are discovered and dispatched repo-local from a
// worktree's .claude/skills and must NEVER be shipped in the embedded payload or the
// claude plugin mirror. Publishing/installing an extension would defeat the model
// documented in docs/skills/extension-contract.md. This is the hard gate that keeps the
// publish allowlist honest as extensions are added.
func TestSkillPayloadsExcludeExtensions(t *testing.T) {
	// 1. Embedded payload (SkillsFS).
	embedded, err := fs.ReadDir(SkillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}
	for _, e := range embedded {
		if e.IsDir() && isExtensionDirName(e.Name()) {
			t.Errorf("embedded payload ships extension %q; extensions must stay repo-local (BOS-288)", e.Name())
		}
	}

	// 2. Claude plugin mirror (git-tracked on disk, a different module).
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	mirrorDir := filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills")
	mirror, err := os.ReadDir(mirrorDir)
	if err != nil {
		t.Fatalf("read plugin mirror %s: %v", mirrorDir, err)
	}
	for _, e := range mirror {
		if e.IsDir() && isExtensionDirName(e.Name()) {
			t.Errorf("plugin mirror ships extension %q; extensions must stay repo-local (BOS-288)", e.Name())
		}
	}
}

func TestEmbeddedSkillMetadataUsesPublishedBossNames(t *testing.T) {
	entries, err := fs.ReadDir(SkillsFS, "skills")
	if err != nil {
		t.Fatalf("read embedded skills dir: %v", err)
	}

	frontmatterName := regexp.MustCompile(`(?m)^name:\s*([a-z0-9-]+)\s*$`)
	legacyHeading := regexp.MustCompile(`(?m)^#\s+BS\s+`)
	legacyDisplayName := regexp.MustCompile(`(?m)^\s*display_name:\s*['"]?BS\s+`)

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		skillPath := filepath.ToSlash(filepath.Join("skills", name, "SKILL.md"))
		content, err := fs.ReadFile(SkillsFS, skillPath)
		if err != nil {
			t.Fatalf("read %s: %v", skillPath, err)
		}

		matches := frontmatterName.FindStringSubmatch(string(content))
		if matches == nil {
			t.Fatalf("%s: missing frontmatter name", skillPath)
		}
		if matches[1] != name {
			t.Fatalf("%s: frontmatter name = %q; want directory name %q", skillPath, matches[1], name)
		}
		if legacyHeading.Match(content) {
			t.Fatalf("%s: H1 must use boss-* naming, not legacy BS branding", skillPath)
		}

		agentPath := filepath.ToSlash(filepath.Join("skills", name, "agents", "openai.yaml"))
		agentContent, err := fs.ReadFile(SkillsFS, agentPath)
		if err != nil {
			if strings.Contains(err.Error(), "file does not exist") {
				continue
			}
			t.Fatalf("read %s: %v", agentPath, err)
		}
		if legacyDisplayName.Match(agentContent) {
			t.Fatalf("%s: display_name must use boss-* naming, not legacy BS branding", agentPath)
		}
	}
}

// shippedPayloads returns the two payload trees a published core can reach a user's machine
// through: the embedded skillinstall FS the boss CLI extracts, and the on-disk claude plugin mirror
// bossd ships. Every cross-payload gate below scans BOTH, because either one alone can be the copy a
// user's global skill directory is populated from — so they resolve the pair here rather than each
// re-deriving the repo root.
func shippedPayloads(t *testing.T) map[string]fs.FS {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	return map[string]fs.FS{
		"embedded skillinstall payload": SkillsFS,
		"claude plugin mirror":          os.DirFS(filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata")),
	}
}

// coreBodyFiles names the payload files that together carry a core's step-by-step spine, for the
// gates that must follow an instruction wherever it lives rather than only into `SKILL.md`.
//
// BOS-674 extracted boss-build's Steps 8-12 — including the Step 12 post-terminal notes dispatch
// site and its toolbox resolver — out of the always-resident body and into
// `references/finalize-and-stop.md`, so a SKILL.md-only read reports the site as deleted when it
// merely moved. The list stays EXPLICIT rather than becoming a tree-wide walk of `references/`:
// a tree-wide search would stay green after a clause was deleted from a core that names it in
// unrelated references too (see TestPublishedCoresNameSkillPathForExtensionDispatch).
var coreBodyFiles = map[string][]string{
	"boss-build": {"SKILL.md", "references/finalize-and-stop.md"},
}

// bodyFilesFor returns the payload-relative files that make up core's spine, defaulting to the
// resident body alone for cores that have not extracted a step range.
func bodyFilesFor(core string) []string {
	if files, ok := coreBodyFiles[core]; ok {
		return files
	}
	return []string{"SKILL.md"}
}

// baseMergeCandidate captures every `git merge` / `git pull` invocation together with its
// argument tail, so baseMergeDirectives can decide whether the operands name the branch's BASE
// ref. `git merge-base` and `git rebase` cannot match: the pattern requires whitespace directly
// after the subcommand. The tail stops at a backtick or newline so an inline-code mention that
// only NAMES the command (e.g. "a `git merge` of the base ref is FORBIDDEN") captures an empty
// tail and is not mistaken for a directive.
var baseMergeCandidate = regexp.MustCompile("git\\s+(merge|pull)\\s+([^\n`]*)")

// baseRefOperand matches an operand that names a branch's base ref. Remote-tracking spellings
// (`origin/x`, `remotes/origin/x`, `upstream/x`), the `$BASE_BRANCH` variable in any of its
// spellings, `FETCH_HEAD` (what a bare `git fetch` leaves behind), and the conventional bare base
// names all count — `git pull origin main` is exactly as poisonous as `git merge origin/main`.
var baseRefOperand = regexp.MustCompile(`^(?:remotes/)?(?:origin|upstream)/|^\$\{?BASE_BRANCH|^FETCH_HEAD$|^(?:main|master|develop|staging|production|trunk)$`)

// baseMergeDirectives returns every base-merge instruction found in doc. A candidate is a
// directive when some non-flag operand names a base ref; `--rebase`/`-r` pulls are exempt (they
// linearize, which is the sanctioned form), and a trailing `#` comment is not scanned so a
// comment word cannot manufacture a false positive.
func baseMergeDirectives(doc string) []string {
	var hits []string
	for _, match := range baseMergeCandidate.FindAllStringSubmatch(doc, -1) {
		rebasing := false
		operands := make([]string, 0, 4)
		for _, field := range strings.Fields(match[2]) {
			field = strings.Trim(field, "\"'`.,;:()")
			if strings.HasPrefix(field, "#") {
				break // trailing comment: not part of the command
			}
			if strings.HasPrefix(field, "-") {
				if flag := strings.SplitN(field, "=", 2)[0]; flag == "--rebase" || flag == "-r" {
					rebasing = true
				}
				continue
			}
			operands = append(operands, field)
		}
		if rebasing {
			continue
		}
		for _, operand := range operands {
			if baseRefOperand.MatchString(operand) {
				hits = append(hits, strings.TrimSpace(match[0]))
				break
			}
		}
	}
	return hits
}

// TestPublishedCoresNeverInstructBaseMerge is the cross-core linear-history gate. Every
// published boss-* core is extracted into each user's global skill directory, so a single
// core telling an agent to merge the base branch in re-introduces the failure this gate
// exists to prevent: on a repo whose merge strategy is rebase, a merge commit on the PR
// branch makes GitHub structurally refuse the merge and deadlocks the PR. Base sync is
// ALWAYS a rebase; the invariant is stated in boss-repair and asserted before push in
// boss-finalize. Prose that forbids the practice must not spell the directive out either —
// state the prohibition without emitting a copyable command.
//
// Both shipped payloads are scanned (the embedded skillinstall FS the boss CLI extracts and the
// on-disk claude plugin mirror bossd ships), because either one alone can be the copy a user's
// global skill directory is populated from.
func TestPublishedCoresNeverInstructBaseMerge(t *testing.T) {
	payloads := shippedPayloads(t)

	for label, fsys := range payloads {
		scanned := 0
		err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			scanned++
			for _, hit := range baseMergeDirectives(string(data)) {
				t.Errorf("%s: %s instructs a base merge (%q); published cores must sync with the base by rebasing — a merge commit breaks GitHub's rebase-merge and deadlocks the PR", label, path, hit)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		// A payload that silently resolves to zero files would make this gate vacuous.
		if scanned == 0 {
			t.Fatalf("%s: no markdown scanned; the base-merge gate would pass vacuously", label)
		}
	}
}

// TestBaseMergeDirectivesDetection pins the classifier itself: the forms it must catch (so the
// gate cannot quietly stop gating) and the forms it must not (so the skills can keep documenting
// the sanctioned rebase-based commands and can name the forbidden command in prose).
func TestBaseMergeDirectivesDetection(t *testing.T) {
	forbidden := []string{
		"git merge origin/main",
		"git merge origin/$BASE_BRANCH",
		"git merge \"origin/$BASE_BRANCH\"",
		"git merge --no-ff \"origin/${BASE_BRANCH}\"",
		"git merge -X ours origin/main",
		"git merge remotes/origin/main",
		"git merge upstream/main",
		"git merge FETCH_HEAD",
		"git merge main",
		"git merge \"$BASE_BRANCH\"",
		"git pull origin main",
		"git pull --no-rebase origin \"$BASE_BRANCH\"",
	}
	for _, cmd := range forbidden {
		if len(baseMergeDirectives(cmd)) == 0 {
			t.Errorf("expected %q to be flagged as a base merge", cmd)
		}
	}

	allowed := []string{
		"git merge-base \"origin/$BASE_BRANCH\" HEAD",
		"git merge-base --is-ancestor \"origin/$BASE_BRANCH\" HEAD",
		"git rebase \"origin/$BASE_BRANCH\"",
		"git rebase --onto \"origin/$BASE_BRANCH\" \"$(git merge-base \"origin/$BASE_BRANCH\" HEAD)\"",
		"git pull --rebase",
		"git pull --rebase origin \"$BASE_BRANCH\"",
		"git merge --abort",
		"git merge --continue",
		"A `git merge` of the base ref is FORBIDDEN.",
		"git merge --abort  # never merge main into the branch",
	}
	for _, cmd := range allowed {
		if hits := baseMergeDirectives(cmd); len(hits) != 0 {
			t.Errorf("expected %q not to be flagged, got %v", cmd, hits)
		}
	}
}

// identityRule matches a class of project-specific identity a PUBLISHED core must not contain.
// Each rule records a leak token per (core, file): when normalize is set the stable `token` is
// recorded (used where the concrete text varies, e.g. the backlog team query written both as
// `team=Bossanova` and `team **Bossanova**`); otherwise each distinct literal match is recorded, so
// a generic server pattern captures `bossanova-linear` and `bossanova-dev` as separate tokens. The
// guard is absolute (zero tolerance): any recorded token in any core fails the build.
type identityRule struct {
	token     string // stable leak token recorded when normalize is true
	normalize bool
	re        *regexp.Regexp
}

// forbiddenIdentity are the project-specific identity patterns a PUBLISHED, globally-installed
// boss-* core must never contain. Every published core is extracted into each user's global
// skill directory (~/.claude/skills, ~/.codex/skills — see lib/bossalib/skillinstall/extract.go),
// so it surfaces in EVERY project on the machine and must be project-agnostic
// (docs/skills/README.md: "a suite of portable, config-driven skills … they ship publicly").
// A published core must reach the issue tracker and proof store ONLY through the
// config-selected adapters in .boss-skills.json, never a hard-coded Bossanova identifier —
// naming one here is exactly the leak that made /boss-plan unusable from unrelated repos.
//
// Coverage matches the invariant documented in CLAUDE.md ("boss-* skills are published globally"):
// project MCP servers / tool namespaces, the proof store, the "internal" self-label, and the
// BOS backlog — its team, project key, direct ticket identifiers (BOS-123), and the hard-coded
// `Unplanned`/`Todo` state names. Patterns are precise on purpose: the backlog-team rule anchors on
// `team … Bossanova` so it does not sweep in incidental prose that merely names the product
// (e.g. "Bossanova cloud login").
var forbiddenIdentity = []identityRule{
	// Any project MCP server / workspace slug: bossanova-linear, bossanova-sentry,
	// bossanova-dev (Linear workspace), bossanova-proof-production (R2 bucket), etc.
	{re: regexp.MustCompile(`bossanova-[a-z0-9-]+`)},
	// Any project MCP tool namespace (mcp__bossanova-*).
	{re: regexp.MustCompile(`mcp__bossanova`)},
	// Project proof-publish base URL.
	{re: regexp.MustCompile(`proof\.bossanova\.dev`)},
	// "internal project skill" self-label.
	{re: regexp.MustCompile(`Internal Bossanova`)},
	// BOS backlog team identity, written `team=Bossanova` or `Team **Bossanova**`
	// (sentence-cased "Team" included).
	{token: "team=Bossanova", normalize: true, re: regexp.MustCompile(`[Tt]eam[\s=*]+Bossanova`)},
	// BOS backlog project key, written (key `BOS`).
	{token: "key BOS", normalize: true, re: regexp.MustCompile("key\\s+`?BOS`?")},
	// BOS backlog ticket identifiers: BOS-123 and the BOS-NN/BOS-Y placeholder forms.
	{token: "BOS-<id>", normalize: true, re: regexp.MustCompile(`\bBOS-[0-9A-Za-z]+`)},
	// Hard-coded Linear state names baked into the portable core.
	{token: "Unplanned", normalize: true, re: regexp.MustCompile(`\bUnplanned\b`)},
	{token: "Todo", normalize: true, re: regexp.MustCompile(`\bTodo\b`)},
}

// knownIdentityLeaks is the tolerated-leak allowlist. The de-hard-code epic (BOS-449) migrated
// every project-specific identity in the published cores onto the .boss-skills.json tracker/publish
// adapters, so this map is now EMPTY: no leak is tolerated and the guard is absolute. Keep it empty
// — do NOT add an entry to make a new leak pass; route the skill through the config-selected adapter
// instead (see CLAUDE.md "boss-* skills are published globally"). Emptiness is the whole contract
// and is enforced mechanically by the len(knownIdentityLeaks) check in
// TestPublishedCoresAreProjectAgnostic, so re-adding an entry fails the build. (The nested map[…]int
// value type is retained from the retired occurrence-count ratchet; only presence/emptiness is read
// now.)
var knownIdentityLeaks = map[string]map[string]int{}

// identityLeaks walks a skill payload (an fs.FS rooted so that "skills/<core>/..." resolves) and
// returns, per top-level core, a representative file (the first seen) for each forbidden identity
// token found. An empty result means the payload is project-agnostic.
func identityLeaks(t *testing.T, fsys fs.FS) map[string]map[string]string {
	t.Helper()
	out := map[string]map[string]string{}
	err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(path, "skills/")
		core, _, _ := strings.Cut(rel, "/")
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		content := string(data)
		record := func(tok string) {
			if out[core] == nil {
				out[core] = map[string]string{}
			}
			if _, seen := out[core][tok]; !seen {
				out[core][tok] = rel
			}
		}
		for _, rule := range forbiddenIdentity {
			matches := rule.re.FindAllString(content, -1)
			if len(matches) == 0 {
				continue
			}
			if rule.normalize {
				record(rule.token)
				continue
			}
			for _, m := range matches {
				record(m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk skills payload: %v", err)
	}
	return out
}

// TestPublishedCoresAreProjectAgnostic is the hard, ZERO-TOLERANCE gate keeping project-specific
// identity out of the globally-installed boss-* cores. It scans both shipped payloads (the embedded
// skillinstall FS the boss CLI extracts, and the on-disk claude plugin mirror bossd ships) for
// forbidden Bossanova identifiers — project MCP servers / tool namespaces (bossanova-*,
// mcp__bossanova-*), the R2 publish store, the "Internal Bossanova" self-label, and the BOS backlog
// (team=Bossanova, key BOS, BOS-123 ticket ids, and the hard-coded Unplanned/Todo state names). Any
// such token in any core fails the build; knownIdentityLeaks (the tolerated-leak allowlist) is
// empty, so nothing is tolerated.
func TestPublishedCoresAreProjectAgnostic(t *testing.T) {
	// The guard is absolute: the tolerated-leak allowlist must stay empty. Enforce that
	// mechanically so a future edit cannot re-open the escape hatch by adding an entry to
	// knownIdentityLeaks (which would silently make a new leak pass). Route the skill through the
	// .boss-skills.json adapter instead (see CLAUDE.md "boss-* skills are published globally").
	if len(knownIdentityLeaks) != 0 {
		t.Fatalf("knownIdentityLeaks must stay empty (zero-tolerance guard): the de-hard-code epic migrated every project-specific identity onto the .boss-skills.json tracker/publish adapters; do NOT add an entry to tolerate a leak — fix the skill to go through an adapter instead")
	}
	payloads := shippedPayloads(t)
	for label, fsys := range payloads {
		// knownIdentityLeaks is asserted empty above, so every recorded token is a failure —
		// no per-token allowlist consultation is needed (it would be dead code).
		for core, tokens := range identityLeaks(t, fsys) {
			for tok, file := range tokens {
				t.Errorf("%s: published core %q references project-specific identity %q (in skills/%s) — a boss-* core installs into every user's ~/.claude/skills and must reach the tracker/publish store via the .boss-skills.json adapter, never a hard-coded Bossanova identifier (see CLAUDE.md \"boss-* skills are published globally\")", label, core, tok, file)
			}
		}
	}
}

// TestPublishedCoresDoNotHardCodeNamespaceHelperPaths keeps globally installed
// skills independent of this repository's installation namespace. The installer
// exposes each boss-* core as a top-level skill, so helper resolution must use
// that portable location (or BOSS_SKILLS_HOME), never a namespace literal.
func TestPublishedCoresDoNotHardCodeNamespaceHelperPaths(t *testing.T) {
	for label, fsys := range shippedPayloads(t) {
		for _, core := range []string{"boss-build", "boss-plan", "boss-review", "boss-epic", "boss-repair"} {
			path := "skills/" + core + "/SKILL.md"
			content, err := fs.ReadFile(fsys, path)
			if err != nil {
				t.Fatalf("%s: read %s: %v", label, path, err)
			}
			if strings.Contains(string(content), "/skills/bossanova") {
				t.Errorf("%s: %s hard-codes the installation namespace; resolve helpers through the top-level boss-* skill path or BOSS_SKILLS_HOME", label, path)
			}
		}
	}
}

// TestPublishedCoreNotesHelpersResolveFromHomeWhenSkillsHomeIsUnset covers the
// terminal notes-hook path independently of a host-provided BOSS_SKILLS_HOME.
// Published cores are installed under a user's home directory, so the exact
// resolver shown in each notes hook must find a Codex installation there.
func TestNotesToolboxResolverUsesPostTerminalNotesSection(t *testing.T) {
	const beforeNotes = `BOSS_REVIEW_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}/boss-review/toolbox"
if [ ! -d "$BOSS_REVIEW_TOOLBOX" ]; then BOSS_REVIEW_TOOLBOX="$HOME/.codex/skills/boss-review/toolbox"; fi`
	const notesResolver = `BOSS_REVIEW_TOOLBOX="${BOSS_SKILLS_HOME:-$HOME/.codex/skills}/boss-review/toolbox"
if [ ! -d "$BOSS_REVIEW_TOOLBOX" ]; then BOSS_REVIEW_TOOLBOX="$HOME/.claude/skills/boss-review/toolbox"; fi`
	content := beforeNotes + `

### Post-terminal notes extensions (repo opt-in)

` + notesResolver + `
NOTES_JSON=$(node "$BOSS_REVIEW_TOOLBOX/skill-extensions.mjs" discover --core boss-review --role notes --json)
`

	if got := notesToolboxResolver(t, content, "boss-review", "BOSS_REVIEW_TOOLBOX"); got != notesResolver {
		t.Fatalf("resolver = %q; want post-terminal notes resolver %q", got, notesResolver)
	}
}

func TestPublishedCoreNotesHelpersResolveFromHomeWhenSkillsHomeIsUnset(t *testing.T) {
	cores := []struct {
		name    string
		toolbox string
	}{
		{"boss-build", "BOSS_BUILD_TOOLBOX"},
		{"boss-epic", "BOSS_EPIC_TOOLBOX"},
		{"boss-plan", "BOSS_PLAN_TOOLBOX"},
		{"boss-repair", "BOSS_REPAIR_TOOLBOX"},
		{"boss-review", "BOSS_REVIEW_TOOLBOX"},
	}

	for label, fsys := range shippedPayloads(t) {
		for _, core := range cores {
			t.Run(label+"/"+core.name, func(t *testing.T) {
				// BOS-674: the notes section may live in an extracted spine file rather than
				// SKILL.md (boss-build's Step 12 moved to references/finalize-and-stop.md).
				// The rooted-home ban applies to every spine file; the resolver is read from
				// whichever one carries the section.
				const notesHeading = "### Post-terminal notes extensions (repo opt-in)"
				var content string
				for _, rel := range bodyFilesFor(core.name) {
					data, err := fs.ReadFile(fsys, "skills/"+core.name+"/"+rel)
					if err != nil {
						t.Fatalf("read %s: %v", rel, err)
					}
					for _, rootedHome := range []string{`"/.claude/skills"`, `"/.codex/skills"`, `:-/.claude/skills`, `=/.codex/skills`} {
						if strings.Contains(string(data), rootedHome) {
							t.Fatalf("published skill %s contains rooted helper home %q", rel, rootedHome)
						}
					}
					if content == "" && strings.Contains(string(data), notesHeading) {
						content = string(data)
					}
				}
				resolver := notesToolboxResolver(t, content, core.name, core.toolbox)

				home := t.TempDir()
				want := filepath.Join(home, ".codex", "skills", core.name, "toolbox")
				if err := os.MkdirAll(want, 0o755); err != nil {
					t.Fatalf("create toolbox: %v", err)
				}
				cmd := exec.Command("bash", "-c", resolver+"\nprintf '%s' \"$"+core.toolbox+"\"")
				cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH")}
				got, err := cmd.Output()
				if err != nil {
					t.Fatalf("resolve notes helper: %v", err)
				}
				if string(got) != want {
					t.Fatalf("notes helper = %q; want %q when BOSS_SKILLS_HOME is unset", got, want)
				}
			})
		}
	}
}

func notesToolboxResolver(t *testing.T, content, core, toolbox string) string {
	t.Helper()
	const heading = "### Post-terminal notes extensions (repo opt-in)"
	_, notesSection, found := strings.Cut(content, heading)
	if !found {
		t.Fatalf("%s post-terminal notes section not found", core)
	}

	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(toolbox) + `="\$\{BOSS_SKILLS_HOME:-[^}]+\}/` + regexp.QuoteMeta(core) + `/toolbox"\n(?:if \[ ! -d "\$` + regexp.QuoteMeta(toolbox) + `" \]; then ` + regexp.QuoteMeta(toolbox) + `="[^"]+/` + regexp.QuoteMeta(core) + `/toolbox"; fi|\[ -d "\$` + regexp.QuoteMeta(toolbox) + `" \] \|\| ` + regexp.QuoteMeta(toolbox) + `="[^"]+/` + regexp.QuoteMeta(core) + `/toolbox")$`)
	loc := pattern.FindStringIndex(notesSection)
	if loc == nil {
		t.Fatalf("%s terminal notes-hook resolver not found", core)
	}
	discovery := `NOTES_JSON=$(node "$` + toolbox + `/skill-extensions.mjs" discover --core ` + core + ` --role notes --json)`
	if !strings.HasPrefix(strings.TrimLeft(notesSection[loc[1]:], " \t\r\n"), discovery) {
		t.Fatalf("%s terminal notes-hook discovery command not found after resolver", core)
	}
	return notesSection[loc[0]:loc[1]]
}

// scriptRefPattern matches a repo-root `scripts/<path>` token anywhere in a payload file — prose
// and fenced code alike, .md and .mjs alike. A published core is extracted into a user's global
// skill directory with nothing but its own tree, so a `scripts/` path it names resolves ONLY if the
// consuming repo happens to carry that file. Prose references are as invisible-and-uncopied as
// executed ones; that is exactly how docs/skills/README.md's hand-vendoring instruction let a
// consuming repo re-derive boss-plan's epic library from scratch.
var scriptRefPattern = regexp.MustCompile(`scripts/[A-Za-z0-9._/-]+`)

// scriptRefTrimCutset is the trailing punctuation stripped off a raw match so markdown decoration
// (`node scripts/proof.mjs`, [x](scripts/foo.mjs), "scripts/foo.mjs".) normalizes to the bare path.
const scriptRefTrimCutset = ".,)]`'\"*:;"

// scriptRefExtensions are the script/data suffixes that make a token an actual invocable path
// rather than prose that merely contains a slash (e.g. boss-review/SKILL.md's "touched only
// scripts/docs").
var scriptRefExtensions = []string{".mjs", ".cjs", ".js", ".ts", ".sh", ".json"}

// unshippedScriptRefs returns the normalized, deduplicated `scripts/<path>` tokens in content, in
// first-seen order. A raw match is kept only when it is not glued to a preceding word character
// (so `subscripts/foo.mjs` is not read as a `scripts/` reference) and, after trimming trailing
// punctuation, its final segment carries a script/data extension OR the token ends in `/` (the
// directory form, e.g. `scripts/publish/`).
func unshippedScriptRefs(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range scriptRefPattern.FindAllStringIndex(content, -1) {
		if loc[0] > 0 {
			prev := content[loc[0]-1]
			isWord := (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9')
			if isWord {
				continue
			}
		}
		token := strings.TrimRight(content[loc[0]:loc[1]], scriptRefTrimCutset)
		if token == "" || seen[token] {
			continue
		}
		if !strings.HasSuffix(token, "/") {
			last := token[strings.LastIndex(token, "/")+1:]
			hasExt := false
			for _, ext := range scriptRefExtensions {
				if strings.HasSuffix(last, ext) {
					hasExt = true
					break
				}
			}
			if !hasExt {
				continue
			}
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

// knownUnshippedScriptRefs is the SHRINK-ONLY baseline of repo-root `scripts/` paths a published
// core is still allowed to name because the file belongs to a seam this repo deliberately keeps
// repo-local: genuinely custom proof harnesses and the remaining boss-plan adapter seams.
// Everything else
// must SHIP — vendor the helper into the core's toolbox/ (scripts/vendor-toolbox.mjs VENDOR_MAP)
// and reference it through $BOSS_<CORE>_TOOLBOX.
//
// The map only ever shrinks: TestPublishedCoresOnlyReferenceShippedScripts fails on an entry that
// is no longer observed ("stale baseline entry"), so removing a reference forces its entry out and
// the ratchet cannot silently slacken. boss-repair needs no entry — its
// scripts/review-feedback-probe.js ships INSIDE the skill payload, which the gate detects by stat.
var knownUnshippedScriptRefs = map[string]map[string]bool{
	"boss-build": {
		// boss-proof is excluded from publication; its roughly 5x closure and repo-specific recipes,
		// scenarios, and publish store belong to the consuming repository's proof pipeline.
		"scripts/proof.mjs": true,
		// This is an in-repo authoring example, not a file boss-build executes.
		"scripts/testdata/scenario-fixtures/valid-full.json": true,
	},
	"boss-epic": {},
	"boss-plan": {
		// The boss-plan cron gate is owned by its dedicated vendoring ticket.
		"scripts/cron-gates/boss-plan.mjs": true,
		// Linear dependency traversal remains a repo-specific tracker seam.
		"scripts/linear-deps-lib.mjs": true,
		// Linear gate selection remains a repo-specific tracker seam.
		"scripts/linear-gate-lib.mjs": true,
		// boss-proof is excluded from publication and its recipes and storage are repo-supplied.
		"scripts/proof.mjs": true,
	},
	"boss-review": {},
}

// TestPublishedCoresOnlyReferenceShippedScripts is the fail-closed ratchet keeping every published
// core's repo-root `scripts/` references visible and shrink-only. A published core installs into a
// user's global skill directory carrying only its own tree, so a `scripts/<path>` it names is
// resolvable only when the consuming repo re-creates that file by hand — the exact failure this
// gate exists to make impossible to add silently.
//
// A reference passes when EITHER the path ships inside the skill payload itself
// (skills/<core>/<token>, as boss-repair's scripts/review-feedback-probe.js does) OR it is listed
// in knownUnshippedScriptRefs. Anything else fails with the remedy. After the walk, every baseline
// entry that was NOT observed fails too: the baseline can only shrink.
//
// Both shipped payloads are scanned (the embedded skillinstall FS the boss CLI extracts and the
// on-disk claude plugin mirror bossd ships), because either one alone can be the copy a user's
// global skill directory is populated from.
func TestPublishedCoresOnlyReferenceShippedScripts(t *testing.T) {
	payloads := shippedPayloads(t)

	for label, fsys := range payloads {
		scanned := 0
		observed := map[string]map[string]bool{}
		err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			scanned++
			rel := strings.TrimPrefix(path, "skills/")
			core, _, _ := strings.Cut(rel, "/")
			for _, token := range unshippedScriptRefs(string(data)) {
				// The path ships inside the skill's own payload (a skill-local scripts/ dir).
				if _, statErr := fs.Stat(fsys, "skills/"+core+"/"+token); statErr == nil {
					continue
				}
				if observed[core] == nil {
					observed[core] = map[string]bool{}
				}
				observed[core][token] = true
				if knownUnshippedScriptRefs[core][token] {
					continue
				}
				t.Errorf("%s: published core %q references unshipped repo-root path %q (in skills/%s) — a boss-* core installs into every user's global skill directory carrying only its own tree, so that path does not exist there; vendor the helper into the core's toolbox/ (scripts/vendor-toolbox.mjs VENDOR_MAP) and reference it through $BOSS_<CORE>_TOOLBOX, or add a baseline entry in knownUnshippedScriptRefs only for a genuinely out-of-scope adapter-seam path", label, core, token, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		// A payload that silently resolves to zero files would make this gate vacuous.
		if scanned == 0 {
			t.Fatalf("%s: no files scanned; the unshipped-script gate would pass vacuously", label)
		}
		// The ratchet: a baseline entry whose reference is gone must be removed, so the
		// allowlist can never quietly outlive the reference it was granted for.
		for core, tokens := range knownUnshippedScriptRefs {
			for token := range tokens {
				if !observed[core][token] {
					t.Errorf("%s: stale baseline entry — knownUnshippedScriptRefs[%q][%q] is no longer referenced by the payload; remove it (the baseline is shrink-only)", label, core, token)
				}
			}
		}
	}
}

// TestUnshippedScriptRefsDetection pins the classifier itself: the forms it must catch (so the
// gate cannot quietly stop gating) and the forms it must not (so prose that merely contains the
// word can stay).
func TestUnshippedScriptRefsDetection(t *testing.T) {
	matched := map[string]string{
		"node scripts/proof.mjs run":                     "scripts/proof.mjs",
		"see `scripts/tracker/linear.mjs` for the shape": "scripts/tracker/linear.mjs",
		"adapters live in scripts/tracker/":              "scripts/tracker/",
		"read [the fixture](scripts/testdata/a.json).":   "scripts/testdata/a.json",
		"run `./scripts/worktree-lock.sh` first":         "scripts/worktree-lock.sh",
		"**scripts/skill-extensions.mjs**":               "scripts/skill-extensions.mjs",
	}
	for content, want := range matched {
		got := unshippedScriptRefs(content)
		if len(got) != 1 || got[0] != want {
			t.Errorf("unshippedScriptRefs(%q) = %v, want exactly [%q]", content, got, want)
		}
	}

	ignored := []string{
		"touched only scripts/docs in that pass",
		"see subscripts/foo.mjs for the nested case",
		"the scripts directory is repo-local",
		"postscripts/bar.sh is unrelated",
	}
	for _, content := range ignored {
		if got := unshippedScriptRefs(content); len(got) != 0 {
			t.Errorf("unshippedScriptRefs(%q) = %v, want none", content, got)
		}
	}

	// Deduplication keeps one entry per distinct token, in first-seen order.
	got := unshippedScriptRefs("scripts/a.mjs then scripts/b.mjs then `scripts/a.mjs`")
	if len(got) != 2 || got[0] != "scripts/a.mjs" || got[1] != "scripts/b.mjs" {
		t.Errorf("unshippedScriptRefs dedup = %v, want [scripts/a.mjs scripts/b.mjs]", got)
	}
}

// namespacedSkillPattern matches a plugin-namespaced skill invocation (`<namespace>:<kebab-skill>`).
// The form is structural, not a name list: no boss-* core is plugin-namespaced, so any match is a
// skill the consuming repo is not guaranteed to have — including plugins that do not exist yet.
//
// Requiring the skill half to carry a hyphen is what makes it precise. Measured across the whole
// published tree, the strict form yields exactly one hit; a relaxed variant allowing a single-word
// skill half yields 24, 23 of them ordinary `key:value` prose (`file:line`, `agent:claude`,
// `refname:short`, `node:path`, `epic:true`, …).
var namespacedSkillPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_/-])([a-z][a-z0-9]*(?:-[a-z0-9]+)*):([a-z][a-z0-9]*(?:-[a-z0-9]+)+)`)

// foreignSkillNames is a BOUNDED DENYLIST of skill names a published core must not assert: an
// operator's globally-installed reviewer and this repo's vendored lenses. A published core reaches
// specialists through the `.boss-skills.json` lens registry, never by naming one in prose.
//
// This is deliberately a denylist and not the allowlist that would generalize, because an allowlist
// is NOT EXPRESSIBLE over prose: a skill name in markdown carries no lexical marker. The narrowest
// plausible handle — a bare hyphenated inline-code token — yields ~40 distinct tokens across the
// published markdown and does not contain `impeccable` at all, which is a single English word. Any
// rule that catches `impeccable` must name it. namespacedSkillPattern is the general half of this
// gate; this list is the honest, explicitly-named half. Do not describe it as more than that.
var foreignSkillNames = []string{"impeccable", "golang-pro", "tui-design", "thermonuclear-review"}

// foreignSkillFamily matches the skill family of an unrelated private repository, whose names would
// be unresolvable for every consumer of a published core.
var foreignSkillFamily = regexp.MustCompile(`\bwc-[a-z0-9-]+\b|\bwondercanvas\b`)

// foreignSkillNamePatterns anchors each denylisted name on a hyphen-aware word boundary, so a name
// that is a substring of a legitimate one is not matched (`api-review` never yields `review`, and
// `boss-review` is untouched).
var foreignSkillNamePatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(foreignSkillNames))
	for _, name := range foreignSkillNames {
		out = append(out, regexp.MustCompile(`(^|[^A-Za-z0-9_-])`+regexp.QuoteMeta(name)+`([^A-Za-z0-9_-]|$)`))
	}
	return out
}()

// foreignSkillRefs returns the normalized, deduplicated foreign-skill tokens in content: first the
// plugin-namespaced invocations in full (`superpowers:requesting-code-review`) in first-seen order,
// then each denylisted bare name in list order, then the foreign-repo family matches. Bare names are
// matched on a word boundary so `boss-review` and `api-review` — the core's own siblings and
// methodology citations — are untouched.
func foreignSkillRefs(content string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(token string) {
		if token == "" || seen[token] {
			return
		}
		seen[token] = true
		out = append(out, token)
	}
	for _, m := range namespacedSkillPattern.FindAllStringSubmatch(content, -1) {
		add(m[2] + ":" + m[3])
	}
	for i, pattern := range foreignSkillNamePatterns {
		if pattern.MatchString(content) {
			add(foreignSkillNames[i])
		}
	}
	for _, m := range foreignSkillFamily.FindAllString(content, -1) {
		add(m)
	}
	return out
}

// knownForeignSkillRefs is the SHRINK-ONLY baseline of foreign skill names a published core still
// asserts, keyed payload-relative FILE → token. The finer key matters: keying on the core alone
// would let a baselined token license a brand-new use of the same name elsewhere in that core.
//
// The baseline was seeded ONCE, from this gate's own failing output, and only ever shrinks:
// TestPublishedCoresNameNoForeignSkills fails on an entry no longer observed ("stale baseline
// entry"). The remedy for a NEW violation is to reword the core, never to append here.
//
// The key is presence, not an occurrence count, and that granularity is deliberate. The unit of
// harm this gate exists to catch is "this published file names this absent skill" — a second mention
// of an already-baselined token in an already-baselined file is the same leak, drained by the same
// reword. Counting instead would break `foreignSkillRefs`'s pinned dedup contract
// (TestForeignSkillRefsDetection) and force the bare-name leg from MatchString onto FindAllString,
// buying a stricter ratchet over four tokens in one file at the cost of turning every benign reflow
// of that contended file into a red build. What the presence key still catches is what matters: a
// NEW token in a baselined file fails, any token in an unbaselined file fails, and an entry whose
// last occurrence is gone fails as stale.
//
// The entries below are un-drained and owned by no ticket: boss-review/SKILL.md's frontmatter and
// opening section are contended by several in-flight tickets at once, so rewording them from here
// would be a merge conflict rather than a fix.
var knownForeignSkillRefs = map[string]map[string]bool{
	"boss-review/SKILL.md": {
		// Named as a conditional Go lens in the opening summary; deferred with the file.
		"golang-pro": true,
		// Named as a conditional web lens in the opening summary; deferred with the file.
		"impeccable": true,
		// Named as a conditional TUI lens in the opening summary; deferred with the file.
		"tui-design": true,
		// The foreign-repo skill this core is described as the analogue of; deferred with the file.
		"wc-auto-review": true,
	},
}

// TestPublishedCoresNameNoForeignSkills is the fourth leg of the payload-reference triangle. The
// other three gate artifacts that carry a path prefix — repo-root `scripts/<path>`,
// `$BOSS_<CORE>_TOOLBOX/<file>`, core-relative `references/<file>`. A SKILL has no prefix: it is
// invoked by bare name, which is exactly why a published core naming another repo's review roster
// passed every existing gate. forbiddenIdentity misses it too — every one of its rules keys on a
// Bossanova/backlog token, and these names contain none.
//
// Scope is markdown only. A `<core>/toolbox/*.mjs` config default naming a lens is the INTENDED
// design (the lens registry pairs every entry with an inline fallback rubric for exactly the case
// where the named reviewer is absent), so scanning .mjs would ratchet against the config seam
// rather than against a leak.
//
// Both shipped payloads are scanned (the embedded skillinstall FS the boss CLI extracts and the
// on-disk claude plugin mirror bossd ships), because either one alone can be the copy a user's
// global skill directory is populated from.
func TestPublishedCoresNameNoForeignSkills(t *testing.T) {
	payloads := shippedPayloads(t)

	for label, fsys := range payloads {
		scanned := 0
		observed := map[string]map[string]bool{}
		err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".md") {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			scanned++
			rel := strings.TrimPrefix(path, "skills/")
			for _, token := range foreignSkillRefs(string(data)) {
				if observed[rel] == nil {
					observed[rel] = map[string]bool{}
				}
				observed[rel][token] = true
				if knownForeignSkillRefs[rel][token] {
					continue
				}
				t.Errorf("%s: published core file %q names foreign skill %q — a boss-* core installs into every user's global skill directory, where that skill does not exist; reword the passage to describe the SHAPE of the step and reach specialists through the .boss-skills.json lens registry instead of naming one", label, rel, token)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		// A payload that silently resolves to zero markdown files would make this gate vacuous.
		if scanned == 0 {
			t.Fatalf("%s: no markdown files scanned; the foreign-skill gate would pass vacuously", label)
		}
		// The ratchet: a baseline entry whose reference is gone must be removed, so the
		// baseline can never quietly outlive the leak it was granted for.
		for rel, tokens := range knownForeignSkillRefs {
			for token := range tokens {
				if !observed[rel][token] {
					t.Errorf("%s: stale baseline entry — knownForeignSkillRefs[%q][%q] is no longer named by the payload; remove it (the baseline is shrink-only)", label, rel, token)
				}
			}
		}
	}
}

// TestForeignSkillRefsDetection pins the classifier itself: the forms it must catch (so the gate
// cannot quietly stop gating) and the forms it must not — ordinary `key:value` prose, the core's
// own siblings, and methodology citations that name a review kind rather than a skill.
func TestForeignSkillRefsDetection(t *testing.T) {
	matched := map[string]string{
		"run a `superpowers:requesting-code-review` round": "superpowers:requesting-code-review",
		"prints a `wc-auto-review`-style report":           "wc-auto-review",
		"the wondercanvas repo owns it":                    "wondercanvas",
		"`impeccable` for services/web":                    "impeccable",
		"lenses (`golang-pro` for Go)":                     "golang-pro",
		"`tui-design` for the TUI":                         "tui-design",
		"a vendored `thermonuclear-review` round":          "thermonuclear-review",
	}
	for content, want := range matched {
		got := foreignSkillRefs(content)
		if len(got) != 1 || got[0] != want {
			t.Errorf("foreignSkillRefs(%q) = %v, want exactly [%q]", content, got, want)
		}
	}

	ignored := []string{
		"name the blocker with file:line so a human can jump to it",
		"the selector agent:claude resolves the runner",
		"git for-each-ref --format='%(refname:short)'",
		"import { join } from 'node:path'",
		"a bare key:value term inside one clause",
		"the host:owner form is accepted too",
		"pass epic:true to widen the selection",
		"dispatch one general-purpose subagent",
		"diff the branch against its merge-base",
		"the read-only probe is safe to repeat",
		"an agent-friendly planned ticket",
		"invoke the boss-review skill with no args",
		"boss-finalize injects the PR tag",
		"the api-review lens covers proto changes",
		"see references/receiving-code-review.md for the fix discipline",
	}
	for _, content := range ignored {
		if got := foreignSkillRefs(content); len(got) != 0 {
			t.Errorf("foreignSkillRefs(%q) = %v, want none", content, got)
		}
	}

	// Deduplication keeps one entry per distinct token, in first-seen order.
	got := foreignSkillRefs("`impeccable`, then `golang-pro`, then `impeccable` again")
	if len(got) != 2 || got[0] != "impeccable" || got[1] != "golang-pro" {
		t.Errorf("foreignSkillRefs dedup = %v, want [impeccable golang-pro]", got)
	}
}

// toolboxRefPattern matches a `$BOSS_<CORE>_TOOLBOX/<file>` token anywhere in a payload file, in
// each spelling the skills actually use: the bare shell form (`$BOSS_PLAN_TOOLBOX/f`), the braced
// shell form (`${BOSS_PLAN_TOOLBOX}/f`), and the JS template-literal form
// (`${process.env.BOSS_EPIC_TOOLBOX}/f`). The variable name — not the containing directory — names
// the owning core, which is how the skills define it
// (`BOSS_PLAN_TOOLBOX="$BOSS_SKILLS_HOME/boss-plan/toolbox"`), so a cross-core reference resolves
// against the core it actually names, and a multi-word core (`BOSS_A_B_TOOLBOX` → `boss-a-b`) is
// spelled back correctly.
//
// One spelling is deliberately OUT of reach: string concatenation that separates the variable from
// the path (`process.env.BOSS_PLAN_TOOLBOX+'/skill-config.mjs'`) has no adjacency for a regex to key
// on. So this gate is a floor, not a census — it proves every ADJACENT reference ships, not that
// every shipped helper is referenced.
var toolboxRefPattern = regexp.MustCompile(`\$\{?(?:process\.env\.)?BOSS_([A-Z]+(?:_[A-Z]+)*)_TOOLBOX\}?/([A-Za-z0-9._/-]+)`)

// toolboxRefs returns the (core, file) pairs a payload file names through a $BOSS_*_TOOLBOX
// variable, with trailing markdown punctuation stripped off the file token.
func toolboxRefs(content string) [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	for _, m := range toolboxRefPattern.FindAllStringSubmatch(content, -1) {
		file := strings.TrimRight(m[2], scriptRefTrimCutset)
		if file == "" {
			continue
		}
		core := "boss-" + strings.ToLower(strings.ReplaceAll(m[1], "_", "-"))
		key := core + "/" + file
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, [2]string{core, file})
	}
	return out
}

// coresWithSubdir lists the payload's top-level cores that ship a non-empty <sub>/ directory
// (`toolbox` or `references`). Used for the per-core vacuity guards: a payload-global "found at
// least one ref" check cannot see a single core's coverage silently dropping to zero.
func coresWithSubdir(t *testing.T, fsys fs.FS, sub string) []string {
	t.Helper()
	entries, err := fs.ReadDir(fsys, "skills")
	if err != nil {
		t.Fatalf("read skills dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := "skills/" + e.Name() + "/" + sub
		files, err := fs.ReadDir(fsys, dir)
		if err != nil {
			// Only "the core ships no <sub>" is an expected miss. Any other read error would
			// silently shrink the set this guard iterates, disabling the vacuity check it feeds.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			t.Fatalf("read %s: %v", dir, err)
		}
		if len(files) == 0 {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// TestPublishedCoresShipTheToolboxFilesTheyName is the POSITIVE inverse of
// TestPublishedCoresOnlyReferenceShippedScripts: that gate proves a core names no path it does not
// ship, this one proves every path it names ADJACENT to a $BOSS_<CORE>_TOOLBOX variable (see
// toolboxRefPattern for the spellings covered, and the one that is not) is actually IN the payload.
//
// Without it a helper can silently drop out of the shipped tree — remove it from the
// scripts/vendor-toolbox.mjs VENDOR_MAP, delete both payload copies and their embedsrcs lines, and
// vendor-toolbox --check (which only walks VENDOR_MAP), the skill-parity gate (both trees pruned in
// lockstep) and the unshipped-scripts gate (no `scripts/` token appears) all stay green while the
// SKILL still invokes the vanished file. That silent-payload-subset failure is exactly what this
// gate exists to close, so it is gated rather than trusted.
func TestPublishedCoresShipTheToolboxFilesTheyName(t *testing.T) {
	payloads := shippedPayloads(t)

	for label, fsys := range payloads {
		refsByCore := map[string]int{}
		err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			for _, ref := range toolboxRefs(string(data)) {
				refsByCore[ref[0]]++
				want := "skills/" + ref[0] + "/toolbox/" + ref[1]
				if _, statErr := fs.Stat(fsys, want); statErr != nil {
					t.Errorf("%s: %s names $BOSS_%s_TOOLBOX/%s but %s is not in the payload — the published core would install without it; add the helper to the scripts/vendor-toolbox.mjs VENDOR_MAP and to BOTH BUILD.bazel embedsrcs lists (services/boss/internal/skillinstall and plugins/bossd-plugin-claude/skilldata), or stop naming it",
						label, strings.TrimPrefix(path, "skills/"), strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(ref[0], "boss-"), "-", "_")), ref[1], want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		// Per-CORE vacuity guard. A payload-global "found at least one" check cannot see one core's
		// coverage silently dropping to zero (reworded onto an unmatched spelling, or the classifier
		// regressing on that form) while other cores keep the count non-zero. Every core that ships a
		// toolbox invokes something out of it, so each must contribute at least one matched reference.
		for _, core := range coresWithSubdir(t, fsys, "toolbox") {
			if refsByCore[core] == 0 {
				t.Errorf("%s: core %q ships a toolbox/ but names no $BOSS_*_TOOLBOX/<file> the classifier matches; the shipped-toolbox gate is vacuous for it — reference the helpers through the variable, or extend toolboxRefPattern to the spelling used", label, core)
			}
		}
	}
}

// TestToolboxRefsDetection pins the classifier's own behaviour, so the gate above cannot regress to
// finding nothing (which the refs==0 guard catches) or to resolving against the wrong core.
func TestToolboxRefsDetection(t *testing.T) {
	got := toolboxRefs("run `node \"$BOSS_PLAN_TOOLBOX/plan-image-guard.mjs\"` then ${BOSS_REVIEW_TOOLBOX}/bs-review-caps.mjs.")
	want := [][2]string{{"boss-plan", "plan-image-guard.mjs"}, {"boss-review", "bs-review-caps.mjs"}}
	if len(got) != len(want) {
		t.Fatalf("toolboxRefs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("toolboxRefs = %v, want %v", got, want)
		}
	}
	// The JS template-literal spelling boss-epic uses resolves to the same core.
	if tpl := toolboxRefs("await import(`${process.env.BOSS_EPIC_TOOLBOX}/bs-epic-lib.mjs`)"); len(tpl) != 1 || tpl[0] != [2]string{"boss-epic", "bs-epic-lib.mjs"} {
		t.Errorf("toolboxRefs template-literal = %v, want [{boss-epic bs-epic-lib.mjs}]", tpl)
	}
	// A multi-word core spells back with hyphens, not underscores.
	if multi := toolboxRefs("$BOSS_SWEEP_PLAN_TOOLBOX/f.mjs"); len(multi) != 1 || multi[0] != [2]string{"boss-sweep-plan", "f.mjs"} {
		t.Errorf("toolboxRefs multi-word core = %v, want [{boss-sweep-plan f.mjs}]", multi)
	}
	// Deduplication keeps one entry per distinct (core, file).
	if dup := toolboxRefs("$BOSS_BUILD_TOOLBOX/a.mjs and $BOSS_BUILD_TOOLBOX/a.mjs"); len(dup) != 1 {
		t.Errorf("toolboxRefs dedup = %v, want one entry", dup)
	}
	// Forms that name no file are not references: the bare variable, the directory-only form, and
	// the `:?` set-guard the boss-plan slug recipe passes as an argument. All three occur in the
	// live payloads, so a loosening that turned any of them into a bogus (core, file) pair would
	// fail the shipped-toolbox gate on a file that was never named.
	for _, content := range []string{
		"export BOSS_PLAN_TOOLBOX; echo $BOSS_PLAN_TOOLBOX",
		`ls "$BOSS_PLAN_TOOLBOX/"`,
		`node -e "…" ISSUE "title" "${BOSS_PLAN_TOOLBOX:?}"`,
	} {
		if none := toolboxRefs(content); len(none) != 0 {
			t.Errorf("toolboxRefs(%q) = %v, want none", content, none)
		}
	}
}

// referenceRefPattern matches a bare `references/<path>` token anywhere in a payload file. A
// published core's SKILL.md routes mandatory steps through sibling reference documents by this
// core-relative path (e.g. boss-plan's entire tracker-attachment plan-storage flow lives behind
// `references/plan-storage.md`), and neither existing classifier sees this form: scriptRefPattern
// is anchored on the literal `scripts/`, and toolboxRefPattern requires a $BOSS_<CORE>_TOOLBOX
// prefix. So a core could name a reference it does not ship and every gate stayed green.
//
// The path is CORE-relative, not toolbox-style core-qualified: the owning core is the core of the
// file doing the naming, because SKILL.md sits at the core root and reaches `references/` beneath
// it. That is why this needs its own resolution rule rather than a widened toolboxRefPattern.
var referenceRefPattern = regexp.MustCompile(`references/[A-Za-z0-9._/-]+`)

// referenceRefExtensions are the suffixes that make a token an actual readable artifact rather
// than prose that merely contains the word (e.g. "the references/ directory"). References are
// markdown today; the script/data suffixes are folded in so a future `references/schema.json`
// is gated the day it lands rather than silently ungated.
var referenceRefExtensions = append([]string{".md"}, scriptRefExtensions...)

// referenceRefs returns the normalized, deduplicated FILE tokens (the part after `references/`)
// that content names core-relative, in first-seen order.
//
// A raw match is kept only when it is not glued to a preceding word character (so
// `subreferences/foo.md` is not read as a reference) AND not preceded by `/` (so a PREFIXED path
// like `docs/references/x.md` or `skills/boss-plan/references/y.md` is not silently resolved
// against the wrong core — the prefixed spelling is prohibited outright by
// crossCoreReferenceRefs, not resolved here), and when its final segment carries a
// referenceRefExtensions suffix after trailing markdown punctuation is trimmed.
func referenceRefs(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range referenceRefPattern.FindAllStringIndex(content, -1) {
		if loc[0] > 0 {
			prev := content[loc[0]-1]
			isWord := (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9')
			if isWord || prev == '/' {
				continue
			}
		}
		token := strings.TrimRight(content[loc[0]:loc[1]], scriptRefTrimCutset)
		file := strings.TrimPrefix(token, "references/")
		if file == "" || strings.HasSuffix(file, "/") || seen[file] {
			continue
		}
		last := file[strings.LastIndex(file, "/")+1:]
		hasExt := false
		for _, ext := range referenceRefExtensions {
			if strings.HasSuffix(last, ext) {
				hasExt = true
				break
			}
		}
		if !hasExt {
			continue
		}
		seen[file] = true
		out = append(out, file)
	}
	return out
}

// crossCoreReferencePattern matches the ONE spelling that could express a cross-core reference:
// a `<core>/references/<file>` path prefixed with a core name. The bare form referenceRefs
// handles is core-relative by construction and cannot name another core.
var crossCoreReferencePattern = regexp.MustCompile(`(boss(?:-[a-z]+)*)/references/([A-Za-z0-9._/-]+)`)

// crossCoreReferenceRefs returns the prefixed `<knownCore>/references/<file>` tokens in content.
// Naming one is illegal: a published core is extracted into a user's global skill directory
// carrying nothing but its own tree, so a sibling core's references/ is not resolvable there —
// exactly the failure TestPublishedCoresOnlyReferenceShippedScripts closes for `scripts/`.
func crossCoreReferenceRefs(content string) []string {
	var out []string
	seen := map[string]bool{}
	for _, loc := range crossCoreReferencePattern.FindAllStringSubmatchIndex(content, -1) {
		if loc[0] > 0 {
			prev := content[loc[0]-1]
			isWord := (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9')
			if isWord {
				continue
			}
		}
		// A top-level skills/<core>/references/... token names a path in the source payload,
		// not a sibling reference from the installed core. Other slash-prefixed paths (such as
		// ../<core>/...) still name an unreachable sibling and must be reported.
		if strings.HasSuffix(content[:loc[0]], "skills/") {
			skillsStart := loc[0] - len("skills/")
			if skillsStart == 0 {
				continue
			}
			prev := content[skillsStart-1]
			isWord := (prev >= 'a' && prev <= 'z') || (prev >= 'A' && prev <= 'Z') || (prev >= '0' && prev <= '9')
			if !isWord && prev != '/' {
				continue
			}
		}
		core := content[loc[2]:loc[3]]
		if !knownCores[core] {
			continue
		}
		file := content[loc[4]:loc[5]]
		token := strings.TrimRight(core+"/references/"+file, scriptRefTrimCutset)
		if seen[token] {
			continue
		}
		seen[token] = true
		out = append(out, token)
	}
	return out
}

// TestReferenceRefsDetection pins the core-relative references/ classifier: the spellings the
// live payloads actually use (so the gate cannot quietly stop gating), and the spellings it must
// NOT claim — a prose mention of the bare word, a nested directory name, and any PREFIXED path,
// which is not core-relative and is handled by the cross-core prohibition instead.
func TestReferenceRefsDetection(t *testing.T) {
	// Every spelling below is verbatim-shaped from the live payloads.
	matched := map[string]string{
		"the flow lives in `references/interactive-mode.md`; the":           "interactive-mode.md",
		"**[`references/cron-gate.md`](references/cron-gate.md)** for":      "cron-gate.md",
		"provisioned this worktree (references/standalone-mode.md):":        "standalone-mode.md",
		"Pass it the **path** `references/headless-drafting-brief.md` (not": "headless-drafting-brief.md",
		"see [the core methodology](references/core-methodology.md), every": "core-methodology.md",
	}
	for content, want := range matched {
		got := referenceRefs(content)
		if len(got) != 1 || got[0] != want {
			t.Errorf("referenceRefs(%q) = %v, want exactly [%q]", content, got, want)
		}
	}

	ignored := []string{
		// Glued to a preceding word character: a different word, not a reference.
		"see subreferences/foo.md for the nested case",
		// PREFIXED: not core-relative. Ungated here on purpose; Task 4 prohibits the
		// knownCores spelling outright, so this must not be silently resolved core-relative.
		"docs/references/foo.md is a repo doc",
		"skills/boss-plan/references/plan-storage.md ships in the payload",
		// Prose that merely contains the word, and the bare directory form.
		"the references directory is core-relative",
		"everything under `references/` is markdown",
		// No script/doc extension: not an invocable or readable artifact path.
		"references/notafile",
	}
	for _, content := range ignored {
		if got := referenceRefs(content); len(got) != 0 {
			t.Errorf("referenceRefs(%q) = %v, want none", content, got)
		}
	}

	// Deduplication keeps one entry per distinct file, in first-seen order — the
	// `[text](target)` markdown form names the same file twice on one line.
	got := referenceRefs("[`references/a.md`](references/a.md) then references/b.md")
	if len(got) != 2 || got[0] != "a.md" || got[1] != "b.md" {
		t.Errorf("referenceRefs dedup = %v, want [a.md b.md]", got)
	}

	// A PREFIXED knownCores path is the only way to express a cross-core reference, and it is
	// illegal: a published core installs carrying only its own tree, so a sibling core's
	// references/ is not reachable there. referenceRefs declines to resolve it (asserted above);
	// crossCoreReferenceRefs is what rejects it.
	if got := crossCoreReferenceRefs("see boss-plan/references/plan-storage.md for the flow"); len(got) != 1 || got[0] != "boss-plan/references/plan-storage.md" {
		t.Errorf("crossCoreReferenceRefs = %v, want [boss-plan/references/plan-storage.md]", got)
	}
	// Traversal cannot make a sibling reference available to an installed core.
	if got := crossCoreReferenceRefs("see ../boss-plan/references/plan-storage.md for the flow"); len(got) != 1 || got[0] != "boss-plan/references/plan-storage.md" {
		t.Errorf("crossCoreReferenceRefs(traversal) = %v, want [boss-plan/references/plan-storage.md]", got)
	}
	// The legal core-relative form is not a cross-core reference.
	if got := crossCoreReferenceRefs("see `references/plan-storage.md` for the flow"); len(got) != 0 {
		t.Errorf("crossCoreReferenceRefs(core-relative) = %v, want none", got)
	}
	// A prefix that is not a known core is not this gate's business.
	if got := crossCoreReferenceRefs("docs/references/plan-storage.md is a repo doc"); len(got) != 0 {
		t.Errorf("crossCoreReferenceRefs(non-core prefix) = %v, want none", got)
	}
	// A repo-qualified core path is not a cross-core token; it names a payload location.
	if got := crossCoreReferenceRefs("skills/boss-plan/references/plan-storage.md ships in the payload"); len(got) != 0 {
		t.Errorf("crossCoreReferenceRefs(repo-qualified) = %v, want none", got)
	}
}

// missingReferenceRefs walks a shipped payload and returns one finding per core-relative
// references/<file> that a payload file names but the payload does not carry, plus the per-core
// count of MATCHED references (a missing one still counts as named — the count answers "did the
// classifier see anything for this core", which is a different question from "did it resolve").
//
// Factored out of the gate so the miss branch can be driven from a synthetic FS: see
// TestMissingReferenceRefsDetectsAnAbsentFile. The owning core is the core of the file doing the
// naming, because the path is core-relative.
func missingReferenceRefs(fsys fs.FS) ([]string, map[string]int, error) {
	var findings []string
	refsByCore := map[string]int{}
	err := fs.WalkDir(fsys, "skills", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "skills/")
		core, _, _ := strings.Cut(rel, "/")
		for _, file := range referenceRefs(string(data)) {
			refsByCore[core]++
			want := "skills/" + core + "/references/" + file
			if _, statErr := fs.Stat(fsys, want); statErr != nil {
				findings = append(findings, rel+" names references/"+file+" but "+want+" is not in the payload")
			}
		}
		for _, token := range crossCoreReferenceRefs(string(data)) {
			findings = append(findings, rel+" names the cross-core path "+token+", which is illegal")
		}
		return nil
	})
	return findings, refsByCore, err
}

// TestMissingReferenceRefsDetectsAnAbsentFile is the NON-VACUITY proof for the gate below. The
// per-core refs>0 guard only catches "the classifier stopped matching"; it cannot show the gate
// would actually FAIL on a payload that names a reference it does not ship. A gate that cannot
// fail is precisely the failure mode this ticket exists to close, so the miss branch is driven
// directly here against a synthetic payload.
func TestMissingReferenceRefsDetectsAnAbsentFile(t *testing.T) {
	// Control: the named reference SHIPS, so nothing is reported.
	present := fstest.MapFS{
		"skills/boss-plan/SKILL.md":                   {Data: []byte("read `references/plan-storage.md` first")},
		"skills/boss-plan/references/plan-storage.md": {Data: []byte("# plan storage")},
	}
	findings, refsByCore, err := missingReferenceRefs(present)
	if err != nil {
		t.Fatalf("missingReferenceRefs(present): %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("missingReferenceRefs(present) = %v, want none", findings)
	}
	if refsByCore["boss-plan"] != 1 {
		t.Errorf("refsByCore[boss-plan] = %d, want 1", refsByCore["boss-plan"])
	}

	// The regression: the payload names a reference it does not ship.
	absent := fstest.MapFS{
		"skills/boss-plan/SKILL.md":                  {Data: []byte("read `references/plan-storage.md` first")},
		"skills/boss-plan/references/interactive.md": {Data: []byte("# some other reference")},
	}
	findings, refsByCore, err = missingReferenceRefs(absent)
	if err != nil {
		t.Fatalf("missingReferenceRefs(absent): %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("missingReferenceRefs(absent) = %v, want exactly one finding", findings)
	}
	for _, want := range []string{"boss-plan/SKILL.md", "references/plan-storage.md", "skills/boss-plan/references/plan-storage.md"} {
		if !strings.Contains(findings[0], want) {
			t.Errorf("finding %q does not name %q", findings[0], want)
		}
	}
	if refsByCore["boss-plan"] != 1 {
		t.Errorf("refsByCore[boss-plan] = %d, want 1 (a MISSING reference still counts as named)", refsByCore["boss-plan"])
	}
}

// TestPublishedCoresShipTheReferencesTheyName closes the third leg of the payload-reference
// triangle. TestPublishedCoresOnlyReferenceShippedScripts gates repo-root `scripts/` paths;
// TestPublishedCoresShipTheToolboxFilesTheyName gates $BOSS_<CORE>_TOOLBOX/<file> paths; NEITHER
// classifier matches a bare, core-relative `references/<file>.md`. So a core whose SKILL.md routes
// a mandatory step through a sibling reference — as boss-plan does for its entire
// tracker-attachment plan-storage flow — passed CI green whether or not that file was in the
// payload, and a payload-side regression would land in every consuming repo at once.
//
// Both shipped payloads are scanned (the embedded skillinstall FS the boss CLI extracts and the
// on-disk claude plugin mirror bossd ships), because either one alone can be the copy a user's
// global skill directory is populated from.
func TestPublishedCoresShipTheReferencesTheyName(t *testing.T) {
	payloads := shippedPayloads(t)

	for label, fsys := range payloads {
		findings, refsByCore, err := missingReferenceRefs(fsys)
		if err != nil {
			t.Fatalf("walk %s: %v", label, err)
		}
		for _, finding := range findings {
			t.Errorf("%s: %s — the published core would install without it; add the file to BOTH payload trees (`make copy-skills`) and to BOTH BUILD.bazel embedsrcs lists (services/boss/internal/skillinstall and plugins/bossd-plugin-claude/skilldata), or stop naming it. If this is prose naming ANOTHER core's reference, spell it without the bare path (references are resolved core-relative, against the core of the file doing the naming). A cross-core `<core>/references/<file>` path is never valid: an installed core carries only its own tree.", label, finding)
		}
		// Per-CORE vacuity guard, mirroring the toolbox gate. A payload-global "found at least
		// one" check cannot see one core's coverage silently dropping to zero (reworded onto an
		// unmatched spelling, or the classifier regressing on that form) while other cores keep
		// the count non-zero. Every core that ships a references/ dir points at it from its
		// SKILL.md, so each must contribute at least one matched reference.
		for _, core := range coresWithSubdir(t, fsys, "references") {
			if refsByCore[core] == 0 {
				t.Errorf("%s: core %q ships a references/ but names no core-relative references/<file> the classifier matches; the shipped-references gate is vacuous for it — reference the documents by their core-relative path, or extend referenceRefPattern to the spelling used", label, core)
			}
		}
	}
}

func TestBossPlanPayloadDocumentsAtomicAttachmentPublish(t *testing.T) {
	b, err := SkillsFS.ReadFile("skills/boss-plan/SKILL.md")
	if err != nil {
		t.Fatalf("read boss-plan payload: %v", err)
	}
	payload := string(b)
	for _, want := range []string{
		"tracker-attachment",
		"preparePlanAttachment",
		"finalizePlanAttachment",
		"deletePlanAttachment",
		"no plan metadata/state write",
		// BOS-651 re-pinned the persisted-epic-spec citation, it did not drop it: the spec
		// is no longer a `planMarkdown`-carrying description marker but a plain-JSON
		// `epic-spec.json` attachment serialized by `serializeEpicSpec`, so the payload must
		// now name that contract instead. These pin the CONTRACT TABLE ROWS rather than the
		// bare identifiers: `epic-spec.json` also appears in six Phase 5 `find … -delete`
		// cleanup lines and `serializeEpicSpec` in the Phase 2.5 toolbox roll-call, so bare
		// substrings would survive deleting the entire attachment contract this test is
		// named for. Pinning the table cells makes prose deletion falsify the gate.
		"| filename         | `epic-spec.json` (`specAttachmentFilename()`)",
		"| body             | `serializeEpicSpec(spec)`",
		// The stage-2 pre-PUT self-check: the only thing that catches a spec whose `parentId`
		// was never set, since `serializeEpicSpec` omits an absent id silently.
		"validateSpecIdentity(parseEpicSpec(",
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("boss-plan payload missing %q", want)
		}
	}
	// The drafting brief is a separate shipped file with its own copy of the epic write ordering,
	// and the assertions above read SKILL.md only. It is the file the headless drafting subagent is
	// actually handed, and this exact requirement has drifted between the two twice, so pin it.
	brief, err := SkillsFS.ReadFile("skills/boss-plan/references/headless-drafting-brief.md")
	if err != nil {
		t.Fatalf("read boss-plan drafting brief: %v", err)
	}
	if !strings.Contains(string(brief), "`deletePlanAttachment` was already required **before the FIRST epic") {
		t.Error("boss-plan drafting brief must require deletePlanAttachment before the first epic write, not at stage 3 (which every resume skips)")
	}
	storage, err := SkillsFS.ReadFile("skills/boss-plan/references/plan-storage.md")
	if err != nil {
		t.Fatalf("read boss-plan storage reference: %v", err)
	}
	if !strings.Contains(string(storage), "Delete that exact scratch file immediately after the PUT returns") {
		t.Error("boss-plan storage reference must remove attachment-header scratch files immediately")
	}
	for _, forbidden := range []string{"planStorageFor", "plan-publish", "R2 credential"} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("boss-plan payload still references retired plan storage %q", forbidden)
		}
	}
	// `planMarkdown` is a retired epic-spec FIELD, not part of the retired R2 plan-storage
	// subsystem above, so it gets its own loop and its own message: BOS-651's central deletion is
	// that the spec no longer persists child plan bodies at all, and a re-introduced `planMarkdown`
	// claim would document a field the library does not serialize. Nothing else guards that
	// deletion — the byte ratchet cannot see a swap.
	for _, forbidden := range []string{"planMarkdown"} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("boss-plan payload must not claim the spec persists %q; BOS-651 removed child plan bodies from the spec", forbidden)
		}
	}
}

// TestBossPlanEmbeddedSkillCopiesStayIdentical closes the gap every other core already covered:
// the assertions above read only the embedded payload, but bossd installs the plugin mirror, so a
// mirror edit (or a revert applied without `make copy-skills`) could ship prose the embedded gates
// never see. boss-epic, boss-finalize and boss-repair each have this test; boss-plan did not.
func TestBossPlanEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	repoRoot := findRepoRoot(t)
	for _, rel := range []string{
		"SKILL.md",
		filepath.Join("references", "headless-drafting-brief.md"),
		filepath.Join("references", "plan-storage.md"),
	} {
		t.Run(rel, func(t *testing.T) {
			embedded, err := SkillsFS.ReadFile("skills/boss-plan/" + filepath.ToSlash(rel))
			if err != nil {
				t.Fatalf("read embedded boss-plan %s: %v", rel, err)
			}
			mirror, err := os.ReadFile(filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-plan", rel))
			if err != nil {
				t.Fatalf("read plugin boss-plan %s: %v", rel, err)
			}
			if string(embedded) != string(mirror) {
				t.Errorf("boss-plan %s differs between services/boss and bossd-plugin-claude; run `make copy-skills`", rel)
			}
		})
	}
}

func TestBossBuildPayloadReadsNativePlanAttachmentsOnly(t *testing.T) {
	b, err := SkillsFS.ReadFile("skills/boss-build/SKILL.md")
	if err != nil {
		t.Fatalf("read boss-build payload: %v", err)
	}
	payload := string(b)
	for _, want := range []string{"selectImplementationPlanAttachment", "readPlanAttachment"} {
		if !strings.Contains(payload, want) {
			t.Errorf("boss-build payload missing %q", want)
		}
	}
	for _, forbidden := range []string{"planStorageFor", "publishConfigFor(config).baseUrl", "raw fetch behavior"} {
		if strings.Contains(payload, forbidden) {
			t.Errorf("boss-build payload still references retired plan storage %q", forbidden)
		}
	}
}
