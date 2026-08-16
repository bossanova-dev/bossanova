package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/recurser/bossalib/agenterr"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

const (
	claudeUsageURL        = "https://api.anthropic.com/api/oauth/usage"
	claudeMessagesURL     = "https://api.anthropic.com/v1/messages"
	claudeOAuthBetaHeader = "oauth-2025-04-20"
	claudeUsageUserAgent  = "claude-code/bossanova-1"
	claudeOAuthTokenEnv   = "CLAUDE_CODE_OAUTH_TOKEN"
	claudeAPIVersion      = "2023-06-01"
	// claudeProbeModel is the cheapest generally-available model used only to
	// elicit rate-limit headers in probeMessagesRateLimit. Keep it current: if
	// the id is ever retired the messages probe still fails safe to UNSUPPORTED
	// (a 400 carries no usage headers), but it stops producing a usable signal.
	claudeProbeModel = "claude-haiku-4-5-20251001"
)

var errClaudeUsageAuthInvalidated = errors.New("auth_invalidated")

type claudeUsageAuthError struct{}

func (claudeUsageAuthError) Error() string {
	return agenterr.KindAuthInvalidated.String()
}

func (claudeUsageAuthError) Unwrap() error {
	return errClaudeUsageAuthInvalidated
}

func (claudeUsageAuthError) GRPCStatus() *status.Status {
	return status.New(codes.Unauthenticated, agenterr.KindAuthInvalidated.String())
}

var errClaudeAccountSuspended = errors.New("account_suspended")

// claudeAccountSuspendedError reports that the account is suspended (an
// org/billing block such as a credit-card limit disabling Claude subscription
// access), as distinct from an invalidated credential (401) or a benign
// scope-refusal 403. It maps to gRPC codes.PermissionDenied so the daemon can
// act ONLY on a confirmed suspension and never on a transient 401. The message
// carries a redaction-safe, human-legible reason for surfacing to operators.
type claudeAccountSuspendedError struct{ reason string }

func (e claudeAccountSuspendedError) Error() string {
	if e.reason != "" {
		return e.reason
	}
	return errClaudeAccountSuspended.Error()
}

func (claudeAccountSuspendedError) Unwrap() error {
	return errClaudeAccountSuspended
}

func (e claudeAccountSuspendedError) GRPCStatus() *status.Status {
	return status.New(codes.PermissionDenied, e.Error())
}

var errClaudeUsageThrottled = errors.New("usage_probe_throttled")

// claudeUsageThrottledError reports that the /api/oauth/usage endpoint itself
// refused the poll (HTTP 429) because we exceeded its polling budget — roughly
// 28-30 requests per identity per rolling 60-minute TRAILING window, so a burst
// saturates the identity for a full hour and pausing does not restore headroom
// early. It says nothing whatsoever about the account's real quota.
//
// That distinction is the whole point of the type. It maps to
// codes.ResourceExhausted, the canonical gRPC mapping for HTTP 429, and is
// deliberately NOT agenterr.ErrRateLimited: that sentinel is the headless-run
// 429 (BOS-406) whose message intentionally contains "429" so a bossd
// round-trip re-derives KindRateLimited and routes it into the usage-limit
// rotation intercept. Reusing it here would bench a perfectly healthy account
// on a polling throttle — the BOS-584 bug class, approached from the other
// side. For the same reason Error() must never contain "429" or a
// "rate limit …exceeded/reached" phrase; agenterr.Classify matches on those.
//
// retryAfter is the parsed Retry-After delay, or zero when the header was
// absent or unparseable. It is a weak lower bound in practice — a post-throttle
// block frequently does not clear at its stated horizon — which is why the
// consuming side applies a floor rather than trusting it alone.
type claudeUsageThrottledError struct{ retryAfter time.Duration }

func (e claudeUsageThrottledError) Error() string {
	if e.retryAfter > 0 {
		return fmt.Sprintf("%s (retry after %s)", errClaudeUsageThrottled.Error(), e.retryAfter)
	}
	return errClaudeUsageThrottled.Error()
}

func (claudeUsageThrottledError) Unwrap() error {
	return errClaudeUsageThrottled
}

// GRPCStatus maps the throttle to codes.ResourceExhausted and, when the
// endpoint told us how long to wait, attaches that as an errdetails.RetryInfo
// so the daemon can honour the server's own horizon instead of guessing. A
// WithDetails failure degrades to the bare status: the code alone is enough for
// the host to apply its floor, so losing the detail must never lose the signal.
func (e claudeUsageThrottledError) GRPCStatus() *status.Status {
	st := status.New(codes.ResourceExhausted, e.Error())
	if e.retryAfter <= 0 {
		return st
	}
	withDetails, err := st.WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(e.retryAfter)})
	if err != nil {
		return st
	}
	return withDetails
}

// parseRetryAfterSeconds parses the delta-seconds form of a Retry-After header.
//
// Only that form is supported. RFC 9110 also allows an HTTP-date, but it is
// rare from this endpoint and parsing it correctly requires trusting the
// server's clock against ours; the caller-side floor already covers the case.
// Every unusable value — absent, non-numeric, fractional, zero, or negative —
// yields a zero duration and never an error: a throttle we cannot time is still
// a throttle, and must not be downgraded to a probe failure.
func parseRetryAfterSeconds(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// suspensionErrFromResponse returns a claudeAccountSuspendedError when resp's
// (non-2xx) body carries the account-suspension signature, else nil. Both probe
// paths call it before degrading a non-2xx response to UNSUPPORTED. The 1 MiB
// cap bounds the read of an untrusted error body.
func suspensionErrFromResponse(resp *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return nil
	}
	if reason, ok := agenterr.SuspensionReason(string(body)); ok {
		return claudeAccountSuspendedError{reason: reason}
	}
	return nil
}

// ProbeRateLimit calls Claude Code's OAuth usage endpoint with the target
// account token and normalizes the response into RateLimitStatus.
func (s *Server) ProbeRateLimit(ctx context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) {
	token := strings.TrimSpace(req.GetCredentialEnv()[claudeOAuthTokenEnv])
	if token == "" {
		return unsupportedProbeRateLimit(), nil
	}

	httpClient := s.probeHTTPClient()
	usageURL := firstNonEmpty(s.usageURL, claudeUsageURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, usageURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build claude usage request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	httpReq.Header.Set("User-Agent", claudeUsageUserAgent)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("probe claude usage rate limit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, claudeUsageAuthError{}
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		// The usage endpoint throttled our polling. This arm sits ahead of the
		// generic non-2xx chain below precisely so it does NOT reach
		// probeMessagesRateLimit: that fallback POSTs a real, billable
		// /v1/messages call which happens to return unified rate-limit headers,
		// so the probe would "succeed" and hide the throttle completely (BOS-828).
		// Paying for a live API call to learn that our poller is rate-limited is
		// the wrong trade — a polling throttle is not evidence about the
		// account's quota.
		//
		// A confirmed suspension still outranks it: an org/billing block is
		// permanent and must fail the account's health, so check the body
		// signature before classifying as the transient throttle.
		if err := suspensionErrFromResponse(resp); err != nil {
			return nil, err
		}
		retryAfter := parseRetryAfterSeconds(resp.Header.Get("Retry-After"))
		// Distinct from both a healthy probe and the daemon's generic
		// "usage probe failed" line so the two are greppable apart. Carries only
		// the endpoint and the parsed delay — never the token or any other
		// credential material.
		s.logger.Warn().
			Str("endpoint", "usage").
			Dur("retry_after", retryAfter).
			Bool("retry_after_known", retryAfter > 0).
			Msg("claude: usage endpoint throttled the poll; skipping the billable messages fallback")
		return nil, claudeUsageThrottledError{retryAfter: retryAfter}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// A suspension (org/billing block) surfaces as a non-2xx body carrying
		// the suspension signature; treat it as a hard failure so the account is
		// failed, not silently degraded to UNSUPPORTED. A benign scope-refusal
		// 403 does not match and falls through to today's header/messages path.
		if err := suspensionErrFromResponse(resp); err != nil {
			return nil, err
		}
		if status, ok := parseUsageHeaders(resp.Header); ok {
			return &bossanovav1.ProbeRateLimitResponse{Status: status}, nil
		}
		return s.probeMessagesRateLimit(ctx, token)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return unsupportedProbeRateLimit(), nil
	}
	status, err := parseUsage(body)
	if err != nil {
		if fallback, ok := parseUsageHeaders(resp.Header); ok {
			return &bossanovav1.ProbeRateLimitResponse{Status: fallback}, nil
		}
		return s.probeMessagesRateLimit(ctx, token)
	}
	return &bossanovav1.ProbeRateLimitResponse{Status: status}, nil
}

// probeMessagesRateLimit infers rate-limit status from the response headers of
// a minimal real /v1/messages call. It is the fallback taken whenever the
// authoritative /api/oauth/usage endpoint yields no usable signal — most
// commonly a 403 scope refusal for `claude setup-token` credentials that lack
// the user:profile scope, which is the *expected* path for such tokens, not a
// rare edge. The request is a deliberate, billable side-effect (Haiku,
// max_tokens=1, ~a dozen tokens) whose cost and cap-window impact are
// negligible; that is the accepted trade for a working usage signal.
func (s *Server) probeMessagesRateLimit(ctx context.Context, token string) (*bossanovav1.ProbeRateLimitResponse, error) {
	httpClient := s.probeHTTPClient()
	messagesURL := firstNonEmpty(s.messagesURL, claudeMessagesURL)

	body, err := json.Marshal(map[string]any{
		"model":      claudeProbeModel,
		"max_tokens": 1,
		"messages": []map[string]string{
			{"role": "user", "content": "hi"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build claude messages probe body: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, messagesURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build claude messages probe request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	httpReq.Header.Set("anthropic-beta", claudeOAuthBetaHeader)
	httpReq.Header.Set("anthropic-version", claudeAPIVersion)
	httpReq.Header.Set("User-Agent", claudeUsageUserAgent)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("probe claude messages rate limit: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, claudeUsageAuthError{}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// The suspended account's real /v1/messages 403 surfaces here; detect it
		// before degrading to UNSUPPORTED. Benign non-2xx bodies do not match.
		if err := suspensionErrFromResponse(resp); err != nil {
			return nil, err
		}
	}
	if status, ok := parseUsageHeaders(resp.Header); ok {
		return &bossanovav1.ProbeRateLimitResponse{Status: status}, nil
	}
	return unsupportedProbeRateLimit(), nil
}

// probeHTTPClient returns the test-overridable client, or a default with a
// bounded timeout so a probe can never hang a rotation decision.
func (s *Server) probeHTTPClient() *http.Client {
	if s.httpClient != nil {
		return s.httpClient
	}
	return &http.Client{Timeout: 10 * time.Second}
}

func unsupportedProbeRateLimit() *bossanovav1.ProbeRateLimitResponse {
	return &bossanovav1.ProbeRateLimitResponse{
		Status: &bossanovav1.RateLimitStatus{
			Limited: false,
			Status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED,
		},
	}
}

type usagePayload struct {
	FiveHour         *usageWindow `json:"five_hour"`
	SevenDay         *usageWindow `json:"seven_day"`
	SevenDayOpus     *usageWindow `json:"seven_day_opus"`
	SevenDaySonnet   *usageWindow `json:"seven_day_sonnet"`
	Status           string       `json:"status"`
	PlanStatus       string       `json:"plan_status"`
	SubscriptionType string       `json:"subscriptionType"`
	RateLimitTier    string       `json:"rate_limit_tier"`
}

type usageWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

func parseUsage(body []byte) (*bossanovav1.RateLimitStatus, error) {
	var payload usagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode claude usage: %w", err)
	}
	status := &bossanovav1.RateLimitStatus{
		Status: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
	}
	seen := false
	if payload.FiveHour != nil {
		status.Util_5H = normalizeUtil(payload.FiveHour.Utilization)
		status.Reset_5H = parseTimestamp(payload.FiveHour.ResetsAt)
		seen = payload.FiveHour.hasRecognizedFields()
	}
	if weekly := mostConstrainedUsageWindow(payload.SevenDay, payload.SevenDayOpus, payload.SevenDaySonnet); weekly != nil {
		status.Util_7D = normalizeUtil(weekly.Utilization)
		status.Reset_7D = parseTimestamp(weekly.ResetsAt)
		seen = seen || weekly.hasRecognizedFields()
	}
	status.PlanTier = firstNonEmpty(payload.SubscriptionType, payload.RateLimitTier)
	if status.PlanTier != "" {
		seen = true
	}
	status.Status = derivePlanStatus(firstNonEmpty(payload.Status, payload.PlanStatus), status.Util_5H, status.Util_7D)
	if firstNonEmpty(payload.Status, payload.PlanStatus) != "" {
		seen = true
	}
	if !seen {
		return nil, errors.New("claude usage response has no recognized usage fields")
	}
	status.Limited = status.Status == bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
	return status, nil
}

func parseUsageHeaders(h http.Header) (*bossanovav1.RateLimitStatus, bool) {
	status := &bossanovav1.RateLimitStatus{
		Status: bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE,
	}
	seen := false
	if util, ok := parseHeaderUtil(h.Get("anthropic-ratelimit-unified-5h-utilization")); ok {
		status.Util_5H = util
		seen = true
	}
	if util, ok := parseHeaderUtil(h.Get("anthropic-ratelimit-unified-7d-utilization")); ok {
		status.Util_7D = util
		seen = true
	}
	if reset := parseTimestamp(h.Get("anthropic-ratelimit-unified-5h-reset")); reset != nil {
		status.Reset_5H = reset
		seen = true
	}
	if reset := parseTimestamp(h.Get("anthropic-ratelimit-unified-7d-reset")); reset != nil {
		status.Reset_7D = reset
		seen = true
	}
	if reset := parseTimestamp(h.Get("anthropic-ratelimit-unified-reset")); reset != nil {
		switch representativeClaimWindow(h.Get("anthropic-ratelimit-unified-representative-claim")) {
		case "5h":
			if status.Reset_5H == nil {
				status.Reset_5H = reset
			}
			seen = true
		case "7d":
			if status.Reset_7D == nil {
				status.Reset_7D = reset
			}
			seen = true
		}
	}
	status.PlanTier = firstNonEmpty(h.Get("anthropic-ratelimit-tier"), h.Get("anthropic-ratelimit-plan-tier"))
	if status.PlanTier != "" {
		seen = true
	}
	genericRaw := firstNonEmpty(h.Get("anthropic-ratelimit-unified-status"), h.Get("anthropic-ratelimit-status"))
	fiveHourRaw := h.Get("anthropic-ratelimit-unified-5h-status")
	sevenDayRaw := h.Get("anthropic-ratelimit-unified-7d-status")
	status.Status = mostSeverePlanStatus(statusFromHeader(genericRaw, status.Util_5H, status.Util_7D), statusFromHeader(fiveHourRaw), statusFromHeader(sevenDayRaw))
	if firstNonEmpty(genericRaw, fiveHourRaw, sevenDayRaw) != "" {
		seen = true
	}
	status.Limited = status.Status == bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
	return status, seen
}

func (w *usageWindow) hasRecognizedFields() bool {
	return w != nil && (w.Utilization != nil || strings.TrimSpace(w.ResetsAt) != "")
}

func mostConstrainedUsageWindow(windows ...*usageWindow) *usageWindow {
	var best *usageWindow
	var bestUtil float64
	for _, window := range windows {
		if window == nil {
			continue
		}
		util := normalizeUtil(window.Utilization)
		if best == nil || util > bestUtil {
			best = window
			bestUtil = util
		}
	}
	return best
}

func parseHeaderUtil(raw string) (float64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	if value > 1 {
		return normalizeUtil(&value), true
	}
	return value, true
}

func representativeClaimWindow(raw string) string {
	normalized := strings.ToLower(strings.TrimSpace(raw))
	normalized = strings.NewReplacer("-", "_", " ", "_").Replace(normalized)
	switch {
	case normalized == "five_hour" || normalized == "5h":
		return "5h"
	case normalized == "seven_day" || normalized == "7d":
		return "7d"
	case strings.HasPrefix(normalized, "seven_day_"):
		return "7d"
	case normalized == "opus" || normalized == "sonnet":
		return "7d"
	default:
		return ""
	}
}

func normalizeUtil(value *float64) float64 {
	if value == nil {
		return 0
	}
	util := *value / 100
	if util > 1 {
		return 1
	}
	return util
}

func parseTimestamp(raw string) *timestamppb.Timestamp {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return timestamppb.New(t)
	}
	if epoch, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return timestamppb.New(time.Unix(epoch, 0).UTC())
	}
	if t, ok := agenterr.ParseResetTime(raw, time.Now()); ok {
		return timestamppb.New(t)
	}
	return nil
}

func derivePlanStatus(raw string, utils ...float64) bossanovav1.RateLimitPlanStatus {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "rate_limited", "limited", "exhausted", "usage_exhausted", "rejected":
		return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
	case "warning", "warn", "near_limit", "allowed_warning":
		return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING
	case "active", "ok", "healthy", "allowed":
		return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE
	case "unsupported":
		return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED
	}
	for _, util := range utils {
		if util >= 1 {
			return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
		}
	}
	for _, util := range utils {
		if util >= 0.8 {
			return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING
		}
	}
	return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE
}

func statusFromHeader(raw string, utils ...float64) bossanovav1.RateLimitPlanStatus {
	if strings.TrimSpace(raw) == "" && len(utils) == 0 {
		return bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSPECIFIED
	}
	return derivePlanStatus(raw, utils...)
}

func mostSeverePlanStatus(values ...bossanovav1.RateLimitPlanStatus) bossanovav1.RateLimitPlanStatus {
	worst := bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSPECIFIED
	for _, value := range values {
		if planStatusSeverity(value) > planStatusSeverity(worst) {
			worst = value
		}
	}
	return worst
}

func planStatusSeverity(value bossanovav1.RateLimitPlanStatus) int {
	switch value {
	case bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED:
		return 4
	case bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING:
		return 3
	case bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE:
		return 2
	case bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED:
		return 1
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
