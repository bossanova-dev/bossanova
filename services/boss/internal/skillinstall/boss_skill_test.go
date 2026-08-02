package skillinstall

import (
	"strings"
	"testing"
)

// TestBossSkillReferencesHaveNoOrphans asserts that every references/*.md the
// boss core SHIPS is reachable: it must be named by a row of the SKILL.md index
// table, which after BOS-637 is the only route an agent has to a command's
// syntax. An unreferenced reference file is dead weight the payload carries into
// every repo the published core installs into, and — worse — a command group
// silently missing from the index reads as a command group that does not exist.
//
// Only this direction is implemented here. The opposite one (SKILL.md names a
// references/<file>.md the payload does not ship) is already gated across both
// shipped payloads by TestPublishedCoresShipTheReferencesTheyName in
// skills_manifest_test.go; re-implementing it here would add a second copy of an
// invariant that already has a non-vacuity proof.
//
// The payload is read through SkillsFS rather than off the filesystem so this
// gates the bytes that actually get embedded into the binary and extracted into
// a user's skill directory.
func TestBossSkillReferencesHaveNoOrphans(t *testing.T) {
	skill := readEmbeddedBossSkill(t)

	entries, err := SkillsFS.ReadDir("skills/boss/references")
	if err != nil {
		t.Fatalf("read embedded skills/boss/references: %v", err)
	}

	shipped := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		shipped++
		ref := "`references/" + entry.Name() + "`"
		if !strings.Contains(skill, ref) {
			t.Errorf("skills/boss/references/%s is an orphan: SKILL.md's index table has no %s row, "+
				"so nothing routes an agent to it — add the row or delete the file", entry.Name(), ref)
		}
	}
	if shipped == 0 {
		t.Fatalf("skills/boss/references ships no .md files — this test would be vacuous")
	}
}

// TestBossSkillDocumentsBroadcasts pins the broadcast documentation to the skill payload agents
// actually read. Both surfaces exist in code either way, so a refactor that drops this section
// would not fail any build — it would silently un-document the primitive, leaving agents unable
// to discover it.
//
// The rule assertions are short LITERAL anchors, not whole sentences: they survive re-punctuation
// and rewrapping of the surrounding prose, but rewording an anchor itself ("body is a secret" ->
// "the body is secret") does fail. That is the intended trade — these five phrases are the rules
// the ticket requires be stated, so a rewrite that loses one should be a deliberate edit to this
// list rather than a silent drift.
func TestBossSkillDocumentsBroadcasts(t *testing.T) {
	skill := readEmbeddedBossSkill(t)

	// The two names the ticket requires: one per surface.
	assertContains(t, skill, "boss broadcast")
	assertContains(t, skill, "send_broadcast")

	prose := bossBroadcastProseSection(t, skill)

	// The six selector dimensions and the AND/OR combinators.
	for _, dimension := range []string{"chat:", "session:", "repo:", "agent:", "account:", "daemon:"} {
		assertContains(t, prose, dimension)
	}
	assertContains(t, prose, "AND")
	assertContains(t, prose, "OR")

	// Subscriptions and their triggers.
	assertContains(t, prose, "boss broadcast subscribe")
	for _, trigger := range []string{"completed", "errored", "settled"} {
		assertContains(t, prose, trigger)
	}

	// The five rules an agent must internalise.
	for _, rule := range []string{
		"signal, not proof",
		"body is a secret",
		"audience is resolved once",
		"wakes target chats",
		"empty selector is an error",
	} {
		assertContains(t, prose, rule)
	}

	// All six MCP tools, so an MCP-aware host learns the whole surface, not just send.
	for _, tool := range []string{
		"send_broadcast",
		"list_broadcasts",
		"delete_broadcast",
		"register_broadcast_subscription",
		"list_broadcast_subscriptions",
		"delete_broadcast_subscription",
	} {
		assertContains(t, prose, tool)
	}
}

// bossBroadcastProseSection returns the hand-written broadcast section. Its heading is
// deliberately NOT "## Broadcasts": that is the generated CLI reference group earlier in the file,
// and a prose section sharing the name would make these assertions pass against generated command
// help rather than the prose they are meant to pin.
//
// The invariant it actually enforces is positional — the prose must sit AFTER the generated
// region's end marker, because `make gen-skill` rewrites everything between the markers and would
// silently discard a section written inside them. Asserting that directly (rather than counting
// headings) keeps the failure message honest if the generated command group is ever renamed.
func bossBroadcastProseSection(t *testing.T, skill string) string {
	t.Helper()

	const heading = "\n## Broadcasting to other chats\n"
	const endMarker = "<!-- END GENERATED -->"

	generatedEnd := strings.Index(skill, endMarker)
	if generatedEnd == -1 {
		t.Fatalf("marker %q not found in the boss skill", endMarker)
	}
	start := strings.LastIndex(skill, heading)
	if start == -1 {
		t.Fatalf("heading %q not found in the boss skill", strings.TrimSpace(heading))
	}
	if start < generatedEnd {
		t.Fatalf("the %q prose section sits inside the generated region (heading at byte %d, region ends at %d): `make gen-skill` would discard it", strings.TrimSpace(heading), start, generatedEnd)
	}

	prose := skill[start+len(heading):]
	if end := strings.Index(prose, "\n## "); end != -1 {
		prose = prose[:end]
	}
	return prose
}

func readEmbeddedBossSkill(t *testing.T) string {
	t.Helper()

	skillBytes, err := SkillsFS.ReadFile("skills/boss/SKILL.md")
	if err != nil {
		t.Fatalf("read embedded boss skill: %v", err)
	}
	return string(skillBytes)
}
