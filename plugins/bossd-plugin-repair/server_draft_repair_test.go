package main

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestMaybeRepair_TriggersForConflict(t *testing.T) {
	for _, state := range []bossanovav1.SessionState{
		bossanovav1.SessionState_SESSION_STATE_AWAITING_CHECKS,
		bossanovav1.SessionState_SESSION_STATE_FIXING_CHECKS,
	} {
		t.Run(state.String(), func(t *testing.T) {
			mock := newTestMock()
			mock.sessions = []*bossanovav1.Session{{Id: "s1", State: state}}
			rm := newTestMonitor(mock)
			rm.maybeRepair("s1", bossanovav1.DisplayStatus_DISPLAY_STATUS_CONFLICT, false)

			waitFor(t, func() bool {
				c, _, _, _ := mock.snapshot()
				return c > 0
			}, "StartChatRun called for CONFLICT")
		})
	}
}
