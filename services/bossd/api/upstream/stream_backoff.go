package upstream

import (
	"math/rand/v2"
	"time"
)

const streamRedialAfter = 3

func (c *StreamClient) attemptProven() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.upstreamProven
}

func resetStreamBackoff(backoff time.Duration, hotRetry, proven bool) time.Duration {
	if hotRetry || proven {
		return streamInitialBackoff
	}
	return backoff
}

func (c *StreamClient) reconnectWait(backoff time.Duration) time.Duration {
	ceiling := min(backoff, streamMaxBackoff)
	wait := time.Duration(0)
	if c.jitter != nil {
		wait = c.jitter(ceiling)
	} else {
		// #nosec G404 -- retry timing jitter, not a secret; owner=@recurser review-by=2027-02-18 issue=BOS-2573
		wait = ceiling/2 + time.Duration(rand.Int64N(int64(ceiling/2)+1))
	}
	return max(streamInitialBackoff, min(wait, ceiling))
}

func (c *StreamClient) redialAfterAttempt(failures int, proven bool) int {
	if proven {
		return 0
	}
	failures++
	if failures == streamRedialAfter {
		if c.redialer != nil {
			c.redialer.Redial()
			c.logger.Info().Int("failed_attempts", failures).Msg("stream: re-dialling upstream after 3 failed attempts")
		}
		return 0
	}
	return failures
}
