package tuitest_test

import (
	"strings"
	"testing"
	"time"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestTUI_NewSession_AgentFlagThreaded verifies that `boss new --agent <name>`
// surfaces the override on the resulting CreateSessionRequest. The full wizard
// is walked: single repo → "Create a new PR" type → title entry → submit.
//
// MockDaemon.CreateSession records the request and returns Unimplemented (the
// TUI then renders an error banner); we assert on the captured request rather
// than a view transition, mirroring the other NewSession submit tests.
func TestTUI_NewSession_AgentFlagThreaded(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...), // single repo skips repo select
		tuitest.WithArgs("new", "--agent", "opencode"),
	)

	// Boot lands directly in the type-select phase (single repo auto-picked).
	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}

	// "Create a new PR" is the first row — already highlighted. Pick it.
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Session name"); err != nil {
		t.Fatalf("expected form phase; screen:\n%s", h.Driver.Screen())
	}

	// Type a title and submit.
	if err := h.Driver.SendString("multi-agent test"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(waitTimeout)
	var req *pb.CreateSessionRequest
	for time.Now().Before(deadline) {
		req = h.Daemon.LastCreateSession()
		if req != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("CreateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if req.AgentName == nil {
		t.Fatalf("CreateSession.AgentName = nil, want non-nil pointer to %q", "opencode")
	}
	if got := *req.AgentName; got != "opencode" {
		t.Fatalf("CreateSession.AgentName = %q, want %q", got, "opencode")
	}
}

// TestTUI_NewSession_NoAgentFlag_LeavesNil verifies that omitting --agent
// produces a CreateSessionRequest with AgentName == nil, signalling to the
// daemon that it should fall back to Settings.DefaultAgent.
func TestTUI_NewSession_NoAgentFlag_LeavesNil(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithArgs("new"),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Create a new PR"); err != nil {
		t.Fatalf("expected type select; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitForText(waitTimeout, "Session name"); err != nil {
		t.Fatalf("expected form phase; screen:\n%s", h.Driver.Screen())
	}
	if err := h.Driver.SendString("default agent test"); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.SendEnter(); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(waitTimeout)
	var req *pb.CreateSessionRequest
	for time.Now().Before(deadline) {
		req = h.Daemon.LastCreateSession()
		if req != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if req == nil {
		t.Fatalf("CreateSession was never called; screen:\n%s", h.Driver.Screen())
	}
	if req.AgentName != nil {
		t.Fatalf("CreateSession.AgentName = %q, want nil (daemon falls back to Settings.DefaultAgent)", *req.AgentName)
	}
}

// TestCLI_New_AgentFlagRegistered is a fast sanity check that `boss new --help`
// advertises the --agent flag. Catches accidental removal of the flag without
// needing to spin up the full TUI driver.
func TestCLI_New_AgentFlagRegistered(t *testing.T) {
	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithArgs("new", "--help"),
	)
	// `--help` exits before the TUI starts; wait for the help text.
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if strings.Contains(h.Driver.Screen(), "--agent") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("expected `--agent` in `boss new --help`; screen:\n%s", h.Driver.Screen())
}
