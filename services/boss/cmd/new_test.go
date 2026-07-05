package main

import "testing"

// TestNewDetachRequestSetsModel pins the BOS-179 `boss new --model` wiring: a
// non-empty model flows into CreateSessionRequest.model, an empty one leaves it
// unset (agent default), and the scripting path always requests a headless run.
func TestNewDetachRequestSetsModel(t *testing.T) {
	req := newDetachRequest("repo-1", "do work", "T", "claude", "claude-opus-4-8")
	if !req.GetDetach() {
		t.Fatalf("detach = false, want true for the scripting path")
	}
	if req.GetModel() != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8", req.GetModel())
	}
	if req.GetAgentName() != "claude" {
		t.Fatalf("agent = %q, want claude", req.GetAgentName())
	}

	empty := newDetachRequest("repo-1", "do work", "T", "", "")
	if empty.Model != nil {
		t.Fatalf("model = %v, want nil (unset) when the flag is empty", empty.Model)
	}
	if empty.AgentName != nil {
		t.Fatalf("agent = %v, want nil (unset) when the flag is empty", empty.AgentName)
	}
}

// TestNewCmdRegistersModelFlag guards the flag surface so `boss new --model`
// stays wired.
func TestNewCmdRegistersModelFlag(t *testing.T) {
	cmd := newCmd()
	f := cmd.Flags().Lookup("model")
	if f == nil {
		t.Fatal("`boss new` is missing the --model flag")
	}
	if f.DefValue != "" {
		t.Fatalf("--model default = %q, want empty", f.DefValue)
	}
}
