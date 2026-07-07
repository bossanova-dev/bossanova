package bossmcp

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// TestAccountTools drives each account tool, asserting args are forwarded to the
// Backend and the result is returned.
func TestAccountTools(t *testing.T) {
	cases := []struct {
		tool     string
		args     map[string]any
		backend  *fakeBackend
		sentinel string
	}{
		{
			tool: "list_accounts",
			args: map[string]any{"provider": "claude", "refresh": true},
			backend: &fakeBackend{listAccounts: func(_ context.Context, provider string, refresh bool) ([]*pb.Account, error) {
				if provider != "claude" {
					t.Errorf("list_accounts provider not forwarded: %q", provider)
				}
				if !refresh {
					t.Errorf("list_accounts refresh not forwarded")
				}
				return []*pb.Account{{Id: "acct-la", Usage: &pb.UsageSnapshot{Util_5H: 0.75, Status: "limited"}}}, nil
			}},
			sentinel: "util_5h",
		},
		{
			tool: "add_account",
			args: map[string]any{"provider": "claude", "label": "primary", "email": "a@b.co", "priority": 5},
			backend: &fakeBackend{addAccount: func(_ context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
				if req.GetProvider() != "claude" || req.GetLabel() != "primary" || req.GetEmail() != "a@b.co" || req.GetPriority() != 5 {
					t.Errorf("add_account args not forwarded: %+v", req)
				}
				return &pb.Account{Id: "acct-aa"}, nil
			}},
			sentinel: "acct-aa",
		},
		{
			tool: "refresh_account",
			args: map[string]any{"id": "acct-1", "credential": "new-token", "test_after_save": true},
			backend: &fakeBackend{refreshAccount: func(_ context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
				if req.GetId() != "acct-1" || string(req.GetCredential()) != "new-token" || !req.GetTestAfterSave() {
					t.Errorf("refresh_account args not forwarded: %+v", req)
				}
				return &pb.RefreshAccountResponse{Account: &pb.Account{Id: "acct-ra"}, Detail: "credential test passed"}, nil
			}},
			sentinel: "acct-ra",
		},
		{
			tool: "update_account",
			args: map[string]any{"id": "acct-1", "label": "renamed", "status": "disabled", "priority": 9, "allowed_models": []any{"opus", "sonnet"}},
			backend: &fakeBackend{updateAccount: func(_ context.Context, req *pb.UpdateAccountRequest) (*pb.Account, error) {
				if req.GetId() != "acct-1" || req.GetLabel() != "renamed" || req.GetStatus() != "disabled" || req.GetPriority() != 9 {
					t.Errorf("update_account args not forwarded: %+v", req)
				}
				if len(req.GetAllowedModels()) != 2 || req.GetAllowedModels()[0] != "opus" {
					t.Errorf("update_account allowed_models not forwarded: %+v", req.GetAllowedModels())
				}
				return &pb.Account{Id: "acct-ua"}, nil
			}},
			sentinel: "acct-ua",
		},
		{
			tool: "test_account",
			args: map[string]any{"id": "acct-1"},
			backend: &fakeBackend{testAccount: func(_ context.Context, id string) (*pb.TestAccountResponse, error) {
				if id != "acct-1" {
					t.Errorf("test_account id not forwarded: %q", id)
				}
				return &pb.TestAccountResponse{Account: &pb.Account{Id: "acct-ta"}, Detail: "validated"}, nil
			}},
			sentinel: "acct-ta",
		},
		{
			tool: "remove_account",
			args: map[string]any{"id": "acct-1", "confirm": true},
			backend: &fakeBackend{removeAccount: func(_ context.Context, id string) error {
				if id != "acct-1" {
					t.Errorf("remove_account id not forwarded: %q", id)
				}
				return nil
			}},
			sentinel: "removed_account",
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			cs := newConnectedClient(t, tc.backend, Options{})
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
			if err != nil {
				t.Fatalf("call %s: %v", tc.tool, err)
			}
			if res.IsError {
				t.Fatalf("%s returned error result: %s", tc.tool, textOf(t, res))
			}
			if got := textOf(t, res); !strings.Contains(got, tc.sentinel) {
				t.Errorf("%s result missing sentinel %q: %s", tc.tool, tc.sentinel, got)
			}
		})
	}
}

// TestRemoveAccountRequiresConfirm asserts remove_account refuses to run without
// confirm:true, like every other destructive tool.
func TestRemoveAccountRequiresConfirm(t *testing.T) {
	called := false
	backend := &fakeBackend{removeAccount: func(_ context.Context, _ string) error {
		called = true
		return nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "remove_account", Arguments: map[string]any{"id": "acct-1"}})
	if err != nil {
		t.Fatalf("call remove_account: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected error result when confirm omitted")
	}
	if called {
		t.Errorf("backend.RemoveAccount must not be called without confirm:true")
	}
}

// TestAddAccountNeverEchoesCredential asserts the add_account response carries
// only metadata — never the inbound credential blob.
func TestAddAccountNeverEchoesCredential(t *testing.T) {
	secret := "super-secret-token-value"
	backend := &fakeBackend{addAccount: func(_ context.Context, req *pb.AddAccountRequest) (*pb.Account, error) {
		if string(req.GetCredential()) != secret {
			t.Errorf("add_account did not forward the credential to the backend")
		}
		return &pb.Account{Id: "acct-secure", Provider: "claude", Label: "primary"}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "add_account",
		Arguments: map[string]any{"provider": "claude", "label": "primary", "credential": secret},
	})
	if err != nil {
		t.Fatalf("call add_account: %v", err)
	}
	if res.IsError {
		t.Fatalf("add_account returned error: %s", textOf(t, res))
	}
	if got := textOf(t, res); strings.Contains(got, secret) {
		t.Fatalf("add_account response echoed the credential blob: %s", got)
	}
}

func TestRefreshAccountNeverEchoesCredential(t *testing.T) {
	secret := "super-secret-refresh-token"
	backend := &fakeBackend{refreshAccount: func(_ context.Context, req *pb.RefreshAccountRequest) (*pb.RefreshAccountResponse, error) {
		if string(req.GetCredential()) != secret {
			t.Errorf("refresh_account did not forward the credential to the backend")
		}
		return &pb.RefreshAccountResponse{
			Account:      &pb.Account{Id: "acct-secure", Provider: "claude", Label: "primary"},
			LiveSmokeRan: true,
			Detail:       "credential test passed",
		}, nil
	}}
	cs := newConnectedClient(t, backend, Options{})
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "refresh_account",
		Arguments: map[string]any{"id": "acct-secure", "credential": secret, "test_after_save": true},
	})
	if err != nil {
		t.Fatalf("call refresh_account: %v", err)
	}
	if res.IsError {
		t.Fatalf("refresh_account returned error: %s", textOf(t, res))
	}
	if got := textOf(t, res); strings.Contains(got, secret) {
		t.Fatalf("refresh_account response echoed the credential blob: %s", got)
	}
}

// jsonFieldNames extracts the json field names declared on a struct type,
// skipping unexported/embedded protobuf bookkeeping fields (which carry no json
// tag).
func jsonFieldNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		name := strings.Split(tag, ",")[0]
		if name == "" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestAccountArgStructsMatchProto enforces the BOS-99 capability-parity lesson:
// each account tool's typed arg struct maps 1:1 onto its proto request fields.
func TestAccountArgStructsMatchProto(t *testing.T) {
	cases := []struct {
		name string
		args reflect.Type
		req  reflect.Type
	}{
		{"add_account", reflect.TypeOf(AddAccountArgs{}), reflect.TypeOf(pb.AddAccountRequest{})},
		{"refresh_account", reflect.TypeOf(RefreshAccountArgs{}), reflect.TypeOf(pb.RefreshAccountRequest{})},
		{"update_account", reflect.TypeOf(UpdateAccountArgs{}), reflect.TypeOf(pb.UpdateAccountRequest{})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argFields := jsonFieldNames(tc.args)
			reqFields := jsonFieldNames(tc.req)
			if !reflect.DeepEqual(argFields, reqFields) {
				t.Errorf("%s arg/proto field mismatch:\n  args: %v\n  proto: %v", tc.name, argFields, reqFields)
			}
		})
	}
}
