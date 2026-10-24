package attach

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/pty"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestAttach_LaunchingViewShowsArmedNote(t *testing.T) {
	note := "verify · " + "awaiting checks_passed_ready" + " · expires in 6d"
	newLaunching := func() Model {
		m := NewAttachModel(&attachTelemetryStub{}, context.Background(), pty.NewManager(), "session-1", "agent-armed")
		m.session = &pb.Session{Id: "session-1", Title: "ship it", AgentName: "claude"}
		m.launching = true
		return m
	}

	plain := newLaunching()
	base := plain.View().Content
	if strings.Contains(base, "awaiting") || strings.Count(base, "\n") != 0 {
		t.Fatalf("launching view without a note changed: %q", base)
	}

	armed := newLaunching()
	armed.SetArmedNote(note)
	view := stripANSI(armed.View().Content)
	lines := strings.Split(view, "\n")
	if len(lines) != 2 {
		t.Fatalf("launching view lines = %d, want 2: %q", len(lines), view)
	}
	if lines[0] != stripANSI(base) {
		t.Fatalf("first line changed: %q, want %q", lines[0], stripANSI(base))
	}
	if strings.TrimSpace(lines[1]) != note {
		t.Fatalf("armed line = %q, want %q", strings.TrimSpace(lines[1]), note)
	}
	if strings.Contains(view, "unpublished callback body") {
		t.Fatalf("message body rendered: %q", view)
	}

	// Width-truncated to one row.
	armed.width = 30
	for _, line := range strings.Split(stripANSI(armed.View().Content), "\n")[1:] {
		if w := len([]rune(line)); w > 30 {
			t.Fatalf("armed line %d columns wide in a 30-column terminal: %q", w, line)
		}
	}
}
