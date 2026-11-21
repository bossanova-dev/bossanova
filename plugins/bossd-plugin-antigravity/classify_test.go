package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/recurser/bossalib/contract/plugin/agenterr"
)

// runLog renders lines the way agentruntime writes the per-run log.
func runLog(lines ...string) []byte {
	var b strings.Builder
	for _, line := range lines {
		data, _ := json.Marshal(struct {
			TS   string `json:"ts"`
			Text string `json:"text"`
		}{"2026-11-20T10:00:00Z", line})
		b.Write(data)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func resultLine(errText string) string {
	data, _ := json.Marshal(map[string]any{"event": "result", "result": map[string]any{"status": "ERROR", "error": errText}})
	return string(data)
}

func TestClassifyExit(t *testing.T) {
	isolateHome(t)
	failed := errors.New("exit status 1")
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		orig error
		tail []byte
		want func(error) bool
	}{
		{
			name: "logged out",
			orig: failed,
			tail: runLog("Error: authentication required. Run 'agy' to log in, then retry.", resultLine("authentication failed or timed out")),
			want: func(err error) bool { return errors.Is(err, ErrAuthRequired) },
		},
		{
			name: "auth wins over a quota marker",
			orig: failed,
			tail: runLog("individual quota reached", "error: authentication failed or timed out"),
			want: func(err error) bool { return errors.Is(err, ErrAuthRequired) },
		},
		{
			name: "individual quota",
			orig: failed,
			tail: runLog(initLine(conversationA), resultLine("individual quota reached")),
			want: func(err error) bool { return errors.As(err, new(agenterr.ErrUsageLimited)) },
		},
		{
			name: "credits exhausted",
			orig: failed,
			tail: runLog("error: Your AI credits balance is too low to continue."),
			want: func(err error) bool { return errors.As(err, new(agenterr.ErrUsageLimited)) },
		},
		{
			name: "model capacity is a retryable rate limit",
			orig: failed,
			tail: runLog(`AGY_ERROR: {"short_error":"MODEL_CAPACITY_EXHAUSTED","retryable":true}`),
			want: func(err error) bool { return errors.As(err, new(agenterr.ErrRateLimited)) },
		},
		{
			name: "assistant prose about quotas is not a cap",
			orig: failed,
			tail: runLog(`{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"individual quota reached and authentication required"}}`, resultLine("blocked by probe hook")),
			want: func(err error) bool { return err == nil },
		},
		{
			name: "a clean exit is never reclassified",
			orig: nil,
			tail: runLog("individual quota reached", "authentication required"),
			want: func(err error) bool { return err == nil },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyExit(zerolog.Nop(), tt.orig, tt.tail, now); !tt.want(got) {
				t.Fatalf("classifyExit = %v (%T)", got, got)
			}
		})
	}
}

func TestClassifyExitParsesQuotaReset(t *testing.T) {
	isolateHome(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	got := classifyExit(zerolog.Nop(), errors.New("exit status 1"), runLog(resultLine("individual quota reached, resets in 5 hours")), now)
	var ul agenterr.ErrUsageLimited
	if !errors.As(got, &ul) || !ul.ResetAt.Equal(now.Add(5*time.Hour)) {
		t.Fatalf("classifyExit = %v; want a usage limit with its reset time", got)
	}
}
