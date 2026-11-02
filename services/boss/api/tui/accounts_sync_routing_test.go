package tui

import (
	"context"
	"testing"

	"github.com/recurser/boss/api/tui/ui"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// appSyncedCloudAccess is a cloud-access client that can also read the hosted
// synced-account registry.
type appSyncedCloudAccess struct {
	appOrgCloudAccess
}

func (*appSyncedCloudAccess) DialSyncedRegistry(context.Context) (func(context.Context) (*pb.ListSyncedAgentAccountsResponse, error), error) {
	return func(context.Context) (*pb.ListSyncedAgentAccountsResponse, error) {
		return &pb.ListSyncedAgentAccountsResponse{}, nil
	}, nil
}

// The accounts screens get the registry exactly when the cloud-access client
// can read it; without one they show unknown rather than claiming synced.
func TestAccountsScreensWireSyncedRegistry_Accounts_AC225601(t *testing.T) {
	cases := []struct {
		name   string
		access CloudAccessClient
		want   bool
	}{
		{"registry-capable client", &appSyncedCloudAccess{}, true},
		{"client without the registry", &appOrgCloudAccess{}, false},
		{"no client", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := NewApp(nil, nil)
			app.cloudAccess = tc.access
			model, _ := app.handleSwitchView(ui.SwitchViewMsg{View: ui.ViewAgentAccounts, ReturnView: ui.ViewSettings})
			if got := model.(App).accountsList.HasSyncedRegistry(); got != tc.want {
				t.Errorf("list has registry = %v, want %v", got, tc.want)
			}
			model, _ = app.handleSwitchView(ui.SwitchViewMsg{View: ui.ViewAgentAccountEdit, Account: &pb.AgentAccount{Id: "a"}, ReturnView: ui.ViewAgentAccounts})
			if got := model.(App).accountEdit.HasSyncedRegistry(); got != tc.want {
				t.Errorf("detail has registry = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAccountsWebURL(t *testing.T) {
	cases := map[string]string{
		"": "",
		"https://app.example.test/subscribe/return?x=1#frag": "https://app.example.test/settings/accounts",
		"not a url": "",
	}
	for in, want := range cases {
		if got := accountsWebURL(in); got != want {
			t.Errorf("accountsWebURL(%q) = %q, want %q", in, got, want)
		}
	}
}
