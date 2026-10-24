package tui

import (
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// countingSupervisionProbe returns a probe that reports result and counts its
// calls, so a test can assert whether a returned tea.Cmd really ran the probe.
func countingSupervisionProbe(result DaemonSupervision) (DaemonSupervisionProbe, *atomic.Int32) {
	calls := &atomic.Int32{}
	return func() DaemonSupervision {
		calls.Add(1)
		return result
	}, calls
}

// supervisionMsgsFrom runs every leaf of cmd (flattening batches) on its own
// goroutine and collects the daemonSupervisionMsgs it produces, returning as
// soon as want have arrived or after wait. Leaves that need a real client
// panic against Home's nil one; that is recovered and ignored, because only
// the probe's message is under test. Leaves that sleep (the poll and spinner
// ticks) simply miss the window.
func supervisionMsgsFrom(t *testing.T, cmd tea.Cmd, want int, wait time.Duration) []DaemonSupervisionMsg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	out := make(chan tea.Msg, 64)
	var run func(tea.Cmd)
	run = func(c tea.Cmd) {
		go func() {
			defer func() { _ = recover() }()
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				for _, sub := range batch {
					if sub != nil {
						run(sub)
					}
				}
				return
			}
			out <- msg
		}()
	}
	run(cmd)
	var got []DaemonSupervisionMsg
	deadline := time.After(wait)
	for {
		if want > 0 && len(got) >= want {
			return got
		}
		select {
		case msg := <-out:
			if m, ok := msg.(DaemonSupervisionMsg); ok {
				got = append(got, m)
			}
		case <-deadline:
			return got
		}
	}
}
