package db

import (
	"testing"

	"github.com/recurser/bossalib/agenttelemetry"
)

// TestAgentRunTerminalStateCheckAdmitsEveryNormalizedState pins the SQL CHECK on
// agent_runs.terminal_state against the Go normalizer that feeds it. The two are
// separate copies of one allowed-value list, and they fail asymmetrically: adding a
// token to NormalizeAgentRunTerminalState without adding it to the CHECK turns every
// telemetry write carrying that token into a hard "CHECK constraint failed", which
// RecordTelemetry's callers only surface at Warn level -- so the run looks healthy
// while recording nothing. Driving each accepted value through a real write is what
// makes that divergence a red test instead of a silent data loss.
func TestAgentRunTerminalStateCheckAdmitsEveryNormalizedState(t *testing.T) {
	db := setupTestDB(t)
	seedAgentRunFixture(t, db, "run1")

	for _, state := range []string{
		AgentRunTerminalReviewReady,
		AgentRunTerminalPartial,
		AgentRunTerminalBlocked,
		AgentRunTerminalNoChange,
		AgentRunTerminalUnrecorded,
	} {
		if got := NormalizeAgentRunTerminalState(state); got != state {
			t.Fatalf("NormalizeAgentRunTerminalState(%q) = %q; want it preserved -- the constant list and the normalizer disagree", state, got)
		}
		if _, err := db.Exec(`UPDATE agent_runs SET terminal_state = ? WHERE id = 'run1'`, state); err != nil {
			t.Fatalf("SQL CHECK rejected normalized terminal state %q: %v -- add it to the CHECK in the agent_runs review-telemetry migration", state, err)
		}
	}
}

// TestAgentRunTerminalStateCheckRejectsUnnormalizedToken proves the CHECK is
// actually enforced on this column, which is what makes the normalizer load-bearing
// rather than decorative. Without it the test above would pass against a column that
// accepts anything.
func TestAgentRunTerminalStateCheckRejectsUnnormalizedToken(t *testing.T) {
	db := setupTestDB(t)
	seedAgentRunFixture(t, db, "run1")

	const bogus = "NOT_A_TERMINAL_STATE"
	if got := NormalizeAgentRunTerminalState(bogus); got != AgentRunTerminalUnrecorded {
		t.Fatalf("NormalizeAgentRunTerminalState(%q) = %q; want it normalized away to %q", bogus, got, AgentRunTerminalUnrecorded)
	}
	if _, err := db.Exec(`UPDATE agent_runs SET terminal_state = ? WHERE id = 'run1'`, bogus); err == nil {
		t.Fatal("SQL CHECK accepted an unnormalized terminal state; the column constraint is not enforced")
	}
}

// TestAgentTelemetryTerminalStatesMatchStoreConstants pins the third copy of the
// list: the extractor in agenttelemetry produces these tokens, and the store is what
// persists them. A token renamed on one side and not the other normalizes to "" at
// write time, so every run silently reads back as "not recorded".
func TestAgentTelemetryTerminalStatesMatchStoreConstants(t *testing.T) {
	for _, pair := range []struct {
		extractor string
		store     string
	}{
		{agenttelemetry.TerminalStateReviewReady, AgentRunTerminalReviewReady},
		{agenttelemetry.TerminalStatePartial, AgentRunTerminalPartial},
		{agenttelemetry.TerminalStateBlocked, AgentRunTerminalBlocked},
		{agenttelemetry.TerminalStateNoChange, AgentRunTerminalNoChange},
	} {
		if pair.extractor != pair.store {
			t.Fatalf("agenttelemetry token %q does not match store constant %q", pair.extractor, pair.store)
		}
		if got := NormalizeAgentRunTerminalState(pair.extractor); got != pair.extractor {
			t.Fatalf("store normalizer drops extractor token %q (got %q)", pair.extractor, got)
		}
	}
}
