package skillinstall

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestBossEpicSkillGatesGreensOnTrackedChatStatus(t *testing.T) {
	skill := readEmbeddedBossEpicSkill(t)

	assertContains(t, skill, "`get_chat_statuses`")
	assertContains(t, skill, "recorded `chat_id`")
	assertContains(t, skill, "session-level `get_session_statuses` aggregate")
	assertContains(t, skill, "ticket to `greens`")
	assertContains(t, skill, "implementation chat")
}

func TestBossEpicSkillDefinesZeroLaunchNoReadyBranch(t *testing.T) {
	// BOS-302: a no-ready/no-inflight run spawns zero sessions and must follow a
	// single explicit branch — upsert exactly one progress comment carrying
	// "no sessions spawned", stop success, never create-then-edit two comments.
	skill := readEmbeddedBossEpicSkill(t)

	assertContains(t, skill, "zero-launch")
	assertContains(t, skill, "no sessions spawned")
	assertContains(t, skill, "upsert exactly one")
	assertContains(t, skill, "stop success")
	// Resume idempotence for the zero-launch comment still keys off the marker.
	assertContains(t, skill, "<!-- boss-epic-progress -->")

	// The zero-launch branch lives in Phase 1's empty-eligible step, before any
	// scheduling — not buried in the daemon-blocks-empty-PR edge case.
	start := strings.Index(skill, "## Phase 1 — Assemble the epic set")
	if start == -1 {
		t.Fatalf("heading %q not found", "## Phase 1 — Assemble the epic set")
	}
	end := strings.Index(skill, "## Phase 2 — Resume reconstruction")
	if end == -1 {
		t.Fatalf("heading %q not found", "## Phase 2 — Resume reconstruction")
	}
	phase1 := skill[start:end]
	assertContains(t, phase1, "zero-launch")
	assertContains(t, phase1, "no sessions spawned")
}

func TestBossEpicSkillReadsChildWallClockFromConfig(t *testing.T) {
	// BOS-1098: the per-child wall clock used to be the literal "default **90 min**" in this
	// prose. Children on a repo of this class measure 2-4 h, so that clock expired on healthy
	// work and fail-isolated children that were still making progress. The budget is now the
	// `epicDefaults.childWallClockMinutes` config knob read through the toolbox skill-config
	// seam; pin the key name here so a hardcoded number cannot silently return.
	skill := readEmbeddedBossEpicSkill(t)

	// sectionBetween rather than a hand-rolled strings.Index pair: it anchors both markers to
	// the start of a line, rejects an ambiguous start, and rejects any heading at or above the
	// start's level inside the window. A bare Index would let an inline prose mention of
	// "### 3c. Transitions" move the left edge and widen the window across most of the
	// document, at which point every assertion below passes vacuously with all gates green.
	transitions := sectionBetween(t, skill, "### 3c. Transitions", "### 3d. Merge")
	// Line wrapping in the source markdown is not part of the contract, so collapse it once and
	// assert every prose phrase against the flattened text. Asserting some phrases against the
	// unflattened section and others against the flattened one is the inconsistency that turns
	// an unrelated markdown reflow into a spurious red.
	flat := strings.Join(strings.Fields(transitions), " ")

	assertContains(t, flat, "**Wall clock exceeded**")
	assertContains(t, flat, "epicDefaults.childWallClockMinutes")
	assertContains(t, flat, "epicChildWallClockMinutes(config)")
	assertContains(t, flat, "toolbox/skill-config.mjs")
	// The accessor reference is only reachable if the skill actually binds a config: pin the
	// loader and the Phase 0 export so a future edit cannot delete the recipe and leave a
	// dangling `epicChildWallClockMinutes(config)` mention behind. A driver improvising a raw
	// .boss-skills.json read gets `undefined` for an absent `epicDefaults` block, and
	// `elapsed > undefined` is always false — a child that can never expire.
	assertContains(t, flat, "$BOSS_EPIC_CHILD_WALL_CLOCK_MIN")
	// Scope the recipe pins to the canonical invocation block's own section rather than the
	// whole file. A whole-file substring match cannot tell the live recipe from an incidental
	// mention — a changelog line, a rationale paragraph, a commented-out block — so it would
	// stay green after the recipe itself was deleted, which is the dangling-mention regression
	// these pins exist to catch.
	library := sectionBetween(t, skill, "## The library: how to compute a decision", "## Phase 0 — Preflight")
	assertContains(t, library, "loadSkillConfig")
	assertContains(t, library, "BOSS_EPIC_CHILD_WALL_CLOCK_MIN=")
	assertContains(t, library, "export BOSS_EPIC_CHILD_WALL_CLOCK_MIN")
	// A throwing loadSkillConfig yields an empty substitution, and an empty budget is exactly the
	// never-expires child this change exists to prevent — so the loader must fail closed, like its
	// planned-state neighbour three lines above it.
	assertContains(t, library, `test -n "$BOSS_EPIC_CHILD_WALL_CLOCK_MIN"`)
	// A guard that only binds the resolving block leaves the COMPARING block — a fresh shell
	// where the export is dead — able to reach `elapsed > ` with an empty budget, the same
	// never-expires child. Pin that 3c carries its own re-resolution and its own guard, so the
	// instruction is a recipe a driver can run rather than a direction to improvise.
	assertContains(t, flat, "re-resolve it here")
	assertContains(t, flat, `test -n "$BOSS_EPIC_CHILD_WALL_CLOCK_MIN"`)
	// And the recipe has to resolve its own $BOSS_EPIC_TOOLBOX, which is as dead in that fresh
	// shell as the budget is: without the prelude the import path is `undefined/skill-config.mjs`,
	// the guard above fires on every poll cycle, and a driver following the recipe blocks the run
	// rather than timing a child. Pin the same prelude the file's other standalone snippets carry
	// — all THREE lines as one flattened string, not just the assignment. Pinning only the
	// `BOSS_EPIC_TOOLBOX=` line stays green while the two lines that give `$BOSS_SKILLS_HOME` a
	// value are deleted, which resolves the toolbox to `/boss-epic/toolbox` and blocks every poll
	// cycle — the same regression, one line further up.
	assertContains(t, flat, `BOSS_SKILLS_HOME="${BOSS_SKILLS_HOME:-$HOME/.claude/skills}" `+
		`if [ ! -d "$BOSS_SKILLS_HOME/boss-epic/toolbox" ]; then BOSS_SKILLS_HOME="$HOME/.codex/skills"; fi `+
		`BOSS_EPIC_TOOLBOX="$BOSS_SKILLS_HOME/boss-epic/toolbox"; export BOSS_EPIC_TOOLBOX`)

	// The prose names a concrete default for a human reader, so couple that figure to the
	// toolbox constant it copies: read the vendored skill-config.mjs and require the skill to
	// name the same number. Hardcoding "360 min" here instead would let the constant move while
	// the published skill told every user the wrong number with all gates green.
	wantDefault := toolboxChildWallClockDefault(t)
	assertContains(t, flat, "**"+wantDefault+" min**")
	// The framing that stops an expiry from being read as death evidence, and the ban on
	// stopping the session, are what keep the verdict fail-isolating rather than repairing.
	assertContains(t, flat, "budget fact, not death evidence")
	assertContains(t, flat, "can never route to repair")
	assertContains(t, flat, "Do **not** `stop_session`")

	// The budget must have a home in the state the scheduling loop carries across cycles;
	// otherwise "carry the resolved integer forward" names no store and a driver improvises.
	schedule := sectionBetween(t, skill, "## Phase 3 — Scheduling loop", "### 3a. Launch")
	assertContains(t, strings.Join(strings.Fields(schedule), " "), "$BOSS_EPIC_CHILD_WALL_CLOCK_MIN")

	// A reintroduced literal default is the exact regression this pins. Ban only the full phrase:
	// a bare "90 min" would also ban the positive assertion above if the toolbox default ever
	// legitimately became 90, leaving the test unsatisfiable from either side.
	const hardcoded = "default **90 min**"
	if strings.Contains(skill, hardcoded) {
		t.Errorf("boss-epic skill hardcodes a child wall clock (%q); read the config key instead", hardcoded)
	}
}

func TestBossEpicSkillFailIsolateDoesNotCascadeToSiblings(t *testing.T) {
	// BOS-1098: a single child's failure (a spent wall clock included) must fail-isolate that
	// child only. The guarantee has to be stated in the fail-isolate procedure itself, because
	// that is where a reader arriving from the expiry bullet lands.
	skill := readEmbeddedBossEpicSkill(t)

	// Same line-anchored, ambiguity-rejecting window as the wall-clock pin above; see the
	// comment there for why a bare strings.Index would let this window widen silently.
	failIsolate := sectionBetween(t, skill, "### 3e. Fail-isolate bookkeeping", "### 3f. Report every transition")
	// Line wrapping is not part of the contract: flatten once, assert everything against it.
	flat := strings.Join(strings.Fields(failIsolate), " ")

	assertContains(t, flat, "leave the session open")
	assertContains(t, flat, "per-ticket")
	assertContains(t, flat, "keeps launching")
	assertContains(t, flat, "must not end the run")
	assertContains(t, flat, "a spent wall clock included")
	assertContains(t, flat, "keeps launching every other ready child")
}

func TestBossEpicEmbeddedSkillCopiesStayIdentical(t *testing.T) {
	serviceSkill := readEmbeddedBossEpicSkill(t)

	repoRoot := findRepoRoot(t)
	pluginPath := filepath.Join(repoRoot, "plugins", "bossd-plugin-claude", "skilldata", "skills", "boss-epic", "SKILL.md")
	pluginSkillBytes, err := os.ReadFile(pluginPath)
	if err != nil {
		t.Fatalf("read plugin boss-epic skill: %v", err)
	}

	if serviceSkill != string(pluginSkillBytes) {
		t.Fatalf("boss-epic skill copies differ between services/boss and bossd-plugin-claude")
	}
}

func readEmbeddedBossEpicSkill(t *testing.T) string {
	t.Helper()

	skillBytes, err := SkillsFS.ReadFile("skills/boss-epic/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded boss-epic skill: %v", err)
	}
	return string(skillBytes)
}

// childWallClockDefaultPattern extracts the built-in per-child wall clock from the vendored
// toolbox copy of skill-config.mjs — the same tree the published skill ships with, pinned
// byte-identical to skills-toolbox/ by `make vendor-toolbox-check`.
var childWallClockDefaultPattern = regexp.MustCompile(`childWallClockMinutes:\s*(\d+)`)

func toolboxChildWallClockDefault(t *testing.T) string {
	t.Helper()

	data, err := SkillsFS.ReadFile("skills/boss-epic/toolbox/skill-config.mjs")
	if err != nil {
		t.Fatalf("read embedded boss-epic toolbox skill-config.mjs: %v", err)
	}
	// Require exactly one declaration: the pattern runs over the whole file, so a second
	// occurrence (a doc comment or a usage example above DEFAULT_CONFIG) would silently rebind the
	// pin to the wrong number with every gate green.
	matches := childWallClockDefaultPattern.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		t.Fatalf("skills/boss-epic/toolbox/skill-config.mjs must declare `childWallClockMinutes: <N>` exactly once, found %d — this gate reads the default from there, and matching nothing or the wrong occurrence would leave the prose figure unpinned", len(matches))
	}
	return string(matches[0][1])
}

func TestBossEpicSkillTrackedChatStatusContractLivesInPollingSection(t *testing.T) {
	skill := readEmbeddedBossEpicSkill(t)
	start := strings.Index(skill, "### 3b. Poll")
	if start == -1 {
		t.Fatalf("heading %q not found", "### 3b. Poll")
	}
	end := strings.Index(skill[start:], "### 3c. Transitions")
	if end == -1 {
		t.Fatalf("heading %q not found", "### 3c. Transitions")
	}
	poll := skill[start : start+end]

	assertContains(t, poll, "`get_chat_statuses {session_id}`")
	assertContains(t, poll, "recorded `chat_id`")
	assertContains(t, poll, "session-level `get_session_statuses` aggregate")
}
