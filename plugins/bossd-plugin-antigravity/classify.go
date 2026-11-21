package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agenterr"
)

// ErrAuthRequired reports that agy has no usable login.
var ErrAuthRequired authErr = "antigravity auth required: run `agy` to log in"

type authErr string

func (e authErr) Error() string { return string(e) }

var authMarkers = []string{
	"authentication required",
	"authentication failed or timed out",
}

// usageMarkers are agy's own quota-exhaustion messages, matched beside the
// shared agenterr usage grammar.
var usageMarkers = []string{
	"individual quota reached",
	"ai credits balance is too low",
}

// rateLimitMarkers are retryable capacity or throttling signals: the server is
// busy or the request rate is too high, but the account still has quota.
var rateLimitMarkers = []string{
	"model_capacity_exhausted",
	"resource_exhausted",
}

// classifyExit upgrades a failed run's generic exit error into a typed
// sentinel. Auth wins over a co-occurring cap, and a clean exit is never
// reclassified.
func classifyExit(logger zerolog.Logger, orig error, tail []byte, now time.Time) error {
	if orig == nil {
		return nil
	}
	texts := failureTexts(tail)
	for _, text := range texts {
		if containsAny(text, authMarkers) {
			return ErrAuthRequired
		}
	}
	for _, text := range texts {
		if containsAny(text, usageMarkers) || agenterr.Classify(text, now).Kind == agenterr.KindUsageExhausted {
			captureBanner(logger, agenterr.KindUsageExhausted, tail)
			var reset time.Time
			if at, ok := agenterr.ParseResetTime(text, now); ok {
				reset = at
			}
			return agenterr.ErrUsageLimited{ResetAt: reset}
		}
	}
	for _, text := range texts {
		if agenterr.Classify(text, now).Kind == agenterr.KindRateLimited || containsAny(text, rateLimitMarkers) {
			captureBanner(logger, agenterr.KindRateLimited, tail)
			return agenterr.ErrRateLimited{}
		}
	}
	return nil
}

// failureTexts extracts what agy itself said about a failure from the run log:
// stderr lines and the `error` of result events. Assistant and tool events are
// skipped because a reply that merely discusses quotas or logins must not
// classify an unrelated failure.
func failureTexts(tail []byte) []string {
	var texts []string
	for line := range bytes.SplitSeq(tail, []byte{'\n'}) {
		var entry struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(bytes.TrimSpace(line), &entry) != nil || entry.Text == "" {
			continue
		}
		if strings.HasPrefix(entry.Text, "[runner]") {
			continue
		}
		if ev, ok := parseStreamEvent(entry.Text); ok {
			if ev.Event == "result" && ev.Result != nil && ev.Result.Error != "" {
				texts = append(texts, ev.Result.Error)
			}
			continue
		}
		texts = append(texts, entry.Text)
	}
	return texts
}

func containsAny(text string, markers []string) bool {
	lower := strings.ToLower(text)
	for _, m := range markers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

func captureBanner(logger zerolog.Logger, kind agenterr.Kind, tail []byte) {
	if path, err := agenterr.CaptureBanner(kind, tail); err != nil {
		logger.Debug().Err(err).Str("kind", kind.String()).Msg("capture cap banner failed")
	} else {
		logger.Debug().Str("path", path).Str("kind", kind.String()).Msg("captured cap banner")
	}
}
