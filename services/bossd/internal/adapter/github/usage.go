package github

import (
	"context"
	"errors"

	"github.com/recurser/bossd/internal/ghusage"
)

// recordSent meters one call gh is about to send to GitHub from pool.
func (p *Provider) recordSent(ctx context.Context, pool quotaPool) {
	if p.usage != nil {
		p.usage.Record(ghusage.LabelFrom(ctx), pool.quota, pool.identity)
	}
}

// recordSkip meters one call gh refused without contacting GitHub against
// pool, classified by skipOutcome.
func (p *Provider) recordSkip(ctx context.Context, pool quotaPool, refusal error) {
	if p.usage != nil {
		p.usage.RecordSkip(ghusage.LabelFrom(ctx), pool.quota, pool.identity, skipOutcome(refusal))
	}
}

// skipOutcome reports which gate refused a call: the background floor when
// the refusal carries a shortfall, otherwise the rate-limit breaker.
func skipOutcome(refusal error) string {
	var rl *rateLimitedError
	if errors.As(refusal, &rl) && rl.belowFloor != nil {
		return ghusage.OutcomeFloor
	}
	return ghusage.OutcomeBreaker
}
