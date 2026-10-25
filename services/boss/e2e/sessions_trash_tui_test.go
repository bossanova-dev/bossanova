package e2e_test

import (
	"context"
	"strings"
	"testing"

	"github.com/recurser/boss/internal/adapter/realstackrpc"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"

	"github.com/recurser/boss/internal/tuitest"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func openTrashFromSettings(t *testing.T, h *tuitest.Harness, waitText string) {
	t.Helper()

	openSettingsView(t, h, 't', waitText)
}

func TestJourney_Sessions_AC4_TrashRestoreAndVerifyHome(t *testing.T) {
	sessions := testSessions()
	sessions[0].ArchivedAt = timestamppb.Now()

	h := tuitest.New(t,
		tuitest.WithRepos(testRepos()...),
		tuitest.WithSessions(sessions...),
		tuitest.WithChats(testChats()...),
	)

	if err := h.Driver.WaitForText(waitTimeout, "Fix login bug"); err != nil {
		t.Fatal(err)
	}

	// Navigate to trash.
	openTrashFromSettings(t, h, "Add dark mode")

	// Restore the session.
	selectTUINamedRow(t, h, "Add dark mode")
	if err := h.Driver.SendKey('r'); err != nil {
		t.Fatal(err)
	}
	if err := h.Driver.WaitFor(waitTimeout, func(screen string) bool {
		return strings.Contains(screen, "Initial implementation") ||
			strings.Contains(screen, "Loading chats")
	}); err != nil {
		t.Fatalf("expected chat picker after restore; screen:\n%s", h.Driver.Screen())
	}
	if h.Driver.ScreenContains("Loading chats") {
		if err := h.Driver.WaitForText(waitTimeout, "Initial implementation"); err != nil {
			t.Fatalf("expected restored session chats after loading; screen:\n%s", h.Driver.Screen())
		}
	}

	var found bool
	for _, session := range storedSessionsIncludingArchived(t, h) {
		if session.Id == h.SessionIDs["sess-aaa-111"] {
			found = true
			if session.ArchivedAt != nil {
				t.Fatal("expected session to be restored in daemon")
			}
		}
	}
	if !found {
		t.Fatal("restored session not found in daemon state")
	}

	// Go back to home.
	if err := h.Driver.SendEscape(); err != nil {
		t.Fatal(err)
	}

	// The restored session should now appear on home.
	if err := h.Driver.WaitForText(waitTimeout, "Add dark mode"); err != nil {
		t.Fatalf("restored session not visible on home; screen:\n%s", h.Driver.Screen())
	}
}

func storedSessionsIncludingArchived(t *testing.T, h *tuitest.Harness) []*pb.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	response, err := h.Stack.Client().ListSessions(ctx, realstackrpc.Request(&pb.ListSessionsRequest{IncludeArchived: true}))
	if err != nil {
		t.Fatal(err)
	}
	return response.Msg.GetSessions()
}
