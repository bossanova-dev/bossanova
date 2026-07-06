package main

import (
	"strings"
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestNewDetachRequestSetsModel pins the BOS-179 `boss new --model` wiring: a
// non-empty model flows into CreateSessionRequest.model, an empty one leaves it
// unset (agent default), and the scripting path always requests a headless run.
func strptr(s string) *string { return &s }

func TestNewDetachRequestSetsModel(t *testing.T) {
	req := newDetachRequest("repo-1", "do work", "T", "claude", "claude-opus-4-8", strptr("acct-1"))
	if !req.GetDetach() {
		t.Fatalf("detach = false, want true for the scripting path")
	}
	if req.GetModel() != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8", req.GetModel())
	}
	if req.GetAgentName() != "claude" {
		t.Fatalf("agent = %q, want claude", req.GetAgentName())
	}
	if req.GetAccountId() != "acct-1" {
		t.Fatalf("account = %q, want acct-1", req.GetAccountId())
	}

	empty := newDetachRequest("repo-1", "do work", "T", "", "", nil)
	if empty.Model != nil {
		t.Fatalf("model = %v, want nil (unset) when the flag is empty", empty.Model)
	}
	if empty.AgentName != nil {
		t.Fatalf("agent = %v, want nil (unset) when the flag is empty", empty.AgentName)
	}
	if empty.AccountId != nil {
		t.Fatalf("account = %v, want nil (unset) when the flag is absent", empty.AccountId)
	}
}

// TestNewDetachRequestSetsAccount pins the BOS-170 `boss new --account` wiring:
// a non-empty account flows into CreateSessionRequest.account_id.
func TestNewDetachRequestSetsAccount(t *testing.T) {
	req := newDetachRequest("repo-1", "do work", "T", "", "", strptr("team-openai"))
	if req.GetAccountId() != "team-openai" {
		t.Fatalf("account = %q, want team-openai", req.GetAccountId())
	}
}

// TestNewDetachRequestAccountPresence pins the account-0 opt-out distinction: a
// present-empty account pointer (explicit `--account=`) forwards a present-empty
// account_id (skips the default-account policy), while a nil pointer (omitted
// flag) leaves account_id unset (the daemon applies its policy).
func TestNewDetachRequestAccountPresence(t *testing.T) {
	explicit := newDetachRequest("repo-1", "do work", "T", "", "", strptr(""))
	if explicit.AccountId == nil {
		t.Fatal("explicit --account=: account_id nil, want present-empty")
	}
	if *explicit.AccountId != "" {
		t.Fatalf("explicit --account=: account_id = %q, want present-empty \"\"", *explicit.AccountId)
	}

	absent := newDetachRequest("repo-1", "do work", "T", "", "", nil)
	if absent.AccountId != nil {
		t.Fatalf("omitted flag: account_id = %v, want nil (unset)", absent.AccountId)
	}
}

// TestNewCmdRegistersAccountFlag guards the flag surface so `boss new
// --account` stays wired.
func TestNewCmdRegistersAccountFlag(t *testing.T) {
	cmd := newCmd()
	f := cmd.Flags().Lookup("account")
	if f == nil {
		t.Fatal("`boss new` is missing the --account flag")
	}
	if f.DefValue != "" {
		t.Fatalf("--account default = %q, want empty", f.DefValue)
	}
}

// TestAccountShowLabel pins the `boss show` Account line: an unbound session
// renders "System default", a bound one with no server label renders its
// account id verbatim, and a server-computed label wins when present.
func TestAccountShowLabel(t *testing.T) {
	if got := accountShowLabel("", ""); got != "System default" {
		t.Fatalf("accountShowLabel(\"\", \"\") = %q, want System default", got)
	}
	if got := accountShowLabel("acct-9", ""); got != "acct-9" {
		t.Fatalf("accountShowLabel(acct-9, \"\") = %q, want acct-9", got)
	}
	if got := accountShowLabel("acct-9", "Work Claude"); got != "Work Claude" {
		t.Fatalf("accountShowLabel(acct-9, Work Claude) = %q, want Work Claude", got)
	}
}

// TestPrintSessionShowHeaderRendersAccountLine asserts `boss show` prints an
// Account line for both bound and unbound sessions.
func TestPrintSessionShowHeaderRendersAccountLine(t *testing.T) {
	bound := "team-openai"
	out := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd", AccountId: &bound})
	})
	if !strings.Contains(out, "Account:  team-openai") {
		t.Fatalf("show header missing bound Account line; got:\n%s", out)
	}

	unbound := captureStdout(t, func() {
		printSessionShowHeader(&pb.Session{Id: "sess-1234abcd"})
	})
	if !strings.Contains(unbound, "Account:  System default") {
		t.Fatalf("show header missing unbound Account line; got:\n%s", unbound)
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
