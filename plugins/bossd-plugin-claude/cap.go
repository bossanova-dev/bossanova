package main

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
)

// classifyUsageCap inspects a PostExit log tail for a provider usage-cap
// banner. When the shared agenterr taxonomy classifies the tail as
// KindUsageExhausted it opportunistically captures a redacted banner snapshot
// (best-effort; a capture failure is logged at Debug and otherwise ignored) and
// returns agenterr.ErrUsageLimited carrying the parsed reset time (zero when the
// banner had no parseable reset). It returns nil for every other
// classification.
//
// Auth is intentionally out of scope for claude: the classifier may report
// KindAuthInvalidated, but claude has no auth sentinel and daemon auth handling
// for claude is unowned this epic, so an auth (or any non-usage) tail returns
// nil (unchanged exit). now anchors reset-time parsing so callers can pass a
// fixed clock in tests.
func classifyUsageCap(logger zerolog.Logger, tail []byte, now time.Time) error {
	c := agenterr.Classify(string(tail), now)
	if c.Kind != agenterr.KindUsageExhausted {
		return nil
	}
	if path, err := agenterr.CaptureBanner(c.Kind, tail); err != nil {
		logger.Debug().Err(err).Msg("capture usage-cap banner failed")
	} else {
		logger.Debug().Str("path", path).Msg("captured usage-cap banner")
	}
	var reset time.Time
	if c.ResetAt != nil {
		reset = *c.ResetAt
	}
	return agenterr.ErrUsageLimited{ResetAt: reset}
}
