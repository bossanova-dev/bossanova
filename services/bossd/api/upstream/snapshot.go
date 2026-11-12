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
		DaemonId:                   c.daemonID,
		IsTerminalSealingSupported: c.terminalSealingSupported,
		CloudControl:               cloudControlProto(c.cloudControl),
		HasCloudControlUpdates:     true,
		// Commands are decided by their resolved target; see command_target.go.
		IsRepoCloudControlSupported: true,
		RepoCloudControlRevision:    c.repoCloudControlRevision.Load(),
		Hostname:                    c.hostname,
		SupportedCommandFields:      daemoncaps.StreamCommands(),
	}

	redactor := c.newCloudRedactor()

	if c.stores.Repos != nil {
		repoIDs, err := c.stores.Repos.SnapshotRepoIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot repos: %w", err)
		}
		snap.RepoIds = repoIDs
		controlIDs, err := c.cloudControlRepoIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot repo cloud control: %w", err)
		}
		redactor.registerRepos(controlIDs)
		snap.RepoCloudControls = redactor.repoCloudControlModes(ctx, controlIDs)
	}

	if c.stores.Sessions != nil {
		sessions, err := c.stores.Sessions.SnapshotSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot sessions: %w", err)
		}
		snap.Sessions = make([]*pb.Session, len(sessions))
		for i, session := range sessions {
			snap.Sessions[i] = redactor.session(ctx, session)
		}
	}

	if c.stores.Chats != nil {
		chats, err := c.stores.Chats.SnapshotChats(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot chats: %w", err)
		}
		snap.Chats = make([]*pb.AgentChatMetadata, len(chats))
		for i, chat := range chats {
			snap.Chats[i] = redactor.chat(ctx, chat)
		}
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

// SessionSnapshotReader returns the slim projection of every active
// session the daemon currently knows about. Built to take the *pb.Session
// directly so the snapshot path never touches models.Session — that
// conversion happens once in the adapter, not per-field here.
type SessionSnapshotReader interface {
	SnapshotSessions(ctx context.Context) ([]*pb.Session, error)
}

// ChatSnapshotReader returns the AgentChat metadata projection (no
// transcripts — just the preview). Kept separate from SessionSnapshot so
// the two calls can run in parallel if that ever becomes a hotspot.
type ChatSnapshotReader interface {
	SnapshotChats(ctx context.Context) ([]*pb.AgentChatMetadata, error)
}

// RepoSnapshotReader lists the repos the daemon is currently managing.
// Snapshot uses the IDs only; the full Repo proto isn't sent up.
type RepoSnapshotReader interface {
	SnapshotRepoIDs(ctx context.Context) ([]string, error)
}

// StatusSnapshotReader returns the current ChatStatusEntry set from the
// in-memory chat-status tracker.
type StatusSnapshotReader interface {
	SnapshotStatuses(ctx context.Context) ([]*pb.ChatStatusEntry, error)
}

// CallbackInterestReader returns the daemon's complete current GitHub
// callback-interest set (distinct repo_origin_url + pr_number over every
// non-terminal callback). Carried on the DaemonSnapshot so bosso can reconcile
// callback interests atomically on every (re)connect. The bossd wiring adapts
// callback.DeriveInterests.
type CallbackInterestReader interface {
	SnapshotCallbackInterests(ctx context.Context) ([]*pb.CallbackInterest, error)
}

// BuildCloudSnapshot shares the stream's per-object redaction with the unary
// reconciliation publisher. Ownership comes from the snapshot's local sessions.
func BuildCloudSnapshot(ctx context.Context, stores StreamStores, policy CloudControlPolicy) (*pb.DaemonSnapshot, error) {
	c := &StreamClient{stores: stores, cloudControl: policy}
	return c.buildSnapshot(ctx)
}
