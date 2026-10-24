package statusdetect

import (
	"bytes"
	"regexp"
)

// apiErrorBannerRe matches Claude Code's end-of-turn API-failure banner as a
// WHOLE LINE and captures its payload, e.g.
//
//	"API Error: 502 Bad Gateway"
//	"  ⎿  API Error: 503 Service Unavailable"
//	"API Error (502 Bad Gateway): <html>…"
//
// It is applied to an individual pane row that has already had its leading
// whitespace and UI-chrome glyphs (⏺ ✗ ✘ ⎿ ×, the markers Claude Code renders in
// the left gutter) trimmed, and anchors "API Error" at position 0 of what
// remains. That whole-line anchoring is the entire safety story: when the agent
// QUOTES the banner in prose — `⏺ The log shows "API Error: 502 Bad Gateway" so
// I will retry.` — the row's first content is "The log shows", so it cannot
// match. A looser substring search would flag the agent merely talking about a
// failure it already recovered from, which is exactly the false auto-resume this
// detector must never cause.
//
// Two payload shapes are accepted, and BOTH are folded into the text handed to
// transientAPIPayloadRe:
//
//   - capture 1: the parenthesised form "API Error (502 Bad Gateway): …". This
//     form is accepted deliberately — the CLI renders it when the upstream
//     returns an HTML error body, where the status lives in the parens and the
//     colon payload is markup — even though the MAD-653 real capture is the
//     plain form.
//   - capture 2: everything after the colon ("502 Bad Gateway").
//
// A colon after the banner label is REQUIRED, so a prose sentence starting with
// "API Errors are usually transient" cannot match.
var apiErrorBannerRe = regexp.MustCompile(`(?i)^api error[ \t]*(?:\(([^)\n]*)\))?[ \t]*:(.*)$`)

// leadingStatusRe extracts the HTTP status code from an API-error payload at the
// ONE position the banner actually puts it: the very front of the parenthesised
// group, or immediately after the colon.
//
// Positional anchoring is load-bearing, not stylistic. The CLI appends the raw
// upstream JSON body to the banner, and a 4xx body routinely contains
// 5xx-SHAPED numbers that have nothing to do with the status — a rate-limit
// response reads
//
//	API Error: 429 {"type":"error","error":{"type":"rate_limit_error",
//	  "message":"…limit of 500,000 input tokens per minute…"}}
//
// where a whole-payload search for `\b5[0-9]{2}\b` matches the `500` of
// "500,000" and classifies a rate-limited chat as transient. That is the single
// most costly false positive this detector can make: 429 is owned by the
// usage-limit lane (DetectUsageLimit), which anchors on the rendered cap banner
// and so does NOT mark this pane LIMITED — nothing downstream would catch it,
// and the resumer would nudge a chat that is capped, not broken.
//
// So a status code, wherever one is present, DECIDES the banner on its own.
var leadingStatusRe = regexp.MustCompile(`^[ \t]*([0-9]{3})\b`)

// transientAPIPhraseRe matches the prose the gateway/overload responses carry,
// for the banner shapes that name no status code at all ("API Error:
// Overloaded"). It is consulted only as a FALLBACK — never against a payload
// whose status code already decided the question — so it can never overturn an
// explicit 4xx.
//
// "overloaded" covers both the human phrasing and the API's "overloaded_error"
// type token, which is how a 529 body reads.
//
// The connection-loss arm is BOS-889: a bossd restart tore down the in-process
// failover proxy every agent routes through, and each severed stream ended on
// "API Error: Connection lost mid-response. The response above may be
// incomplete." — no status code, none of the gateway prose, so the auto-resume
// lane never armed for any of the three stalled chats. A dropped connection is
// the same retry class as a 5xx: the request never completed, and resuming the
// session is exactly the right recovery (verified by hand, 3/3, during the
// investigation). The arm stays narrow and claude-shaped per the NOTE on
// IsTransientAPIError — "reset"/"closed" are the sibling wordings of the same
// transport failure, and nothing here is generalised to codex without a real
// captured fixture from that runner.
var transientAPIPhraseRe = regexp.MustCompile(`(?i)bad gateway|service unavailable|overloaded|internal server error|connection lost|connection reset|connection closed`)

// bannerChromeGlyphs are the left-gutter glyphs Claude Code renders before a
// banner row (assistant marker ⏺, tool-result connector ⎿, and the failure
// marks). They are stripped, along with whitespace, before the whole-line
// anchor is applied so a banner nested under a tool result still matches.
const bannerChromeGlyphs = " \t⏺⎿✗✘×•·"

// transientTailLines is how many trailing (stripped) lines to inspect for the
// transient-API-failure banner. It matches login.go's loginTailLines because the
// two banners render in the SAME screen region — above the composer, at the
// bottom of the pane — and 20 is the window already proven in production there.
//
// The window must clear all the chrome a real `capture-pane -p -S -1000` puts
// BELOW the banner: a blank row, the three-row composer box, and the
// shortcut/context footer, plus whatever blank pane rows sit under the cursor.
// A window sized to the banner alone would make the detector silently never fire
// in production, which is the expensive failure here — a missed detection is
// invisible, whereas the stale-history risk a wider window admits is already
// covered downstream (the resumer re-checks the marker, IDLE, and a settle
// window before it delivers anything).
const transientTailLines = 20

// IsTransientAPIError reports whether the captured pane content ends on Claude
// Code's transient API-failure banner — a 5xx / gateway-overload response that
// aborted the turn and is safe to retry by resuming the session.
//
// It fires only on the narrow whole-line banner shape within the last few lines
// (see transientTailLines), so a quoted mention in agent prose, a banner buried
// in scrollback behind later work, a 4xx, the login-required banner, and the
// usage-limit banner all read as false. Like the other detectors here it fails
// toward NOT flagging: a missed transient failure costs one manual resume,
// whereas a false one resumes a session that is legitimately blocked on
// credentials, a usage cap, or the user.
//
// NOTE: the shapes here are claude-banner-shaped FIRST. Codex (and other agent
// runners) render their API failures with different chrome and wording; do not
// speculatively widen the patterns for them — add shapes only when a real
// captured fixture from that runner exists to pin them down.
func IsTransientAPIError(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	clean := StripANSI(data)
	if len(clean) == 0 {
		return false
	}
	for line := range bytes.SplitSeq(LastNLines(clean, transientTailLines), []byte("\n")) {
		row := bytes.Trim(line, bannerChromeGlyphs)
		m := apiErrorBannerRe.FindSubmatch(row)
		if m == nil {
			continue
		}
		// m[1] is the parenthesised status (if any), m[2] the post-colon payload;
		// either may carry the signal.
		if bannerIsTransient(m[1], m[2]) {
			return true
		}
	}
	return false
}

// bannerIsTransient decides a single API-error banner from its parenthesised
// status group and its post-colon payload.
//
// A status code found at the FRONT of either payload is authoritative and
// decides alone — 5xx is retryable, everything else is not. 4xx is excluded
// deliberately: 401/403 are credential failures owned by the rotation lane, 400
// is a malformed request that will fail identically on retry, and 429 is rate
// limiting owned by the usage-limit lane. Resuming on any of those would either
// loop forever or fight another lane for the same session.
//
// Any 5xx numeric counts, not just the 500/502/503/504/529 the spec enumerates:
// the set of server-side statuses the API and its edge can emit is open-ended
// and they are uniformly "the server failed, the request is safe to retry".
//
// Only when NEITHER payload names a status code does the prose fallback apply,
// so a body quoting 5xx-shaped prose can never overturn an explicit 4xx.
func bannerIsTransient(paren, rest []byte) bool {
	for _, payload := range [][]byte{paren, rest} {
		if m := leadingStatusRe.FindSubmatch(payload); m != nil {
			return m[1][0] == '5'
		}
	}
	return transientAPIPhraseRe.Match(paren) || transientAPIPhraseRe.Match(rest)
}
