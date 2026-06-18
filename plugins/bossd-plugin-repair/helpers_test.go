package main

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestIsRepairableState pins the safety gate that decides whether
// /boss-repair may fire for a given (state, displayStatus) pair. A
// regression that admits BLOCKED or IMPLEMENTING_PLAN would defeat the
// max-attempts guardrail and re-open the hammer-loop incident that
// motivated the backoff cap, so every state is asserted explicitly.
func TestIsRepairableState(t *testing.T) {
	t.Parallel()

	const failing = bossanovav1.DisplayStatus_DISPLAY_STATUS_FAILING
	const conflict = bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT
	const rejected = bossanovav1.DisplayStatus_DISPLAY_STATUS_REJECTED

	tests := []struct {
		name    string
		state   bossanovav1.SessionState
		display bossanovav1.DisplayStatus
		want    bool
	}{
		{"awaiting_checks repairable", bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS, failing, true},
		{"fixing_checks repairable", bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS, failing, true},
		{"green_draft repairable", bossanovav1.SessionState_SESSION_STATE_GREEN_DRAFT, rejected, true},
		{"ready_for_review repairable", bossanovav1.SessionState_SESSION_STATE_READY_FOR_REVIEW, rejected, true},
		// Finalizing is only repairable for a CONFLICT signal (rebase-and-push).
		{"finalizing with conflict repairable", bossanovav1.SessionState_SESSION_STATE_FINALIZING, conflict, true},
		{"finalizing without conflict not repairable", bossanovav1.SessionState_SESSION_STATE_FINALIZING, failing, false},
		{"finalizing unspecified not repairable", bossanovav1.SessionState_SESSION_STATE_FINALIZING, bossanovav1.DisplayStatus_DISPLAY_STATUS_UNSPECIFIED, false},
		// Blocked is the manual-intervention terminal state — never auto-repair.
		{"blocked not repairable", bossanovav1.SessionState_SESSION_STATE_BLOCKED, failing, false},
		{"blocked with conflict not repairable", bossanovav1.SessionState_SESSION_STATE_BLOCKED, conflict, false},
		// ImplementingPlan is owned by the idle-chat heuristic, not repair.
		{"implementing_plan not repairable", bossanovav1.SessionState_SESSION_STATE_IMPLEMENTING_PLAN, failing, false},
		{"merged not repairable", bossanovav1.SessionState_SESSION_STATE_MERGED, conflict, false},
		{"unspecified not repairable", bossanovav1.SessionState_SESSION_STATE_UNSPECIFIED, failing, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isRepairableState(tt.state, tt.display))
		})
	}
}

// TestIsActionableReviewComment pins which review states feed the
// fingerprint that re-triggers repair on new feedback.
func TestIsActionableReviewComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		state bossanovav1.ReviewState
		want  bool
	}{
		{"changes_requested actionable", bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED, true},
		{"commented actionable", bossanovav1.ReviewState_REVIEW_STATE_COMMENTED, true},
		{"approved not actionable", bossanovav1.ReviewState_REVIEW_STATE_APPROVED, false},
		{"dismissed not actionable", bossanovav1.ReviewState_REVIEW_STATE_DISMISSED, false},
		{"unspecified not actionable", bossanovav1.ReviewState_REVIEW_STATE_UNSPECIFIED, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := &bossanovav1.ReviewComment{State: tt.state}
			assert.Equal(t, tt.want, isActionableReviewComment(c))
		})
	}

	// A nil comment relies on proto getter nil-safety returning the zero
	// (unspecified) state, which must be treated as non-actionable rather
	// than panicking.
	t.Run("nil comment not actionable", func(t *testing.T) {
		t.Parallel()
		assert.False(t, isActionableReviewComment(nil))
	})
}

// TestSessionByID covers the linear lookup used to resolve a session from
// the daemon's snapshot.
func TestSessionByID(t *testing.T) {
	t.Parallel()

	a := &bossanovav1.Session{Id: "a"}
	b := &bossanovav1.Session{Id: "b"}
	sessions := []*bossanovav1.Session{a, b}

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		assert.Same(t, b, sessionByID(sessions, "b"))
	})
	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, sessionByID(sessions, "missing"))
	})
	t.Run("empty slice", func(t *testing.T) {
		t.Parallel()
		assert.Nil(t, sessionByID(nil, "a"))
	})
}

// TestAgentSessionIDFromAlreadyExists exercises the regex that recovers the
// existing agent session id from an AlreadyExists error so repair can
// resume rather than spawn a duplicate.
func TestAgentSessionIDFromAlreadyExists(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil error", nil, ""},
		{"no match", errors.New("some unrelated failure"), ""},
		{"plain match", errors.New("chat already running (agent_session_id=sess-123)"), "sess-123"},
		{"match with trailing text", errors.New("prefix (agent_session_id=abc-def) suffix"), "abc-def"},
		{"empty captured id", errors.New("(agent_session_id=)"), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, agentSessionIDFromAlreadyExists(tt.err))
		})
	}
}

// TestIsInteractiveChatIncomplete pins the substring match used to detect a
// chat run that exited without reporting completion.
func TestIsInteractiveChatIncomplete(t *testing.T) {
	t.Parallel()

	assert.True(t, isInteractiveChatIncomplete("boss: interactive chat did not report completion"))
	assert.False(t, isInteractiveChatIncomplete("exit status 1"))
	assert.False(t, isInteractiveChatIncomplete(""))
}

// TestParseRepairConfig covers the three branches: empty input yields an
// empty (non-nil) config, valid JSON unmarshals, invalid JSON errors.
func TestParseRepairConfig(t *testing.T) {
	t.Parallel()

	t.Run("empty input", func(t *testing.T) {
		t.Parallel()
		cfg, err := parseRepairConfig("")
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, 0, cfg.CooldownMinutes)
	})

	t.Run("valid json", func(t *testing.T) {
		t.Parallel()
		cfg, err := parseRepairConfig(`{"cooldown_minutes":7,"sweep_interval_minutes":3}`)
		require.NoError(t, err)
		assert.Equal(t, 7, cfg.CooldownMinutes)
		assert.Equal(t, 3, cfg.SweepIntervalMinutes)
	})

	t.Run("invalid json", func(t *testing.T) {
		t.Parallel()
		cfg, err := parseRepairConfig(`{not json`)
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
}

// TestRepairConfigDurations pins the default/override behaviour of every
// duration getter, including the nil-receiver fail-safe (the getters are
// called on a possibly-nil config under lock).
func TestRepairConfigDurations(t *testing.T) {
	t.Parallel()

	t.Run("nil receiver returns defaults", func(t *testing.T) {
		t.Parallel()
		var c *repairConfig
		assert.Equal(t, defaultCooldownDuration, c.cooldownDuration())
		assert.Equal(t, defaultSweepInterval, c.sweepInterval())
		assert.Equal(t, defaultStuckTimeout, c.stuckTimeout())
		assert.Equal(t, defaultForceAdvanceTimeout, c.forceAdvanceTimeout())
		assert.Equal(t, defaultIdleRepairThreshold, c.idleRepairThreshold())
		assert.Equal(t, defaultPostRepairPollInterval, c.postRepairPollInterval())
		assert.Equal(t, defaultPostRepairWaitTimeout, c.postRepairWaitTimeout())
	})

	t.Run("zero values fall back to defaults", func(t *testing.T) {
		t.Parallel()
		c := &repairConfig{}
		assert.Equal(t, defaultCooldownDuration, c.cooldownDuration())
		assert.Equal(t, defaultSweepInterval, c.sweepInterval())
		assert.Equal(t, defaultStuckTimeout, c.stuckTimeout())
		assert.Equal(t, defaultForceAdvanceTimeout, c.forceAdvanceTimeout())
		assert.Equal(t, defaultIdleRepairThreshold, c.idleRepairThreshold())
		assert.Equal(t, defaultPostRepairPollInterval, c.postRepairPollInterval())
		assert.Equal(t, defaultPostRepairWaitTimeout, c.postRepairWaitTimeout())
	})

	t.Run("positive overrides applied", func(t *testing.T) {
		t.Parallel()
		c := &repairConfig{
			CooldownMinutes:            2,
			SweepIntervalMinutes:       3,
			StuckTimeoutMinutes:        4,
			ForceAdvanceTimeoutMinutes: 5,
			IdleRepairThresholdMinutes: 6,
			PostRepairPollMilliseconds: 250,
			PostRepairWaitMilliseconds: 7000,
		}
		assert.Equal(t, 2*time.Minute, c.cooldownDuration())
		assert.Equal(t, 3*time.Minute, c.sweepInterval())
		assert.Equal(t, 4*time.Minute, c.stuckTimeout())
		assert.Equal(t, 5*time.Minute, c.forceAdvanceTimeout())
		assert.Equal(t, 6*time.Minute, c.idleRepairThreshold())
		assert.Equal(t, 250*time.Millisecond, c.postRepairPollInterval())
		assert.Equal(t, 7000*time.Millisecond, c.postRepairWaitTimeout())
	})
}
