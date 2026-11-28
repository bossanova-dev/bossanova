package main

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	// defaultCooldownDuration is the minimum time between repair attempts
	// for the same session. The actual wait between attempts is then
	// extended by exponential backoff via cooldownFor — see that helper
	// for the schedule. This default is the base of the exponent and the
	// post-success / post-restart cooldown.
	defaultCooldownDuration = 1 * time.Minute
	// defaultSweepInterval is how often the periodic sweep runs.
	defaultSweepInterval = 1 * time.Minute
	// defaultStuckTimeout is how long a session must be in ImplementingPlan
	// before it is considered stuck and eligible for automatic advancement.
	defaultStuckTimeout = 5 * time.Minute
	// defaultForceAdvanceTimeout is how long a session with an active chat
	// must be stuck before force-advancing (zombie/idle chat suspected).
	defaultForceAdvanceTimeout = 1 * time.Hour
	// defaultIdleRepairThreshold is how long a session's chat must have been
	// quiet (no new output) before auto-repair is allowed to proceed despite
	// the chat process still being attached. Default is 5 minutes — the user
	// may have stepped away; we don't want to wait forever.
	defaultIdleRepairThreshold = 5 * time.Minute

	defaultPostRepairPollInterval = 2 * time.Second
	defaultPostRepairWaitTimeout  = 30 * time.Minute
	defaultMaxConcurrentRepairs   = 3
)

// repairConfig holds parsed config for a repair workflow. Fields mirror
// config.RepairConfig but are local to the plugin to avoid importing
// the config package.
type repairConfig struct {
	Skills                     repairSkillOverrides `json:"skills,omitempty"`
	CooldownMinutes            int                  `json:"cooldown_minutes,omitempty"`
	PollIntervalSeconds        int                  `json:"poll_interval_seconds,omitempty"`
	SweepIntervalMinutes       int                  `json:"sweep_interval_minutes,omitempty"`
	StuckTimeoutMinutes        int                  `json:"stuck_timeout_minutes,omitempty"`
	ForceAdvanceTimeoutMinutes int                  `json:"force_advance_timeout_minutes,omitempty"`
	IdleRepairThresholdMinutes int                  `json:"idle_repair_threshold_minutes,omitempty"`
	PostRepairPollMilliseconds int                  `json:"post_repair_poll_milliseconds,omitempty"`
	PostRepairWaitMilliseconds int                  `json:"post_repair_wait_milliseconds,omitempty"`
	MaxConcurrentRepairs       int                  `json:"max_concurrent_repairs,omitempty"`
}

type repairSkillOverrides struct {
	Repair string `json:"repair,omitempty"`
}

func (c *repairConfig) cooldownDuration() time.Duration {
	if c != nil && c.CooldownMinutes > 0 {
		return time.Duration(c.CooldownMinutes) * time.Minute
	}
	return defaultCooldownDuration
}

func (c *repairConfig) sweepInterval() time.Duration {
	if c != nil && c.SweepIntervalMinutes > 0 {
		return time.Duration(c.SweepIntervalMinutes) * time.Minute
	}
	return defaultSweepInterval
}

func (c *repairConfig) stuckTimeout() time.Duration {
	if c != nil && c.StuckTimeoutMinutes > 0 {
		return time.Duration(c.StuckTimeoutMinutes) * time.Minute
	}
	return defaultStuckTimeout
}

func (c *repairConfig) forceAdvanceTimeout() time.Duration {
	if c != nil && c.ForceAdvanceTimeoutMinutes > 0 {
		return time.Duration(c.ForceAdvanceTimeoutMinutes) * time.Minute
	}
	return defaultForceAdvanceTimeout
}

func (c *repairConfig) idleRepairThreshold() time.Duration {
	if c != nil && c.IdleRepairThresholdMinutes > 0 {
		return time.Duration(c.IdleRepairThresholdMinutes) * time.Minute
	}
	return defaultIdleRepairThreshold
}

func (c *repairConfig) postRepairPollInterval() time.Duration {
	if c != nil && c.PostRepairPollMilliseconds > 0 {
		return time.Duration(c.PostRepairPollMilliseconds) * time.Millisecond
	}
	return defaultPostRepairPollInterval
}

func (c *repairConfig) postRepairWaitTimeout() time.Duration {
	if c != nil && c.PostRepairWaitMilliseconds > 0 {
		return time.Duration(c.PostRepairWaitMilliseconds) * time.Millisecond
	}
	return defaultPostRepairWaitTimeout
}

func (c *repairConfig) maxConcurrentRepairs() int {
	if c != nil && c.MaxConcurrentRepairs > 0 {
		return c.MaxConcurrentRepairs
	}
	return defaultMaxConcurrentRepairs
}

func parseRepairConfig(configJSON string) (*repairConfig, error) {
	cfg := &repairConfig{}
	if configJSON == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(configJSON), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}
