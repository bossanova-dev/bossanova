package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Codex rollout spike (BOS-295): rate-limit snapshots are carried by an outer
// codexEnvelope with type "event_msg"; the payload has type "token_count" and
// a non-null "rate_limits" object. The object contains primary/secondary
// windows with used_percent, window_minutes, and Unix-seconds resets_at fields,
// plus plan_type and optional rate_limit_reached_type.
type codexRateLimitEventPayload struct {
	Type       string                  `json:"type"`
	RateLimits *codexRateLimitSnapshot `json:"rate_limits"`
}

type codexRateLimitSnapshot struct {
	Timestamp            time.Time            `json:"-"`
	Primary              codexRateLimitWindow `json:"primary"`
	Secondary            codexRateLimitWindow `json:"secondary"`
	PlanType             string               `json:"plan_type"`
	RateLimitReachedType string               `json:"rate_limit_reached_type"`
}

type codexRateLimitWindow struct {
	UsedPercent   float64 `json:"used_percent"`
	WindowMinutes int     `json:"window_minutes"`
	ResetsAt      int64   `json:"resets_at"`
}

func (s *Server) ProbeRateLimit(_ context.Context, req *bossanovav1.ProbeRateLimitRequest) (*bossanovav1.ProbeRateLimitResponse, error) { //nolint:unparam // interface implementation
	codexHome := strings.TrimSpace(req.GetCredentialEnv()["CODEX_HOME"])
	if codexHome == "" {
		return unsupportedRateLimitStatus(), nil
	}
	sessionsRoot, ok := accountScopedSessionsRoot(codexHome)
	if !ok {
		return unsupportedRateLimitStatus(), nil
	}
	snapshot, ok := latestRateLimitSnapshot(sessionsRoot)
	if !ok {
		return unsupportedRateLimitStatus(), nil
	}
	return &bossanovav1.ProbeRateLimitResponse{Status: mapRateLimitSnapshot(snapshot)}, nil
}

func accountScopedSessionsRoot(codexHome string) (string, bool) {
	root := filepath.Join(codexHome, "sessions")
	resolvedHome, homeErr := filepath.EvalSymlinks(codexHome)
	resolvedRoot, rootErr := filepath.EvalSymlinks(root)
	if homeErr != nil || rootErr != nil {
		return root, true
	}
	rel, err := filepath.Rel(resolvedHome, resolvedRoot)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return root, true
}

func unsupportedRateLimitStatus() *bossanovav1.ProbeRateLimitResponse {
	return &bossanovav1.ProbeRateLimitResponse{
		Status: &bossanovav1.RateLimitStatus{
			Limited: false,
			Status:  bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_UNSUPPORTED,
		},
	}
}

func latestRateLimitSnapshot(root string) (codexRateLimitSnapshot, bool) {
	var latest codexRateLimitSnapshot
	var found bool
	walkRoot := root
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		walkRoot = resolved
	}
	_ = filepath.WalkDir(walkRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if matched, _ := filepath.Match("rollout-*.jsonl", filepath.Base(path)); !matched {
			return nil
		}
		info, statErr := d.Info()
		fallback := time.Time{}
		if statErr == nil {
			fallback = info.ModTime()
		}
		if snapshot, ok := latestRateLimitSnapshotInFile(path, fallback); ok {
			if !found || snapshot.Timestamp.After(latest.Timestamp) {
				latest, found = snapshot, true
			}
		}
		return nil
	})
	return latest, found
}

func latestRateLimitSnapshotInFile(path string, fallback time.Time) (codexRateLimitSnapshot, bool) {
	tail, err := readFileTail(path, transcriptTailSize)
	if err != nil {
		return codexRateLimitSnapshot{}, false
	}
	var latest codexRateLimitSnapshot
	var found bool
	scanner := bufio.NewScanner(bytes.NewReader(tail))
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.Contains(line, []byte(`"rate_limits"`)) {
			continue
		}
		snapshot, ok := parseRateLimitLine(line, fallback)
		if !ok {
			continue
		}
		if !found || snapshot.Timestamp.After(latest.Timestamp) {
			latest, found = snapshot, true
		}
	}
	return latest, found
}

func readFileTail(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > maxBytes {
		start = info.Size() - maxBytes
	}
	if _, err := f.Seek(start, 0); err != nil {
		return nil, err
	}
	buf := make([]byte, info.Size()-start)
	_, err = f.Read(buf)
	return buf, err
}

func parseRateLimitLine(line []byte, fallback time.Time) (codexRateLimitSnapshot, bool) {
	var env codexEnvelope
	if err := json.Unmarshal(line, &env); err != nil || env.Type != "event_msg" {
		return codexRateLimitSnapshot{}, false
	}
	var payload codexRateLimitEventPayload
	if err := json.Unmarshal(env.Payload, &payload); err != nil || payload.Type != "token_count" || payload.RateLimits == nil {
		return codexRateLimitSnapshot{}, false
	}
	snapshot := *payload.RateLimits
	snapshot.Timestamp = fallback
	if parsed, err := time.Parse(time.RFC3339Nano, env.Timestamp); err == nil {
		snapshot.Timestamp = parsed
	}
	return snapshot, true
}

func mapRateLimitSnapshot(snapshot codexRateLimitSnapshot) *bossanovav1.RateLimitStatus {
	return mapRateLimitSnapshotAt(snapshot, time.Now())
}

func mapRateLimitSnapshotAt(snapshot codexRateLimitSnapshot, now time.Time) *bossanovav1.RateLimitStatus {
	limited := windowCurrentlyLimited(snapshot.Primary, now) ||
		windowCurrentlyLimited(snapshot.Secondary, now) ||
		reachedTypeCurrentlyLimited(snapshot, now)
	status := bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE
	if limited {
		status = bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
	}
	return &bossanovav1.RateLimitStatus{
		Limited:  limited,
		Status:   status,
		Util_5H:  percentToFraction(snapshot.Primary.UsedPercent),
		Reset_5H: unixTimestamp(snapshot.Primary.ResetsAt),
		Util_7D:  percentToFraction(snapshot.Secondary.UsedPercent),
		Reset_7D: unixTimestamp(snapshot.Secondary.ResetsAt),
		PlanTier: snapshot.PlanType,
	}
}

func windowCurrentlyLimited(window codexRateLimitWindow, now time.Time) bool {
	return window.UsedPercent >= 100 && !windowExpired(window, now)
}

func reachedTypeCurrentlyLimited(snapshot codexRateLimitSnapshot, now time.Time) bool {
	switch reachedType := strings.ToLower(snapshot.RateLimitReachedType); {
	case strings.Contains(reachedType, "primary") || strings.Contains(reachedType, "5h"):
		return !windowExpired(snapshot.Primary, now)
	case strings.Contains(reachedType, "secondary") || strings.Contains(reachedType, "7d"):
		return !windowExpired(snapshot.Secondary, now)
	case reachedType != "":
		return !windowExpired(snapshot.Primary, now) || !windowExpired(snapshot.Secondary, now)
	default:
		return false
	}
}

func windowExpired(window codexRateLimitWindow, now time.Time) bool {
	return window.ResetsAt > 0 && !time.Unix(window.ResetsAt, 0).After(now)
}

func percentToFraction(percent float64) float64 {
	if percent <= 0 {
		return 0
	}
	if percent >= 100 {
		return 1
	}
	return percent / 100
}

func unixTimestamp(seconds int64) *timestamppb.Timestamp {
	if seconds <= 0 {
		return nil
	}
	return timestamppb.New(time.Unix(seconds, 0).UTC())
}
