package tui

import (
	"reflect"
	"testing"
	"time"

	"github.com/recurser/boss/api/tui/home"

	tea "charm.land/bubbletea/v2"
)

func captureStatusTimers(t *testing.T) *statusTimers {
	t.Helper()
	timers := &statusTimers{}
	prev := home.HomeStatusTick
	home.HomeStatusTick = func(d time.Duration, fn func(time.Time) tea.Msg) tea.Cmd {
		msg := fn(time.Time{}).(HomeStatusExpiredMsg)
		timers.fired = append(timers.fired, msg)
		timers.durations = append(timers.durations, d)
		cmd := func() tea.Msg { return msg }
		timers.cmdCode = reflect.ValueOf(cmd).Pointer()
		return cmd
	}
	t.Cleanup(func() { home.HomeStatusTick = prev })
	return timers
}

func (s *statusTimers) isExpiry(cmd tea.Cmd) bool {
	return s.cmdCode != 0 && reflect.ValueOf(cmd).Pointer() == s.cmdCode
}

// scheduledBy returns the status expiries cmd delivers to the program, so a
// handler that builds the timer and then drops it schedules nothing. A cmd that
// is not itself an expiry is run once to open its batch; the batch's other
// commands are left unrun, as they block on real ticks or reach the client.
func (s *statusTimers) scheduledBy(cmd tea.Cmd) []HomeStatusExpiredMsg {
	if cmd == nil {
		return nil
	}
	if s.isExpiry(cmd) {
		return []HomeStatusExpiredMsg{cmd().(HomeStatusExpiredMsg)}
	}
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		return nil
	}
	var expiries []HomeStatusExpiredMsg
	for _, c := range batch {
		if c != nil && s.isExpiry(c) {
			expiries = append(expiries, c().(HomeStatusExpiredMsg))
		}
	}
	return expiries
}
