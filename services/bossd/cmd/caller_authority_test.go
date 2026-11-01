package main

import (
	"reflect"
	"testing"

	"github.com/recurser/bossd/internal/adapter/tmux"
)

func TestCallerPaneSessionsPreservesLinkedMemberships(t *testing.T) {
	panes := []tmux.Pane{{PID: 20, SessionName: "managed-chat"}, {PID: 20, SessionName: "personal"}}
	want := map[int][]string{20: {"managed-chat", "personal"}}
	if got := callerPaneSessions(panes); !reflect.DeepEqual(got, want) {
		t.Fatalf("pane memberships: got %v, want %v", got, want)
	}
}
