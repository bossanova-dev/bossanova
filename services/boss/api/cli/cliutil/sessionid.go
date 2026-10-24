package cliutil

import (
	"context"
	"fmt"
	"strings"

	"github.com/recurser/boss/internal/domain/rpc"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

type SessionLister interface {
	ListSessions(context.Context, *pb.ListSessionsRequest, rpc.SessionReadOptions) ([]*pb.Session, error)
}

func ResolveSessionID(c SessionLister, ctx context.Context, prefix string) (string, error) {
	if len(prefix) >= 32 {
		return prefix, nil
	}
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true}, rpc.SessionReadOptions{})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if strings.HasPrefix(s.Id, prefix) {
			matches = append(matches, s.Id)
		}
	}
	switch len(matches) {
	case 0:
		// Tagged, not wrapped: CodedError leaves the message byte-identical,
		// so the human output is unchanged while --json can still classify
		// this as NOT_FOUND. Resolution happens entirely client-side, so
		// there is no connect code to derive one from.
		return "", CodedError(CodeNotFound, fmt.Errorf("no session found matching prefix %q", prefix))
	case 1:
		return matches[0], nil
	default:
		return "", CodedError(CodeAmbiguousPrefix, fmt.Errorf("ambiguous prefix %q matches %d sessions", prefix, len(matches)))
	}
}

func ResolveArchivedSessionID(c SessionLister, ctx context.Context, prefix string) (string, error) {
	sessions, err := c.ListSessions(ctx, &pb.ListSessionsRequest{IncludeArchived: true}, rpc.SessionReadOptions{})
	if err != nil {
		return "", fmt.Errorf("list sessions: %w", err)
	}
	var matches []string
	for _, s := range sessions {
		if s.ArchivedAt != nil && strings.HasPrefix(s.Id, prefix) {
			matches = append(matches, s.Id)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("no archived session found matching prefix %q", prefix)
	case 1:
		return matches[0], nil
	default:
		return "", fmt.Errorf("ambiguous prefix %q matches %d archived sessions", prefix, len(matches))
	}
}
