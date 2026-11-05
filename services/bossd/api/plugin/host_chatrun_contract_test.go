package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/session"
)

func TestChatRunFailureRetryContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mode   chatRunCompletionMode
		reason string
		want   bool
	}{
		{"explicit deadline", chatRunCompletionExplicit, "deadline 1m0s elapsed", true},
		{"explicit cancel", chatRunCompletionExplicit, context.Canceled.Error(), true},
		{"non-explicit deadline", chatRunCompletionHook, "deadline 1m0s elapsed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newRunCompleteServer()
			done := srv.registerRunWithCompletionMode("sess-1", "agent-x", "tok", tc.mode, "", time.Now())
			srv.setWaitChatRunDeadline(time.Nanosecond)
			ctx := t.Context()
			if tc.reason == context.Canceled.Error() {
				srv.setWaitChatRunDeadline(time.Minute)
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			}
			resp, err := srv.WaitChatRun(ctx, &bossanovav1.WaitChatRunHostRequest{AgentSessionId: "agent-x"})
			done <- completionResult{}
			if err != nil {
				t.Fatal(err)
			}
			exitErr := resp.GetExitError()
			if got := sharedplugin.ChatRunRetryable(exitErr); got != tc.want {
				t.Fatalf("retryable(%q) = %v, want %v", exitErr, got, tc.want)
			}
		})
	}
	for _, reclaimFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle watchdog", true: "watchdog reclaim failure"}[reclaimFails], func(t *testing.T) {
			srv, lc := newWatchdogServer(t)
			done := srv.registerRun("sess-1", "agent-x", "tok")
			if reclaimFails {
				lc.reclaimRepairChatFunc = func(context.Context, string, string, string) (session.ReclaimRepairChatResult, error) {
					return session.ReclaimRepairChatResult{}, errors.New("tmux reclaim failed")
				}
			}
			run := srv.runCompletion["agent-x"]
			resp := srv.handleChatRunWatchdog("agent-x", run, time.Minute)
			if !sharedplugin.ChatRunRetryable(resp.GetExitError()) {
				t.Fatalf("host error is not retryable: %q", resp.GetExitError())
			}
			if reclaimFails {
				done <- completionResult{}
			}
		})
	}
}
