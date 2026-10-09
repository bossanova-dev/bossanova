package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// archiveTestSessionID is long enough (>= 32 chars) that resolveSessionID
// passes it through without a ListSessions prefix lookup.
const archiveTestSessionID = "11111111-2222-3333-4444-555555555555"

// runArchiveAgainst drives the real `boss archive` command through the real
// LocalClient against an in-process mock daemon (BOS-1380).
func runArchiveAgainst(t *testing.T, daemon *tuitest.MockDaemon, env map[string]string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("BOSS_SOCKET", daemon.SocketPath())
	stubEnv(t, env)
	cmd := archiveCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{archiveTestSessionID}, args...))
	err := cmd.Execute()
	return out.String(), err
}

func newArchiveTestDaemon(t *testing.T) *tuitest.MockDaemon {
	t.Helper()
	d := tuitest.NewMockDaemon(t)
	d.AddSession(&pb.Session{Id: archiveTestSessionID, Title: "Add dark mode"})
	return d
}

func TestRunArchive_ArchivedHumanOutput(t *testing.T) {
	d := newArchiveTestDaemon(t)
	out, err := runArchiveAgainst(t, d, nil)
	if err != nil {
		t.Fatalf("archive: %v", err)
	}
	want := "Session " + archiveTestSessionID + " archived (Add dark mode).\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunArchive_PendingHumanOutput(t *testing.T) {
	d := newArchiveTestDaemon(t)
	d.SetArchiveDeferred("chat-build-1")
	out, err := runArchiveAgainst(t, d, nil)
	if err != nil {
		t.Fatalf("a pending archive must exit 0, got %v", err)
	}
	want := "Session " + archiveTestSessionID + " archive pending (chat chat-build-1 working).\n"
	if out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
}

func TestRunArchive_JSONEnvelope(t *testing.T) {
	decode := func(t *testing.T, out string) map[string]any {
		t.Helper()
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("stdout is not JSON: %v\n%s", err, out)
		}
		return env
	}

	t.Run("archived", func(t *testing.T) {
		d := newArchiveTestDaemon(t)
		out, err := runArchiveAgainst(t, d, nil, "--json")
		if err != nil {
			t.Fatalf("archive --json: %v", err)
		}
		env := decode(t, out)
		if env["outcome"] != "archived" {
			t.Errorf("outcome = %v, want archived", env["outcome"])
		}
		// Always emitted, empty when archived.
		if v, ok := env["blocking_chat_id"]; !ok || v != "" {
			t.Errorf("blocking_chat_id = %v (present=%v), want present and empty", v, ok)
		}
		sess, _ := env["session"].(map[string]any)
		if sess["id"] != archiveTestSessionID || sess["title"] != "Add dark mode" {
			t.Errorf("session = %v, want id+title", sess)
		}
	})

	t.Run("pending", func(t *testing.T) {
		d := newArchiveTestDaemon(t)
		d.SetArchiveDeferred("chat-build-1")
		out, err := runArchiveAgainst(t, d, nil, "--json")
		if err != nil {
			t.Fatalf("archive --json: %v", err)
		}
		env := decode(t, out)
		if env["outcome"] != "pending" || env["blocking_chat_id"] != "chat-build-1" {
			t.Errorf("envelope = %v, want pending on chat-build-1", env)
		}
	})
}

func TestRunArchive_ForceAndRequesterReachTheRequest(t *testing.T) {
	d := newArchiveTestDaemon(t)
	d.SetArchiveDeferred("chat-build-1")
	out, err := runArchiveAgainst(t, d, map[string]string{"BOSS_AGENT_SESSION_ID": " chat-self "}, "--force")
	if err != nil {
		t.Fatalf("archive --force: %v", err)
	}
	if !strings.Contains(out, "archived (Add dark mode)") {
		t.Errorf("stdout = %q, want the archived line (force bypasses deferral)", out)
	}
	reqs := d.ArchiveSessionRequests()
	if len(reqs) != 1 {
		t.Fatalf("ArchiveSession requests = %d, want 1", len(reqs))
	}
	if !reqs[0].GetShouldForce() {
		t.Error("should_force not sent for --force")
	}
	if got := reqs[0].GetRequesterAgentSessionId(); got != "chat-self" {
		t.Errorf("requester_agent_session_id = %q, want chat-self from $BOSS_AGENT_SESSION_ID", got)
	}
}

func TestRunArchive_NoRequesterOutsideASession(t *testing.T) {
	d := newArchiveTestDaemon(t)
	if _, err := runArchiveAgainst(t, d, nil); err != nil {
		t.Fatalf("archive: %v", err)
	}
	reqs := d.ArchiveSessionRequests()
	if len(reqs) != 1 || reqs[0].GetRequesterAgentSessionId() != "" || reqs[0].GetShouldForce() {
		t.Fatalf("requests = %v, want one unforced request with no requester", reqs)
	}
}
