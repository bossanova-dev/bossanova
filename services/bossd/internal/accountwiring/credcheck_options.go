package accountwiring

import "time"

// CredentialMaintainerOption configures a CredentialMaintainer.
type CredentialMaintainerOption func(*CredentialMaintainer)

// WithAuthCheckInterval sets the periodic sweep cadence (pre-jitter).
func WithAuthCheckInterval(d time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if d > 0 {
			m.interval = d
		}
	}
}

// WithAuthCheckStaleness sets how old a recorded check may be before the
// account is swept again.
func WithAuthCheckStaleness(d time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if d > 0 {
			m.staleness = d
		}
	}
}

// WithAuthCheckEnabled wires the live managed-accounts kill switch consulted
// before each scheduled sweep. Pass a loader that re-reads settings from disk,
// not a captured boot-time boolean; fail closed on a load error, matching
// Lifecycle.currentRotationConfig. It gates only the scheduled path — an
// operator-initiated TestAgentAccount still runs through Smoke with rotation off.
func WithAuthCheckEnabled(fn func() bool) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		m.enabled = fn
	}
}

// WithAuthCheckBootDelay sets the delay before the stale-at-boot sweep.
func WithAuthCheckBootDelay(d time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if d >= 0 {
			m.bootDelay = d
		}
	}
}

// WithAuthCheckBackoff sets the exponential retry backoff base and ceiling
// used after a transient or unavailable verification.
func WithAuthCheckBackoff(base, maxWait time.Duration) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if base > 0 {
			m.backoffBase = base
		}
		if maxWait > 0 {
			m.backoffMax = maxWait
		}
	}
}

// WithAuthCheckJitter sets the jitter fraction (0 disables jitter) and the
// [0,1) random source used to spread scheduled work.
func WithAuthCheckJitter(fraction float64, source func() float64) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if fraction >= 0 && fraction < 1 {
			m.jitter = fraction
		}
		if source != nil {
			m.rand = source
		}
	}
}

// WithAuthCheckClock substitutes the time seam.
func WithAuthCheckClock(c AuthCheckClock) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) {
		if c != nil {
			m.clock = c
		}
	}
}

// WithAuthCheckChanged publishes a metadata inventory invalidation after auth writes.
func WithAuthCheckChanged(changed func()) CredentialMaintainerOption {
	return func(m *CredentialMaintainer) { m.changed = changed }
}
