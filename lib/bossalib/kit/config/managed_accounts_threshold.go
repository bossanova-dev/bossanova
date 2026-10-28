package config

import (
	"log/slog"
	"sync"
)

// Load is also called by periodic sweeps; warn once per process, not per read.
var proactive5hThresholdWarning = new(sync.Once)

// Proactive5hThreshold returns the 5h utilization ceiling for account selection.
// Unset and invalid percentages use the default of 90%.
func (c ManagedAgentAccountsConfig) Proactive5hThreshold() float64 {
	if c.Proactive5hThresholdPercent >= 1 && c.Proactive5hThresholdPercent <= 100 {
		return float64(c.Proactive5hThresholdPercent) / 100
	}
	return 0.90
}

// ProactiveRotationEnabled reports whether the proactive pre-cap sweep is on.
// It defaults OFF (nil ⇒ false); the sweep is opt-in only (BOS-318).
func (c ManagedAgentAccountsConfig) ProactiveRotationEnabled() bool {
	return c.ProactiveRotation != nil && *c.ProactiveRotation
}

func warnProactive5hThreshold(percent int) {
	if percent < 0 || percent > 100 {
		proactive5hThresholdWarning.Do(func() {
			slog.Warn("invalid proactive_5h_threshold_percent; using default", "percent", percent, "default_percent", 90)
		})
	}
}
