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
