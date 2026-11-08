package main

import (
	"reflect"
	"testing"

	"github.com/recurser/bossalib/kit/config"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/domain"
	"github.com/rs/zerolog"
)

func TestCallerGrantsFromSettings(t *testing.T) {
	coordinator := domain.CallerPrincipal{Kind: domain.CallerAgentChat, Role: domain.AgentChatRoleCoordinator}
	build := domain.CallerPrincipal{Kind: domain.CallerAgentChat, Role: domain.AgentChatRoleBuild}
	for _, tc := range []struct {
		raw     string
		granted bool
	}{
		{"", true}, // shipped default: the key is absent from a fresh settings.json
		{"on", true},
		{"off", false},
		{"nope", false}, // an unreadable opt-out withholds the grant
	} {
		grants := callerGrants(&config.Settings{CoordinatorApprovalGrant: tc.raw}, zerolog.Nop())
		for _, c := range []domain.CallerCapability{domain.CapabilityVerifyApprove, domain.CapabilityOperatorMerge} {
			if got := coordinator.HasCapability(c, grants); got != tc.granted {
				t.Fatalf("%q: coordinator %s = %v, want %v", tc.raw, c, got, tc.granted)
			}
			if build.HasCapability(c, grants) {
				t.Fatalf("%q: build chat gained %s", tc.raw, c)
			}
		}
	}
}

func TestCallerPaneSessionsPreservesLinkedMemberships(t *testing.T) {
	panes := []tmux.Pane{{PID: 20, SessionName: "managed-chat"}, {PID: 20, SessionName: "personal"}}
	want := map[int][]string{20: {"managed-chat", "personal"}}
	if got := callerPaneSessions(panes); !reflect.DeepEqual(got, want) {
		t.Fatalf("pane memberships: got %v, want %v", got, want)
	}
}
