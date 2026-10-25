package upstream

import (
	"context"
	"fmt"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

// buildSnapshot assembles the DaemonSnapshot that's sent as the first
// event on every stream (re)connect. The snapshot is explicitly slim:
// full chat transcripts are fetched on demand via AttachSession later,
// so this function must never fan out to large text fields. Each store
// is asked for an already-projected proto so this glue stays transport-
// agnostic — conversion from the SQLite row model happens behind the
// StreamStores interfaces (wired in T3.7).
func (c *StreamClient) buildSnapshot(ctx context.Context) (*pb.DaemonSnapshot, error) {
	snap := &pb.DaemonSnapshot{
		DaemonId:               c.daemonID,
		Hostname:               c.hostname,
		SupportedCommandFields: daemoncaps.StreamCommands(),
	}

	if c.stores.Repos != nil {
		repoIDs, err := c.stores.Repos.SnapshotRepoIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot repos: %w", err)
		}
		snap.RepoIds = repoIDs
	}

	if c.stores.Sessions != nil {
		sessions, err := c.stores.Sessions.SnapshotSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot sessions: %w", err)
		}
		snap.Sessions = sessions
	}

	if c.stores.Chats != nil {
		chats, err := c.stores.Chats.SnapshotChats(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot chats: %w", err)
		}
		snap.Chats = chats
	}

	if c.stores.Statuses != nil {
		statuses, err := c.stores.Statuses.SnapshotStatuses(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot statuses: %w", err)
		}
		snap.Statuses = statuses
	}

	if c.stores.Interests != nil {
		interests, err := c.stores.Interests.SnapshotCallbackInterests(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot callback interests: %w", err)
		}
		snap.CallbackInterests = interests
	}

	if c.stores.AgentAccounts != nil {
		accounts, err := c.stores.AgentAccounts.SnapshotAgentAccounts(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot agent accounts: %w", err)
		}
		snap.AgentAccounts = accounts
	}
	return snap, nil
}

// TruncatePreview clamps a string to at most maxChars runes. Exposed for
// the store adapters that build AgentChatMetadata — keeping the clamp
// in this package means every caller enforces the same cap and the
// 100KB snapshot budget has one owner, not five.
func TruncatePreview(s string, maxChars int) string {
	if maxChars <= 0 || s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxChars {
		return s
	}
	return string(runes[:maxChars])
}

// DefaultPreviewChars is the preview cap applied everywhere a chat's
// last_message_preview is populated. Lives here rather than scattered
// across call sites so changing the cap is a one-line edit.
const DefaultPreviewChars = 200

// StreamStores bundles the SQLite-backed readers the snapshot builder
// needs.
type StreamStores struct {
	Sessions      SessionSnapshotReader
	Chats         ChatSnapshotReader
	Repos         RepoSnapshotReader
	Statuses      StatusSnapshotReader
	Interests     CallbackInterestReader
	AgentAccounts AgentAccountSnapshotReader
}
