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
