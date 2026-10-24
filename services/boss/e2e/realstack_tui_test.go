package e2e_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/tuitest"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func realRepos(t *testing.T, h *tuitest.Harness) []*pb.Repo {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	response, err := h.Stack.Client().ListRepos(ctx, connect.NewRequest(&pb.ListReposRequest{}))
	if err != nil {
		t.Fatalf("list real daemon repos: %v", err)
	}
	return response.Msg.GetRepos()
}

func realSessions(t *testing.T, h *tuitest.Harness) []*pb.Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	response, err := h.Stack.Client().ListSessions(ctx, connect.NewRequest(&pb.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("list real daemon sessions: %v", err)
	}
	return response.Msg.GetSessions()
}

func realNotes(t *testing.T, h *tuitest.Harness) []*pb.Note {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	response, err := h.Stack.Client().ListNotes(ctx, connect.NewRequest(&pb.ListNotesRequest{}))
	if err != nil {
		t.Fatalf("list real daemon notes: %v", err)
	}
	return response.Msg.GetNotes()
}

func fieldCursorOnLine(screen, text string) bool {
	for _, line := range strings.Split(screen, "\n") {
		if strings.Contains(line, "┃") && strings.Contains(line, text) {
			return true
		}
	}
	return false
}
