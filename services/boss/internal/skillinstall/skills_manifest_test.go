package skillinstall

import (
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
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate repo root")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	// The guard is absolute: the tolerated-leak allowlist must stay empty. Enforce that
	// mechanically so a future edit cannot re-open the escape hatch by adding an entry to
	// knownIdentityLeaks (which would silently make a new leak pass). Route the skill through the
	// .boss-skills.json adapter instead (see CLAUDE.md "boss-* skills are published globally").
	if len(knownIdentityLeaks) != 0 {
		t.Fatalf("knownIdentityLeaks must stay empty (zero-tolerance guard): the de-hard-code epic migrated every project-specific identity onto the .boss-skills.json tracker/publish adapters; do NOT add an entry to tolerate a leak — fix the skill to go through an adapter instead")
	}
	payloads := map[string]fs.FS{
		"embedded skillinstall payload": SkillsFS,
		"claude plugin mirror":          os.DirFS(filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata")),
	}
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
