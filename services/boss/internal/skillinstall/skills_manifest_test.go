package skillinstall

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
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
// A published core must reach the issue tracker and plan-publish store ONLY through the
// config-selected adapters in .boss-skills.json, never a hard-coded Bossanova identifier —
// naming one here is exactly the leak that made /boss-plan unusable from unrelated repos.
//
// Coverage matches the invariant documented in CLAUDE.md ("boss-* skills are published globally"):
// project MCP servers / tool namespaces, the plan-publish store, the "internal" self-label, and the
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
	// Project plan-publish base URL.
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
// repo-local: the publish/tracker/session/callback/finalize adapter modules, the proof harness,
// the Linear-specific libs, extension discovery, and the repo's own cron gates. Everything else
// must SHIP — vendor the helper into the core's toolbox/ (scripts/vendor-toolbox.mjs VENDOR_MAP)
// and reference it through $BOSS_<CORE>_TOOLBOX.
//
// The map only ever shrinks: TestPublishedCoresOnlyReferenceShippedScripts fails on an entry that
// is no longer observed ("stale baseline entry"), so removing a reference forces its entry out and
// the ratchet cannot silently slacken. boss-repair needs no entry — its
// scripts/review-feedback-probe.js ships INSIDE the skill payload, which the gate detects by stat.
var knownUnshippedScriptRefs = map[string]map[string]bool{
	"boss-build": {
		"scripts/bossd-present.mjs":                          true,
		"scripts/callback/adapter.mjs":                       true,
		"scripts/callback/boss.mjs":                          true,
		"scripts/check-no-inline-stop-hooks.mjs":             true,
		"scripts/cron-gates/boss-build.mjs":                  true,
		"scripts/finalize/adapter.mjs":                       true,
		"scripts/finalize/cli.mjs":                           true,
		"scripts/linear-deps-lib.mjs":                        true,
		"scripts/linear-gate-lib.mjs":                        true,
		"scripts/pr-ownership.mjs":                           true,
		"scripts/proof.mjs":                                  true,
		"scripts/remove-bossd-stop-hooks.mjs":                true,
		"scripts/skill-extensions.mjs":                       true,
		"scripts/testdata/scenario-fixtures/valid-full.json": true,
		"scripts/tracker/adapter.mjs":                        true,
		"scripts/tracker/cli.mjs":                            true,
		"scripts/tracker/linear.mjs":                         true,
	},
	"boss-epic": {
		"scripts/callback/adapter.mjs": true,
		"scripts/callback/boss.mjs":    true,
		"scripts/linear-deps-lib.mjs":  true,
		"scripts/session/adapter.mjs":  true,
		"scripts/session/boss.mjs":     true,
		"scripts/tracker/adapter.mjs":  true,
		"scripts/tracker/cli.mjs":      true,
		"scripts/tracker/linear.mjs":   true,
	},
	// boss-plan's deterministic planning core (plan-epic-lib.mjs, plan-image-guard.mjs,
	// plan-slug.mjs) now ships in its toolbox/, so it holds NO entry for them. What remains is
	// exclusively adapter-seam, proof-harness, tracker-lib, cron-gate and extension-discovery
	// paths that are out of scope for vendoring.
	"boss-plan": {
		"scripts/cron-gates/boss-plan.mjs": true,
		"scripts/linear-deps-lib.mjs":      true,
		"scripts/linear-gate-lib.mjs":      true,
		"scripts/plan-publish.mjs":         true,
		"scripts/proof.mjs":                true,
		"scripts/skill-extensions.mjs":     true,
	},
	"boss-review": {
		"scripts/skill-extensions.mjs": true,
	},
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
		"adapters live in scripts/publish/":              "scripts/publish/",
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

// coresWithToolbox lists the payload's top-level cores that ship a non-empty toolbox/ directory.
// Used for the per-core vacuity guard: a payload-global "found at least one ref" check cannot see a
// single core's coverage silently dropping to zero.
func coresWithToolbox(t *testing.T, fsys fs.FS) []string {
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
		dir := "skills/" + e.Name() + "/toolbox"
		files, err := fs.ReadDir(fsys, dir)
		if err != nil {
			// Only "the core ships no toolbox" is an expected miss. Any other read error would
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
		for _, core := range coresWithToolbox(t, fsys) {
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

func TestBossPlanPayloadDocumentsStorageKindsAndAtomicAttachmentPublish(t *testing.T) {
	b, err := SkillsFS.ReadFile("skills/boss-plan/SKILL.md")
	if err != nil {
		t.Fatalf("read boss-plan payload: %v", err)
	}
	payload := string(b)
	for _, want := range []string{
		"planStorage",
		"tracker-attachment",
		"preparePlanAttachment",
		"finalizePlanAttachment",
		"deletePlanAttachment",
		"no plan metadata/state write",
	} {
		if !strings.Contains(payload, want) {
			t.Errorf("boss-plan payload missing %q", want)
		}
	}
}
