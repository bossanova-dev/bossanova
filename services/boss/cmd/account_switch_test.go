package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// switchStub is a narrow accountSwitcher test double capturing the request and
// returning a canned response or error.
type switchStub struct {
	req  *pb.SwitchSessionAccountRequest
	resp *pb.SwitchSessionAccountResponse
	err  error
}

func (s *switchStub) SwitchSessionAccount(_ context.Context, req *pb.SwitchSessionAccountRequest) (*pb.SwitchSessionAccountResponse, error) {
	s.req = req
	return s.resp, s.err
}

// findSwitchSubcommand returns the `switch` subcommand of the account group,
// which carries the real --chat/--force flag definitions.
func findSwitchSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	account := accountCmd()
	for _, c := range account.Commands() {
		if c.Name() == "switch" {
			return c
		}
	}
	t.Fatalf("account command has no `switch` subcommand")
	return nil
}

func TestAccountSwitchCommandShape(t *testing.T) {
	sw := findSwitchSubcommand(t)
	for _, name := range []string{"chat", "force"} {
		if sw.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag on `account switch`", name)
		}
	}
	if sw.Args == nil {
		t.Fatalf("expected Args validator on `account switch`")
	}
	if err := sw.Args(sw, []string{"sess", "acct"}); err != nil {
		t.Errorf("expected two positional args to be accepted, got %v", err)
	}
	if err := sw.Args(sw, []string{"only-one"}); err == nil {
		t.Errorf("expected one positional arg to be rejected")
	}
}

func TestAccountSwitchForwardsArgs(t *testing.T) {
	sw := findSwitchSubcommand(t)
	var out bytes.Buffer
	sw.SetOut(&out)
	if err := sw.Flags().Set("force", "true"); err != nil {
		t.Fatal(err)
	}
	if err := sw.Flags().Set("chat", "agent-42"); err != nil {
		t.Fatal(err)
	}

	stub := &switchStub{resp: &pb.SwitchSessionAccountResponse{
		Resumed:     true,
		TargetLabel: "work",
		NoticeText:  "switched to work — resumed",
	}}

	if err := accountSwitch(sw, stub, "sess-1", "acct-9"); err != nil {
		t.Fatalf("accountSwitch: %v", err)
	}

	if stub.req.GetSessionId() != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", stub.req.GetSessionId())
	}
	if stub.req.GetAccountId() != "acct-9" {
		t.Errorf("account_id = %q, want acct-9", stub.req.GetAccountId())
	}
	if !stub.req.GetForce() {
		t.Errorf("--force should set Force=true")
	}
	if stub.req.AgentSessionId == nil || stub.req.GetAgentSessionId() != "agent-42" {
		t.Errorf("--chat should set AgentSessionId, got %v", stub.req.AgentSessionId)
	}
	if got := out.String(); !strings.Contains(got, "switched to work — resumed") {
		t.Errorf("output %q missing notice", got)
	}
}

func TestAccountSwitchSystemDefaultAndNoChat(t *testing.T) {
	sw := findSwitchSubcommand(t)
	var out bytes.Buffer
	sw.SetOut(&out)

	stub := &switchStub{resp: &pb.SwitchSessionAccountResponse{
		TargetLabel: "system default",
		NoticeText:  "switched to system default — started fresh",
	}}

	if err := accountSwitch(sw, stub, "sess-2", "system-default"); err != nil {
		t.Fatalf("accountSwitch: %v", err)
	}

	// "system-default" sentinel maps to empty account_id (account 0).
	if stub.req.GetAccountId() != "" {
		t.Errorf("account_id = %q, want empty for system default", stub.req.GetAccountId())
	}
	// --chat omitted leaves AgentSessionId nil (target the primary live chat).
	if stub.req.AgentSessionId != nil {
		t.Errorf("AgentSessionId should be nil when --chat omitted, got %q", stub.req.GetAgentSessionId())
	}
	if stub.req.GetForce() {
		t.Errorf("Force should default to false")
	}
	if got := out.String(); !strings.Contains(got, "started fresh") {
		t.Errorf("output %q missing notice", got)
	}
}

func TestAccountSwitchMidTurnRejectionNudgesForce(t *testing.T) {
	sw := findSwitchSubcommand(t)
	sw.SetOut(&bytes.Buffer{})

	stub := &switchStub{err: connect.NewError(
		connect.CodeFailedPrecondition,
		errors.New("chat is mid-turn; confirm or pass --force"),
	)}

	err := accountSwitch(sw, stub, "sess-3", "acct-9")
	if err == nil {
		t.Fatalf("expected error for mid-turn rejection")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mid-turn") {
		t.Errorf("error %q should surface the daemon's rejection reason", msg)
	}
	if !strings.Contains(msg, "--force") {
		t.Errorf("error %q should nudge the user toward --force", msg)
	}
}

func TestSwitchTargetAccountID(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"system-default", ""},
		{"System-Default", ""},
		{"0", ""},
		{"none", ""},
		{"default", ""},
		{"acct-9", "acct-9"},
		{"  acct-9  ", "  acct-9  "}, // non-sentinel passthrough is verbatim
	} {
		if got := switchTargetAccountID(tc.in); got != tc.want {
			t.Errorf("switchTargetAccountID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
