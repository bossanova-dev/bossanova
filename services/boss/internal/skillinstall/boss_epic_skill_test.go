package skillinstall

import (
	"os"
	"path/filepath"
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
