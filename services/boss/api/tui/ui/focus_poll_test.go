package ui

import (
	"testing"
	"time"
)

func TestPollDue(t *testing.T) {
	now := time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name    string
		focused bool
		last    time.Time
		want    bool
	}{
		{"focused immediately", true, now, true},
		{"blurred below interval", false, now.Add(-9 * time.Second), false},
		{"blurred at interval", false, now.Add(-10 * time.Second), true},
		{"blurred above interval", false, now.Add(-11 * time.Second), true},
		{"first poll", false, time.Time{}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := PollDue(tt.focused, tt.last, now); got != tt.want {
				t.Errorf("PollDue = %v, want %v", got, tt.want)
			}
		})
	}
}
