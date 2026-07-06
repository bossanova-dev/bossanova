package session

import (
	"strings"
	"testing"
	"time"
)

func TestAutoRotateNotice(t *testing.T) {
	reset := time.Date(2026, 7, 3, 15, 0, 0, 0, time.Local)
	got := AutoRotateNotice("acct-b", reset)
	if !strings.Contains(got, "switched to acct-b") {
		t.Fatalf("notice missing label: %q", got)
	}
	if !strings.Contains(got, "resets ~15:00") {
		t.Fatalf("notice missing reset clause: %q", got)
	}

	noReset := AutoRotateNotice("acct-b", time.Time{})
	if !strings.Contains(noReset, "switched to acct-b") {
		t.Fatalf("zero-reset notice missing label: %q", noReset)
	}
	if strings.Contains(noReset, "resets") {
		t.Fatalf("zero-reset notice must omit reset clause: %q", noReset)
	}
}
