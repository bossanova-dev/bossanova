package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type refreshStub struct {
	req  *pb.RefreshAccountRequest
	resp *pb.RefreshAccountResponse
	err  error
}

func (s *refreshStub) RefreshAccount(_ context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
	s.req = req
	return s.resp, s.err
}

func findRefreshSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	account := accountCmd()
	for _, c := range account.Commands() {
		if c.Name() == "refresh" {
			return c
		}
	}
	t.Fatalf("account command has no `refresh` subcommand")
	return nil
}

func TestAccountRefreshCommandShape(t *testing.T) {
	refresh := findRefreshSubcommand(t)
	for _, name := range []string{"token", "credential-file", "test", "json"} {
		if refresh.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag on `account refresh`", name)
		}
	}
	if refresh.Args == nil {
		t.Fatalf("expected Args validator on `account refresh`")
	}
	if err := refresh.Args(refresh, []string{"acct-1"}); err != nil {
		t.Errorf("expected one account id to be accepted, got %v", err)
	}
	if err := refresh.Args(refresh, []string{}); err == nil {
		t.Errorf("expected missing account id to be rejected")
	}
}

func TestAccountRefreshForwardsCredentialFromStdinAndDoesNotEcho(t *testing.T) {
	refresh := findRefreshSubcommand(t)
	var out bytes.Buffer
	refresh.SetOut(&out)
	refresh.SetIn(strings.NewReader("replacement-token"))
	if err := refresh.Flags().Set("credential-file", "-"); err != nil {
		t.Fatal(err)
	}
	if err := refresh.Flags().Set("test", "true"); err != nil {
		t.Fatal(err)
	}

	stub := &refreshStub{resp: &pb.RefreshAccountResponse{
		Account:      &pb.Account{Id: "acct-1", Label: "work"},
		LiveSmokeRan: true,
		Detail:       "credential test passed",
	}}
	if err := accountRefresh(refresh, stub, "acct-1"); err != nil {
		t.Fatalf("accountRefresh: %v", err)
	}
	if stub.req.GetId() != "acct-1" {
		t.Fatalf("id = %q, want acct-1", stub.req.GetId())
	}
	if got := string(stub.req.GetCredential()); got != "replacement-token" {
		t.Fatalf("credential = %q, want stdin token", got)
	}
	if !stub.req.GetTestAfterSave() {
		t.Fatalf("test_after_save = false, want true")
	}
	if strings.Contains(out.String(), "replacement-token") {
		t.Fatalf("output echoed credential: %q", out.String())
	}
}

func TestAccountRefreshJSONSchema(t *testing.T) {
	refresh := findRefreshSubcommand(t)
	var out bytes.Buffer
	refresh.SetOut(&out)
	if err := refresh.Flags().Set("token", "replacement-token"); err != nil {
		t.Fatal(err)
	}
	if err := refresh.Flags().Set("json", "true"); err != nil {
		t.Fatal(err)
	}

	stub := &refreshStub{resp: &pb.RefreshAccountResponse{
		Account:      &pb.Account{Id: "acct-1", Label: "work"},
		LiveSmokeRan: true,
		Detail:       "credential test passed",
	}}
	if err := accountRefresh(refresh, stub, "acct-1"); err != nil {
		t.Fatalf("accountRefresh: %v", err)
	}
	var got struct {
		Account      accountJSON `json:"account"`
		LiveSmokeRan bool        `json:"live_smoke_ran"`
		Detail       string      `json:"detail"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", out.String(), err)
	}
	if got.Account.ID != "acct-1" || !got.LiveSmokeRan || got.Detail != "credential test passed" {
		t.Fatalf("unexpected json output: %+v", got)
	}
	if strings.Contains(out.String(), "replacement-token") {
		t.Fatalf("json output echoed credential: %q", out.String())
	}
}

// --- reauth subcommand (BOS-1142) ------------------------------------------

func findReauthSubcommand(t *testing.T) *cobra.Command {
	t.Helper()
	account := accountCmd()
	for _, c := range account.Commands() {
		if c.Name() == "reauth" {
			return c
		}
	}
	t.Fatalf("account command has no `reauth` subcommand")
	return nil
}

func TestAccountReauthCommandShape(t *testing.T) {
	reauth := findReauthSubcommand(t)
	if reauth.Flags().Lookup("timeout") == nil {
		t.Error("expected --timeout flag on `account reauth`")
	}
	// Reauth ACQUIRES a credential; it must not also accept one, or the two
	// verbs collapse and `refresh` loses its reason to exist.
	for _, name := range []string{"token", "credential-file"} {
		if reauth.Flags().Lookup(name) != nil {
			t.Errorf("`account reauth` must not take --%s; that is `account refresh`", name)
		}
	}
	if reauth.Args == nil {
		t.Fatal("expected Args validator on `account reauth`")
	}
	if err := reauth.Args(reauth, []string{"acct-1"}); err != nil {
		t.Errorf("expected one account id to be accepted, got %v", err)
	}
	if err := reauth.Args(reauth, []string{}); err == nil {
		t.Error("expected missing account id to be rejected")
	}
	if err := reauth.Args(reauth, []string{"acct-1", "acct-2"}); err == nil {
		t.Error("expected two account ids to be rejected")
	}
}

// `refresh` and `reauth` must stay distinct verbs: one takes a credential you
// already hold, the other goes and gets one.
func TestAccountRefreshAndReauthAreDistinctVerbs(t *testing.T) {
	refresh := findRefreshSubcommand(t)
	reauth := findReauthSubcommand(t)
	if refresh.Name() == reauth.Name() {
		t.Fatal("refresh and reauth collapsed to one command")
	}
	if refresh.Flags().Lookup("credential-file") == nil {
		t.Error("`account refresh` lost its credential input")
	}
}
