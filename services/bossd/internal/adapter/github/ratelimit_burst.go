package github

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

const (
	// rateLimitBurstPause is the first pause after a per-minute or secondary
	// refusal. GitHub documents "wait at least one minute" for a refusal with
	// no retry-after, and gh never exposes the headers on failure.
	rateLimitBurstPause = 60 * time.Second
	// rateLimitBurstJitter bounds the random delay added to a burst pause, so
	// the callers it releases do not all hit GitHub in the same second.
	rateLimitBurstJitter = 15 * time.Second
)

// WithJitterFunc overrides the source of burst-pause jitter: f returns a
// duration in [0, max). Tests pin it to zero.
func WithJitterFunc(f func(max time.Duration) time.Duration) ProviderOption {
	return func(p *Provider) { p.limits.jitter = f }
}

func defaultJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return rand.N(max) // #nosec G404 -- pause jitter, not a secret
}

// isSecondaryRateLimit reports whether err is GitHub's secondary (abuse) rate
// limit, which its wording names unambiguously.
func isSecondaryRateLimit(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(ghResponseText(err)), "secondary rate limit")
}

// isBurstRefusal reports whether err is a short per-minute or secondary rate
// limit, which the transient retry ladder may wait out: a burst
// rateLimitedError, or a raw secondary-limit refusal.
func isBurstRefusal(err error) bool {
	var limited *rateLimitedError
	if errors.As(err, &limited) {
		return limited.burst
	}
	return isSecondaryRateLimit(err)
}

// burstPause is the pause for the streak-th consecutive burst refusal:
// rateLimitBurstPause doubled per earlier refusal, plus jitter, never longer
// than rateLimitFallbackPause.
func burstPause(streak int, jitter time.Duration) time.Duration {
	pause := rateLimitBurstPause
	for i := 1; i < streak && pause < rateLimitFallbackPause; i++ {
		pause *= 2
	}
	return min(pause+jitter, rateLimitFallbackPause)
}

// openBurst pauses quota for a burst refusal and returns the window. A pause
// that is already open is returned unchanged, so concurrent refusals count
// once. Otherwise the quota's streak grows and the pause doubles with it,
// which still backs off to rateLimitFallbackPause when the rate_limit endpoint
// lags what GitHub enforces. res is the reading that judged the refusal, nil
// for a secondary limit.
func (b *rateLimitBreaker) openBurst(pool quotaPool, res *quotaStatus) pauseWindow {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if w, ok := b.open[pool]; ok && now.Before(w.until) {
		return w
	}
	if b.open == nil {
		b.open = make(map[quotaPool]pauseWindow)
	}
	if b.streak == nil {
		b.streak = make(map[quotaPool]int)
	}
	b.streak[pool]++
	jitter := b.jitter
	if jitter == nil {
		jitter = defaultJitter
	}
	pause := burstPause(b.streak[pool], jitter(rateLimitBurstJitter))
	w := pauseWindow{until: now.Add(pause), burst: true}
	b.open[pool] = w
	event := b.logger.Warn().
		Str("quota", pool.quota).
		Str("identity", pool.identity).
		Dur("pause", pause.Round(time.Second)).
		Int("streak", b.streak[pool])
	if res != nil {
		event = event.Int("remaining", res.Remaining).Int("limit", res.limitOrDefault())
	}
	event.Msg("github per-minute rate limit hit; pausing calls briefly")
	return w
}

// succeeded records a successful call to pool, which ends its burst streak.
func (b *rateLimitBreaker) succeeded(pool quotaPool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.streak, pool)
}

// burstMessage is a burst rateLimitedError's text. It names the per-minute
// limit, never an exhausted quota, so log triage can tell the two apart.
func (e *rateLimitedError) burstMessage() string {
	if e.cause != nil {
		return fmt.Sprintf("%v (GitHub %s per-minute rate limit hit for %s; pausing %s calls briefly, until %s)",
			e.cause, e.quota, e.identityName(), e.quota, e.resetAt.Format(time.RFC3339))
	}
	return fmt.Sprintf("GitHub %s per-minute rate limit hit for %s; call skipped until %s",
		e.quota, e.identityName(), e.resetAt.Format(time.RFC3339))
}
