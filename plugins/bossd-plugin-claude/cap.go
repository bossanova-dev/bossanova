package main

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
)

// classifyUsageCap inspects a PostExit log tail and classifies it into a
// usage-cap or rate-limit typed error, or nil. When the shared agenterr
// taxonomy classifies the tail as KindUsageExhausted it opportunistically
// captures a redacted banner snapshot and returns agenterr.ErrUsageLimited
// carrying the parsed reset time (zero when the banner had no parseable reset).
// When it classifies as KindRateLimited (a 429 / rate-limit banner, BOS-406) it
// captures the banner the same way and returns agenterr.ErrRateLimited (no reset
// — the bossd usage probe resolves it). Banner capture is best-effort; a capture
// failure is logged at Debug and otherwise ignored. It returns nil for every
// other classification.
//
// Auth is intentionally out of scope for claude: the classifier may report
// KindAuthInvalidated, but claude has no auth sentinel and daemon auth handling
// for claude is unowned this epic, so an auth (or any non-usage/non-rate) tail
// returns nil (unchanged exit). now anchors reset-time parsing so callers can
// pass a fixed clock in tests.
func classifyUsageCap(logger zerolog.Logger, tail []byte, now time.Time) error {
	c := agenterr.Classify(string(tail), now)
	switch c.Kind {
	case agenterr.KindUsageExhausted:
		captureBanner(logger, c.Kind, tail)
		var reset time.Time
		if c.ResetAt != nil {
			reset = *c.ResetAt
		}
		return agenterr.ErrUsageLimited{ResetAt: reset}
	case agenterr.KindRateLimited:
		captureBanner(logger, c.Kind, tail)
		return agenterr.ErrRateLimited{}
	default:
		return nil
	}
}

// captureBanner is the shared best-effort redacted-banner capture used for both
// the usage-cap and rate-limit classifications. A capture failure is logged at
// Debug and otherwise ignored.
func captureBanner(logger zerolog.Logger, kind agenterr.Kind, tail []byte) {
	if path, err := agenterr.CaptureBanner(kind, tail); err != nil {
		logger.Debug().Err(err).Str("kind", kind.String()).Msg("capture banner failed")
	} else {
		logger.Debug().Str("path", path).Str("kind", kind.String()).Msg("captured banner")
	}
}
