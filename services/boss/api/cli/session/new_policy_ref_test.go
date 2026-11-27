package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/recurser/boss/api/cli/cliutil"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/spf13/cobra"
)

func TestNewPolicyRefRequiresVerifyOnlyBeforeRPC(t *testing.T) {
	contacted := false
	g := New(func(*cobra.Command) (Client, error) { contacted = true; return nil, nil }, Options{})
	cmd := g.NewCmd()
	cmd.SetArgs([]string{"--repo", "repo-1", "--prompt", "verify", "--pr", "42", "--policy-ref", "dev-config"})
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--policy-ref requires --verify-only") {
		t.Fatalf("error = %v, want --policy-ref requires --verify-only", err)
	}
	if cliutil.ErrorCodeFor(err) != cliutil.CodeInvalidArgument {
		t.Fatalf("error code = %s, want invalid argument", cliutil.ErrorCodeFor(err))
	}
	if contacted {
		t.Fatal("invalid flags contacted the daemon")
	}
}

// An explicitly empty --policy-ref (an unset shell variable, say) must not
// quietly create an ordinary verify session that can merge the PR.
func TestNewEmptyPolicyRefRefusedBeforeRPC(t *testing.T) {
	for _, ref := range []string{"", "  "} {
		contacted := false
		g := New(func(*cobra.Command) (Client, error) { contacted = true; return nil, nil }, Options{})
		cmd := g.NewCmd()
		cmd.SetArgs([]string{"--repo", "repo-1", "--prompt", "verify", "--pr", "42", "--verify-only", "--policy-ref", ref})
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--policy-ref needs a branch name") {
			t.Fatalf("policy ref %q: error = %v, want --policy-ref needs a branch name", ref, err)
		}
		if cliutil.ErrorCodeFor(err) != cliutil.CodeInvalidArgument {
			t.Fatalf("policy ref %q: error code = %s, want invalid argument", ref, cliutil.ErrorCodeFor(err))
		}
		if contacted {
			t.Fatalf("policy ref %q: an empty policy ref contacted the daemon", ref)
		}
	}
}

func TestNewPolicyRefRequest(t *testing.T) {
	c := &verifyOnlyCreateClient{}
	g := New(func(*cobra.Command) (Client, error) { return c, nil }, Options{})
	cmd := g.NewCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--repo", "repo-1", "--prompt", "verify", "--pr", "42", "--verify-only", "--policy-ref", "config/trial"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if c.request == nil {
		t.Fatal("no CreateSession request")
	}
	if !c.request.GetIsVerifyOnly() || c.request.GetPolicyRef() != "config/trial" {
		t.Fatalf("request = %v, want is_verify_only and policy_ref=config/trial", c.request)
	}
}

// A bossd that predates trial policy refs drops the field and creates an
// ordinary verify-only session; the CLI must fail rather than report success.
func TestNewPolicyRefFailsWhenDaemonDropsIt(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		c := &createFrameClient{frames: []*pb.CreateSessionResponse{createdFrame("chat-created")}}
		g := New(func(*cobra.Command) (Client, error) { return c, nil }, Options{})
		cmd := g.NewCmd()
		var stdout bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&bytes.Buffer{})
		args := []string{"--repo", "repo-1", "--prompt", "verify", "--pr", "42", "--verify-only", "--policy-ref", "config/trial"}
		if asJSON {
			args = append(args, "--json")
		}
		cmd.SetArgs(args)
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
		err := cmd.Execute()
		if err == nil {
			t.Fatalf("json=%t: no error for a session created without its policy ref; stdout=%s", asJSON, stdout.String())
		}
		if !strings.Contains(err.Error(), "session-created") {
			t.Fatalf("json=%t: error %q does not name the session to archive", asJSON, err)
		}
		if asJSON {
			var envelope cliutil.JSONErrorEnvelope
			if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil || !strings.Contains(envelope.Error.Message, "session-created") {
				t.Fatalf("stdout = %s, want one error envelope naming the session", stdout.String())
			}
		}
	}
}

// Both `boss show --json` and each `boss ls --json` row name policy_ref: verify
// reads the ls rows to hold a standing trial park while its trial session lives.
func TestSessionDetailJSONPolicyRef(t *testing.T) {
	encoders := map[string]func(*pb.Session) any{
		"show": func(s *pb.Session) any { return newSessionDetailJSON(s) },
		"ls":   func(s *pb.Session) any { return newSessionRowJSON(s) },
	}
	for name, encode := range encoders {
		for _, want := range []string{"", "config/trial"} {
			assertPolicyRefKey(t, name, encode(&pb.Session{IsVerifyOnly: true, PolicyRef: want}), want)
		}
	}
}

func assertPolicyRefKey(t *testing.T, name string, row any, want string) {
	t.Helper()
	encoded, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	raw, present := fields["policy_ref"]
	if !present {
		t.Fatalf("%s: policy_ref absent; verify reads an absent key as unreadable", name)
	}
	var got string
	if err := json.Unmarshal(raw, &got); err != nil || got != want {
		t.Fatalf("%s: policy_ref = %s, want %q", name, raw, want)
	}
}
