package credmaterialize

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// errIncomingUnusable marks a merge that failed because the INCOMING blob (next)
// is not a usable credential object — as distinct from a failure attributable to
// the stored blob or to marshalling. Callers that read next off disk use it to
// tell "the agent refreshed the credential" apart from "the file is garbage":
// garbage holds no refreshed secret worth preserving, so the materialize-time
// reconcile skips it and lets the normal write replace the file. Aborting there
// instead would fail that materialization and every later one identically, since
// nothing else ever rewrites auth.json.
var errIncomingUnusable = errors.New("incoming credential blob is not a usable credential object")

// mergePreservingIDToken overlays the freshly-written codex credential blob
// (next) onto the previously-stored blob (prev), preserving fields that a
// partial refresh may have dropped. It is ported in spirit from lumi's
// mergePreservingIdToken.
//
// Rules:
//   - Both blobs are parsed as top-level JSON objects so UNKNOWN top-level
//     fields present only in prev survive the merge.
//   - next is normalized to codex shape: a top-level "tokens" object. An
//     opencode-ish "{openai:{...}}" shape is rewritten to "{tokens:{...}}", and
//     the account-store top-level "{access,refresh,id_token}" shape is mirrored
//     into "tokens" (under codex field names) so its secrets take part in the
//     fallback below. id_token is NEVER lost regardless of the source shape.
//   - next wins at the top level for keys it defines; prev-only keys survive.
//   - Within "tokens": next overlays prev, but id_token, access_token,
//     refresh_token and account_id fall back to prev when absent or empty in
//     next. id_token is NEVER lost.
//
// The returned bytes are compact JSON. Errors reference the failure context but
// NEVER include blob contents (the blobs are credentials). A failure caused by
// next being unusable wraps errIncomingUnusable, so a caller that read next off
// disk can distinguish it from a stored-blob or marshalling failure.
func mergePreservingIDToken(prev, next []byte) ([]byte, error) {
	prevTop, err := parseObject(prev)
	if err != nil {
		return nil, fmt.Errorf("parse previous credential blob: %w", err)
	}
	nextTop, err := parseObject(next)
	if err != nil {
		return nil, fmt.Errorf("parse new credential blob: %w: %w", errIncomingUnusable, err)
	}

	normalizeTokens(prevTop)
	normalizeTokens(nextTop)

	merged := make(map[string]json.RawMessage, len(prevTop)+len(nextTop))
	// Start from prev so prev-only top-level keys survive.
	for k, v := range prevTop {
		merged[k] = v
	}
	// Overlay next; next wins for keys it defines. "tokens" is merged specially
	// below.
	for k, v := range nextTop {
		if k == "tokens" {
			continue
		}
		merged[k] = v
	}

	prevTokens, prevErr := parseTokenObject(prevTop["tokens"])
	if prevErr != nil {
		return nil, fmt.Errorf("parse previous tokens object: %w", prevErr)
	}
	nextTokens, nextErr := parseTokenObject(nextTop["tokens"])
	if nextErr != nil {
		return nil, fmt.Errorf("parse new tokens object: %w: %w", errIncomingUnusable, nextErr)
	}

	if prevTokens != nil || nextTokens != nil {
		mergedTokens := mergeTokens(prevTokens, nextTokens)
		raw, mErr := json.Marshal(mergedTokens)
		if mErr != nil {
			return nil, fmt.Errorf("marshal merged tokens: %w", mErr)
		}
		merged["tokens"] = raw
	}

	out, err := json.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged credential blob: %w", err)
	}
	return out, nil
}

// codexAuthForWrite returns the bytes to materialize as the codex auth.json for
// a stored credential. A blob already in complete codex "{tokens:{...}}" shape
// is returned unchanged so the on-disk auth.json stays byte-identical to what
// was stored. A blob carrying the secrets in the account-store top-level
// "{access,refresh,id_token}" shape (or an opencode "{openai:{...}}" shape) is
// normalized so the first spawned codex process finds a usable "tokens" object
// instead of an auth file it cannot read — this includes a blob whose "tokens"
// key is present but empty, null, or missing known fields the top-level secrets
// can fill. Anything that does not parse as a JSON object, or that normalization
// leaves untouched, is returned unchanged — the write is never worse than the
// previous verbatim behavior.
func codexAuthForWrite(blob []byte) []byte {
	top, err := parseObject(blob)
	if err != nil {
		return blob
	}
	beforeTokens, hadTokens := top["tokens"]
	normalizeTokens(top)
	afterTokens, hasTokens := top["tokens"]
	if !hasTokens {
		// No recognizable shape produced a tokens object; write verbatim.
		return blob
	}
	if hadTokens && string(afterTokens) == string(beforeTokens) {
		// tokens object unchanged (already complete); preserve byte-identical.
		return blob
	}
	out, err := json.Marshal(top)
	if err != nil {
		return blob
	}
	return out
}

// authGenerationKey is the codex auth.json field recording when the credential
// it carries was last rotated. Codex stamps it on every token refresh, so it
// orders two blobs that both claim to hold the account's credentials.
const authGenerationKey = "last_refresh"

// authGeneration returns the credential's own generation marker: the
// "last_refresh" timestamp codex stamps into auth.json each time it rotates the
// tokens. The bool is false whenever the blob carries no usable marker — not an
// object, no "last_refresh", or a value that is not an RFC 3339 timestamp — so a
// caller can tell "this blob is older" apart from "these blobs cannot be
// ordered". Blobs stored in the account-store top-level shape carry no marker at
// all, which is why the unordered case must stay a first-class answer rather
// than collapsing into a zero time.
func authGeneration(blob []byte) (time.Time, bool) {
	top, err := parseObject(blob)
	if err != nil {
		return time.Time{}, false
	}
	raw, ok := top[authGenerationKey]
	if !ok || isEmptyJSON(raw) {
		return time.Time{}, false
	}
	var stamp string
	if err := json.Unmarshal(raw, &stamp); err != nil {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(stamp))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// storedCredentialIsNewer reports whether the stored blob was rotated strictly
// later than the on-disk one, per the generation marker both carry. It answers
// the one question a content hash cannot: when auth.json and the store have BOTH
// moved since bossd last wrote the file, which side moved second.
//
// It is deliberately conservative. Either blob lacking a usable marker leaves the
// pair unordered, and an unordered pair reports false — preserving the fold-back
// that keeps an agent-rotated refresh token, which is the data loss this package
// exists to prevent. Equal stamps report false for the same reason. Only a
// provably later store write turns it true.
func storedCredentialIsNewer(stored, current []byte) bool {
	storedGen, ok := authGeneration(stored)
	if !ok {
		return false
	}
	currentGen, ok := authGeneration(current)
	if !ok {
		return false
	}
	return storedGen.After(currentGen)
}

// parseObject parses a blob as a top-level JSON object into a raw-message map,
// so nested values are preserved verbatim unless we explicitly rewrite them.
func parseObject(blob []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(blob, &m); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("blob is not a JSON object")
	}
	return m, nil
}

// normalizeTokens rewrites recognized alternative credential shapes into the
// codex "{tokens:{...}}" shape in place, so mergeTokens' fallback can reach the
// secret fields regardless of the source shape. It handles, in order:
//   - an opencode-ish "{openai:{...}}" object (when "tokens" is absent or
//     unusable — empty, null, a non-object, or an empty object), promoted whole
//     and its key removed;
//   - the account-store top-level "{access,refresh,id_token}" fields that the
//     AddAccount/TestAccount path validates, used to fill "tokens" — synthesizing
//     it when absent, and filling any known token field that is missing, empty,
//     or null even when a "tokens" key already exists (empty/null/partial). The
//     top-level keys are left in place so the stored blob still satisfies that
//     same account-store validation; and
//   - a surviving "openai" object (one the promotion above left in place because
//     "tokens" was a usable but partial object), whose codex fields backfill any
//     token field still missing or empty — so a partial "tokens" such as
//     {"access_token":...} is completed with openai's id_token rather than
//     materializing an auth codex cannot use.
//
// The three steps compose and never clobber a present, non-empty token field, so
// a fresher agent-refreshed value always wins over a stale source, and an
// already-complete "tokens" object is left byte-for-byte unchanged.
func normalizeTokens(top map[string]json.RawMessage) {
	if openai, ok := top["openai"]; ok && tokensUnusable(top["tokens"]) {
		// Promote a token-ish "openai" object whenever "tokens" is absent or
		// unusable (empty, null, a non-object, or an empty object), so a valid
		// credential blob is never blocked by a stale, unreadable "tokens" value.
		// A scalar/empty "openai" fails the object probe and is left for the fills
		// below to handle.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(openai, &probe); err == nil && probe != nil {
			top["tokens"] = openai
			delete(top, "openai")
		}
	}
	// Backfill any field a kept "tokens" object still lacks: top-level account
	// secrets first, then a surviving "openai" object for whatever remains. Both
	// preserve present, non-empty fields, so promotion above (which deletes
	// "openai") makes the openai fill a no-op, and a partial "tokens" is completed
	// without clobbering fresher values.
	fillTokensFromTopLevel(top)
	fillTokensFromOpenai(top)
}

// tokensUnusable reports whether a raw "tokens" value cannot serve as codex
// credentials on its own — absent, null, empty, a non-object (array/string/etc),
// or an empty object. Such a value must not block promoting a usable "openai"
// object into "tokens".
func tokensUnusable(raw json.RawMessage) bool {
	if isEmptyJSON(raw) {
		return true
	}
	m, err := parseTokenObject(raw)
	if err != nil {
		return true
	}
	return len(m) == 0
}

// topLevelTokenKeys maps the account-store top-level credential field names to
// their codex tokens-object equivalents.
var topLevelTokenKeys = []struct{ top, token string }{
	{"access", "access_token"},
	{"refresh", "refresh_token"},
	{"id_token", "id_token"},
}

// codexTokenFields are the codex tokens-object field names an opencode "openai"
// object shares, used to backfill a partial "tokens" from a surviving "openai".
var codexTokenFields = []string{"access_token", "refresh_token", "id_token", "account_id"}

// fillTokensFromTopLevel uses the account-store top-level OAuth fields (mapped to
// codex field names) to fill the "tokens" object: it synthesizes "tokens" when
// absent or null, fills any known token field that is missing or empty from the
// corresponding non-empty top-level field, and rebuilds a "tokens" value that is
// present but not a JSON object (array/string/etc, which codex cannot read and
// persistBack cannot merge) from the top-level secrets. A present, non-empty
// token field is never clobbered, so a fresher agent-refreshed value is
// preserved over a stale top-level one. "tokens" is only rewritten when
// something actually changed, so an already-complete object stays byte-identical.
func fillTokensFromTopLevel(top map[string]json.RawMessage) {
	sources := make(map[string]json.RawMessage, len(topLevelTokenKeys))
	for _, k := range topLevelTokenKeys {
		if v, ok := top[k.top]; ok && !isEmptyJSON(v) {
			sources[k.token] = v
		}
	}
	fillTokensFrom(top, sources)
}

// fillTokensFromOpenai backfills a partial "tokens" object from the matching
// fields of a token-ish "openai" object that promotion left in place (because
// "tokens" was a usable, non-empty object). Only the known codex fields are
// carried over, and only into token fields that are missing or empty, so a
// partial "tokens" missing id_token is completed while present values win. A
// scalar/empty or absent "openai" is a no-op.
func fillTokensFromOpenai(top map[string]json.RawMessage) {
	openai, err := parseTokenObject(top["openai"])
	if err != nil || len(openai) == 0 {
		return
	}
	sources := make(map[string]json.RawMessage, len(codexTokenFields))
	for _, name := range codexTokenFields {
		if v, ok := openai[name]; ok && !isEmptyJSON(v) {
			sources[name] = v
		}
	}
	fillTokensFrom(top, sources)
}

// fillTokensFrom fills any token field named in sources into the "tokens" object:
// it synthesizes "tokens" when absent or null, rebuilds a "tokens" value that is
// present but not a JSON object (array/string/etc, which codex cannot read and
// persistBack cannot merge), and fills any field that is missing or empty. A
// present, non-empty token field is never clobbered, so a fresher agent-refreshed
// value is preserved over a stale source. "tokens" is only rewritten when
// something actually changed, so an already-complete object stays byte-identical.
func fillTokensFrom(top map[string]json.RawMessage, sources map[string]json.RawMessage) {
	if len(sources) == 0 {
		return
	}

	tokens, err := parseTokenObject(top["tokens"])
	if err != nil {
		// "tokens" is present but not a JSON object (array/string/etc). With valid
		// source secrets available, treat it as absent and rebuild from them rather
		// than leaving codex an unreadable tokens value.
		tokens = nil
	}
	if tokens == nil {
		tokens = make(map[string]json.RawMessage, len(sources))
	}
	changed := false
	for tokenKey, src := range sources {
		if existing, ok := tokens[tokenKey]; ok && !isEmptyJSON(existing) {
			continue
		}
		tokens[tokenKey] = src
		changed = true
	}
	if !changed {
		return
	}
	raw, err := json.Marshal(tokens)
	if err != nil {
		return
	}
	top["tokens"] = raw
}

// isEmptyJSON reports whether a raw JSON value is absent, null, or an empty
// string — the states that make a credential field unusable and eligible for a
// top-level fallback.
func isEmptyJSON(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", `""`:
		return true
	default:
		return false
	}
}

// tokenFields is the subset of the codex tokens object we reason about for
// fallback. Unknown fields inside tokens are preserved via the raw overlay in
// mergeTokens.
type tokenFields struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

// parseTokenObject decodes a raw "tokens" value into a raw-message map, or nil
// when the value is absent. An explicit JSON null is treated as absent.
func parseTokenObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// mergeTokens overlays next-token fields onto prev-token fields, preserving
// unknown token fields from both, and applying the fallback rules for the
// four known secret fields.
func mergeTokens(prev, next map[string]json.RawMessage) map[string]json.RawMessage {
	merged := make(map[string]json.RawMessage, len(prev)+len(next))
	for k, v := range prev {
		merged[k] = v
	}
	for k, v := range next {
		merged[k] = v
	}

	prevKnown := decodeKnown(prev)
	nextKnown := decodeKnown(next)

	setKnown(merged, "id_token", firstNonEmpty(nextKnown.IDToken, prevKnown.IDToken))
	setKnown(merged, "access_token", firstNonEmpty(nextKnown.AccessToken, prevKnown.AccessToken))
	setKnown(merged, "refresh_token", firstNonEmpty(nextKnown.RefreshToken, prevKnown.RefreshToken))
	setKnown(merged, "account_id", firstNonEmpty(nextKnown.AccountID, prevKnown.AccountID))
	return merged
}

func decodeKnown(m map[string]json.RawMessage) tokenFields {
	var f tokenFields
	if m == nil {
		return f
	}
	// Re-marshal the raw map and decode the known fields. Errors are ignored:
	// a malformed field simply yields the zero value and falls back.
	if raw, err := json.Marshal(m); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	return f
}

// setKnown writes value under key when non-empty; when empty it removes the key
// so we never emit an empty-string secret field.
func setKnown(m map[string]json.RawMessage, key, value string) {
	if value == "" {
		delete(m, key)
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		delete(m, key)
		return
	}
	m[key] = raw
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// --- Access-token expiry (BOS-1174) ---------------------------------------
//
// A live credential check that merely succeeds proves less than it looks like
// it proves: codex answers the smoke prompt with whatever access token it
// already holds, so a credential whose OAuth refresh chain is dead keeps
// passing until that access token finally expires. The only non-invasive
// discriminator is the access token's own `exp` claim — it lets the caller ask
// "should a healthy credential have refreshed by now?" and read a missing
// refresh as evidence only once the answer is yes.
//
// SIGNATURE VERIFICATION IS DELIBERATELY NOT PERFORMED. Verifying would need a
// JWKS fetch this package has no business making, and it would not change the
// answer: the token is one bossd itself stored and is about to hand to codex,
// so the question is when this credential says it expires, not whether some
// third party forged it. Nothing here is a trust decision — a token that lies
// about `exp` can only make the daemon report a warning, never bench an
// account.
//
// SAFETY: every function below returns times and booleans only. None of them
// returns an error, precisely because an error is the shape that carries a
// quoted value into a log line, and the value here is a bearer token.

// RefreshAssertion is a redacted verdict about whether the credential this
// package materialized should already have been refreshed. It is a
// classification and nothing else: it carries no claim value, no timestamp and
// no token byte, which is what lets it cross package boundaries and reach a
// durable record that is permitted to hold only closed-set tokens.
type RefreshAssertion int

const (
	// RefreshAssertionUnknown means the question could not be evaluated: no
	// readable access token, no readable `exp`, or no usable issuance instant
	// to measure a lifetime against. Absence of a readable claim is NOT
	// evidence of a dead refresh chain, so this is the only safe default and
	// every malformed input lands here.
	RefreshAssertionUnknown RefreshAssertion = iota
	// RefreshAssertionNotDue means the credential is still well inside its
	// access-token lifetime. A client that has not refreshed is behaving
	// normally, so silence says nothing and must not be reported as a problem.
	RefreshAssertionNotDue
	// RefreshAssertionOverdue means the access token is far enough through its
	// own lifetime that a healthy client should already have refreshed it.
	// Combined with a run that wrote no credential, that silence becomes
	// evidence rather than noise.
	RefreshAssertionOverdue
)

// String renders the redacted token form used in logs and tests. It is derived
// entirely from the constant, never from the credential.
func (a RefreshAssertion) String() string {
	switch a {
	case RefreshAssertionNotDue:
		return "not_due"
	case RefreshAssertionOverdue:
		return "overdue"
	default:
		return "unknown"
	}
}

// refreshDueFraction is how far through its own observed lifetime an access
// token must be before a client that still has not refreshed is treated as
// evidence rather than as ordinary quiet.
//
// It is expressed as a FRACTION OF THE OBSERVED LIFETIME, never as an absolute
// duration: the ten-day access token measured on one machine in 2026-09 is a
// single observation of an undocumented provider schedule, and hardcoding it
// would silently stop working the moment the provider retunes. Deriving the
// lifetime from the token itself keeps the threshold meaningful whatever the
// provider chooses.
//
// 0.8 is chosen to sit above any plausible client refresh point while still
// leaving real warning time. OAuth clients conventionally refresh between half
// and three quarters of the way through an access token's life, and codex is
// observed to refresh only near expiry, so a lower fraction would report every
// quiet account as unproven — noise that trains an operator to ignore the
// state, which is the more expensive failure here. On the observed ten-day
// token this converts an eight-day silent window that ended in a hard failure
// into roughly two days of warning.
//
// KNOWN FALSE-POSITIVE WINDOW — read this before retuning the constant. A
// healthy client whose refresh point sits ABOVE this fraction is reported as
// unproven for the stretch between the two. Codex is observed to refresh only
// near expiry, so on the measured ten-day token that is up to the last two
// days of every lifetime, and the caller's other conjunct cannot close it: the
// credential check drives a tiny smoke prompt, which is not by itself an event
// that obliges a client to rotate, so "no credential write during this run" is
// the expected reading for a perfectly healthy account in that window.
//
// The window is accepted rather than engineered away, because of what the
// outcome costs when it is wrong. AuthCheckOutcomeRefreshChainUnproven is
// non-condemning by construction: IsAuthInvalid stays false, the account stays
// selectable and rotation still picks it, so a false positive costs an
// operator-visible "unverified" label on a working account — not a benched one.
// The alternative that would close it, escalating only after a STREAK of
// observations, needs refresh history persisted across checks; the AuthCheck
// record carries no such state today, and inventing it is BOS-1174's explicit
// deferral rather than an oversight.
//
// The consequence for tuning: LOWERING this fraction widens the false-positive
// window proportionally and is what turns the state into ignorable noise.
// Raising it narrows the window but eats the warning time the outcome exists to
// buy. Neither direction is safe to change without a fresh measurement of the
// provider's actual refresh point.
const refreshDueFraction = 0.8

// refreshAssertion classifies the credential blob's access token against the
// refresh-due threshold. It is deliberately conservative in one direction: any
// input it cannot fully evaluate — unparseable blob, no tokens object, no
// access token, an unreadable `exp`, no usable issuance instant, a nonsensical
// lifetime, or a clock skewed such that issuance appears to be in the future —
// returns Unknown rather than Overdue. Reporting "unproven" on a parsing
// failure would put a warning on a working account for a reason that has
// nothing to do with its credential.
func refreshAssertion(blob []byte, now time.Time) RefreshAssertion {
	token, ok := accessToken(blob)
	if !ok {
		return RefreshAssertionUnknown
	}
	expires, ok := tokenNumericClaim(token, "exp")
	if !ok {
		return RefreshAssertionUnknown
	}
	issued, ok := credentialIssuedAt(blob, token, now)
	if !ok {
		return RefreshAssertionUnknown
	}
	lifetime := expires.Sub(issued)
	if lifetime <= 0 {
		// Expiry no later than issuance is not a lifetime this can reason
		// about. Say so rather than dividing by it.
		return RefreshAssertionUnknown
	}
	if now.Sub(issued) > time.Duration(float64(lifetime)*refreshDueFraction) {
		return RefreshAssertionOverdue
	}
	return RefreshAssertionNotDue
}

// credentialIssuedAt resolves the instant the current access token came into
// existence, which is the baseline the observed lifetime is measured from. The
// token's own `iat` claim is preferred — it is what the provider itself says —
// and the auth.json `last_refresh` stamp codex writes on every rotation is the
// fallback for tokens that omit it.
//
// CLOCK SKEW: an issuance instant in the future is refused outright. Nothing
// else guards last_refresh (due() guards only CheckedAt), and a future stamp
// read at face value would look exactly like a refresh that had just happened —
// vouching for the credential this check exists to doubt. "Cannot evaluate" is
// the honest answer and is also the one that cannot warn about a working
// account on a skewed clock.
func credentialIssuedAt(blob []byte, token string, now time.Time) (time.Time, bool) {
	if issued, ok := tokenNumericClaim(token, "iat"); ok {
		return issued, !issued.After(now)
	}
	if issued, ok := authGeneration(blob); ok {
		return issued, !issued.After(now)
	}
	return time.Time{}, false
}

// accessToken pulls the codex access token out of a credential blob. The bool
// is false whenever the blob is not an object, carries no usable tokens
// object, or has no non-empty access_token — every one of which means the
// expiry question simply cannot be asked.
func accessToken(blob []byte) (string, bool) {
	top, err := parseObject(blob)
	if err != nil {
		return "", false
	}
	tokens, err := parseTokenObject(top["tokens"])
	if err != nil || len(tokens) == 0 {
		return "", false
	}
	raw, ok := tokens["access_token"]
	if !ok || isEmptyJSON(raw) {
		return "", false
	}
	var token string
	if err := json.Unmarshal(raw, &token); err != nil {
		return "", false
	}
	return token, token != ""
}

// tokenNumericClaim reads one NumericDate claim (RFC 7519 §2: seconds since
// the Unix epoch) out of a JWT-shaped token's payload segment.
//
// It decodes, it does not verify — see the package note above. The bool is
// false for every shape it cannot read: an empty string, a token without three
// dot-separated segments, a payload that is not unpadded base64url, a payload
// that is not a JSON object, a missing claim, and a claim that is not a
// number. All of them mean the same thing to the caller ("cannot evaluate"),
// and collapsing them onto one boolean is deliberate: distinguishing them in a
// returned error is exactly how token bytes end up quoted in a log line.
func tokenNumericClaim(token, claim string) (time.Time, bool) {
	segments := strings.Split(token, ".")
	if len(segments) != 3 || segments[1] == "" {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(segments[1])
	if err != nil {
		return time.Time{}, false
	}
	claims, err := parseObject(payload)
	if err != nil {
		return time.Time{}, false
	}
	raw, ok := claims[claim]
	if !ok || isEmptyJSON(raw) {
		return time.Time{}, false
	}
	var seconds float64
	if err := json.Unmarshal(raw, &seconds); err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0).UTC(), true
}
