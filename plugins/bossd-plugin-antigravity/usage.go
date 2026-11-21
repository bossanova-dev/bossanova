package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// usageProbeTimeout stays under bossd's 30-second plugin RPC budget.
const usageProbeTimeout = 20 * time.Second

const usageWarningUtil = 0.8

// usageReport is `agy -p /usage --output-format json`, which agy answers
// locally without a model call.
type usageReport struct {
	Command struct {
		Data struct {
			Groups []usageGroup `json:"groups"`
		} `json:"data"`
	} `json:"command"`
}

// usageGroup is a set of models sharing quota buckets. agy lists the group of
// its default (Gemini) models first.
type usageGroup struct {
	Name    string `json:"name"`
	Buckets []struct {
		Window            string  `json:"window"`
		RemainingFraction float64 `json:"remaining_fraction"`
		ResetTime         string  `json:"reset_time"`
	} `json:"buckets"`
}

func (r *Runner) probeUsage(ctx context.Context, credentialEnv map[string]string) (*bossanovav1.RateLimitStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, usageProbeTimeout)
	defer cancel()
	out, err := r.runAgy(ctx, os.TempDir(), []string{"-p", "/usage", "--output-format", "json"}, "", credentialEnv)
	if err != nil {
		return nil, err
	}
	var report usageReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return nil, err
	}
	return rateLimitStatus(report)
}

// rateLimitStatus maps agy's quota groups onto bossd's status. The first group
// decides limited, because it holds agy's default models; every group is
// reported as a model window so other models' quota stays visible.
func rateLimitStatus(report usageReport) (*bossanovav1.RateLimitStatus, error) {
	groups := report.Command.Data.Groups
	if len(groups) == 0 {
		return nil, errors.New("usage report has no quota groups")
	}
	st := &bossanovav1.RateLimitStatus{}
	worst := 0.0
	for _, b := range groups[0].Buckets {
		util := 1 - b.RemainingFraction
		worst = max(worst, util)
		switch b.Window {
		case "5h":
			st.Util_5H, st.Reset_5H = util, parseReset(b.ResetTime)
		case "weekly":
			st.Util_7D, st.Reset_7D = util, parseReset(b.ResetTime)
		}
	}
	for _, g := range groups {
		window := &bossanovav1.UsageModelWindow{ModelName: g.Name}
		for _, b := range g.Buckets {
			if util := 1 - b.RemainingFraction; util >= window.GetUtil() {
				window.Util, window.ResetsAt = util, parseReset(b.ResetTime)
			}
		}
		st.ModelWindows = append(st.ModelWindows, window)
	}
	switch {
	case worst >= 1:
		st.Status = bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
	case worst >= usageWarningUtil:
		st.Status = bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_WARNING
	default:
		st.Status = bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_ACTIVE
	}
	st.Limited = st.Status == bossanovav1.RateLimitPlanStatus_RATE_LIMIT_PLAN_STATUS_RATE_LIMITED
	return st, nil
}

func parseReset(s string) *timestamppb.Timestamp {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timestamppb.New(t)
}
