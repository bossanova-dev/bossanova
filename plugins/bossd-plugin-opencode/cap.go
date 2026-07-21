package main

import (
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/agenterr"
)

// classifyUsageCap inspects a PostExit log tail for a provider usage-cap or
// rate-limit signal and returns the matching typed agenterr sentinel, or nil.
// It returns nil for every other classification — including auth and transient
// failures — so a non-matching tail never manufactures a cap. now anchors
// reset-time parsing so callers can pass a fixed clock in tests.
//
// The usage-exhaustion vs rate-limit split mirrors the canonical claude twin
// (plugins/bossd-plugin-claude/cap.go, BOS-406) and the agenterr taxonomy's own
// distinction, rather than collapsing both into one sentinel:
//
//   - KindUsageExhausted → agenterr.ErrUsageLimited carrying the parsed reset
//     time (zero when none was parseable).
//   - KindRateLimited (a retryable 429) → agenterr.ErrRateLimited, which carries
//     no reset: a rate limit's reset is resolved authoritatively by the bossd
//     usage probe, not a banner.
//
// A cap is classified ONLY from opencode's structured `error` events, never the
// free text of the tail. opencode emits caps as `error` events with a
// `{statusCode, message}` (unlike codex, whose free-text stderr banner the twin
// scans wholesale), so the message/status IS the authoritative signal. Scanning
// arbitrary tail text would be unsafe here: the run prompt rides argv and is
// echoed into the spawn-preamble log, and opencode's own assistant/tool events
// can quote the task, so a plan or reply mentioning a token like
// `usage_limit_reached` must never manufacture a cap on an unrelated failure.
//
//   - A usage banner in any error message → ErrUsageLimited (preserving any
//     parsed reset), regardless of status code, and it wins over a rate limit.
//   - A structured 429 whose message is not a usage banner → the retryable
//     ErrRateLimited — a transient throttle must not be mislabeled as usage
//     exhaustion.
//
// Auth precedence lives in the runner's PostExit closure (detectAuthFailure is
// checked first); this helper only ever produces the usage-cap / rate-limit
// upgrade.
func classifyUsageCap(logger zerolog.Logger, tail []byte, now time.Time) error {
	events := scanErrorEvents(tail)
	// A usage banner in any error message wins regardless of status code.
	for _, ev := range events {
		if c := agenterr.Classify(ev.Message, now); c.Kind == agenterr.KindUsageExhausted {
			return usageLimited(logger, c.ResetAt, tail)
		}
	}
	// A structured 429 with no usage banner is a retryable rate limit.
	for _, ev := range events {
		if ev.StatusCode != 429 {
			continue
		}
		switch agenterr.Classify(ev.Message, now).Kind {
		case agenterr.KindRateLimited, agenterr.KindNone:
			return rateLimited(logger, tail)
		case agenterr.KindAuthInvalidated:
			// Auth is handled first in PostExit; never manufacture a cap here.
		default:
			// Any future kind: leave it to the taxonomy, surface no cap.
		}
	}
	return nil
}

// usageLimited builds the ErrUsageLimited sentinel, opportunistically capturing a
// redacted banner snapshot (best-effort; a capture failure is logged at Debug and
// otherwise ignored). resetAt is nil when no reset time was parsed, mapping to a
// zero ResetAt.
func usageLimited(logger zerolog.Logger, resetAt *time.Time, tail []byte) error {
	captureBanner(logger, agenterr.KindUsageExhausted, tail)
	var reset time.Time
	if resetAt != nil {
		reset = *resetAt
	}
	return agenterr.ErrUsageLimited{ResetAt: reset}
}

// rateLimited builds the ErrRateLimited sentinel for a retryable 429. Unlike
// ErrUsageLimited it carries no reset time — a rate limit's reset is resolved
// authoritatively by the bossd usage probe (BOS-406), not parsed from the banner.
// The redacted banner snapshot is captured best-effort under the KindRateLimited
// label so the on-disk artifact matches the classification.
func rateLimited(logger zerolog.Logger, tail []byte) error {
	captureBanner(logger, agenterr.KindRateLimited, tail)
	return agenterr.ErrRateLimited{}
}

// captureBanner is the shared best-effort redacted-banner capture used for both
// the usage-cap and rate-limit classifications. A capture failure is logged at
// Debug and otherwise ignored.
func captureBanner(logger zerolog.Logger, kind agenterr.Kind, tail []byte) {
	if path, err := agenterr.CaptureBanner(kind, tail); err != nil {
		logger.Debug().Err(err).Str("kind", kind.String()).Msg("capture cap banner failed")
	} else {
		logger.Debug().Str("path", path).Str("kind", kind.String()).Msg("captured cap banner")
	}
}
