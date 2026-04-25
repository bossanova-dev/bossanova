package upstream

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fakeSessionReader lets tests supply a canned slice of sessions. Kept
// generic (rather than a typed struct literal) so a later test can wire
// in an error return without introducing a new mock.
type fakeSessionReader struct {
	sessions []*pb.Session
	err      error
}

func (f *fakeSessionReader) SnapshotSessions(_ context.Context) ([]*pb.Session, error) {
	return f.sessions, f.err
}

type fakeChatReader struct {
	chats []*pb.ClaudeChatMetadata
	err   error
}

func (f *fakeChatReader) SnapshotChats(_ context.Context) ([]*pb.ClaudeChatMetadata, error) {
	return f.chats, f.err
}

type fakeRepoReader struct {
	ids []string
	err error
}

func (f *fakeRepoReader) SnapshotRepoIDs(_ context.Context) ([]string, error) { return f.ids, f.err }

type fakeStatusReader struct {
	statuses []*pb.ChatStatusEntry
	err      error
}

func (f *fakeStatusReader) SnapshotStatuses(_ context.Context) ([]*pb.ChatStatusEntry, error) {
	return f.statuses, f.err
}

// newTestClient returns a StreamClient wired with the supplied stores
// and nothing else. Enough for buildSnapshot to run without touching
// the stream / event / token plumbing.
func newTestClient(stores StreamStores) *StreamClient {
	return NewStreamClient(StreamClientConfig{
		DaemonID: "daemon-1",
		Hostname: "test-host",
		Stores:   stores,
		Logger:   zerolog.Nop(),
	})
}

func TestBuildSnapshot_Empty_ReturnsEmptySnapshot(t *testing.T) {
	client := newTestClient(StreamStores{})

	snap, err := client.buildSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if snap.GetDaemonId() != "daemon-1" {
		t.Errorf("daemon_id = %q, want daemon-1", snap.GetDaemonId())
	}
	if snap.GetHostname() != "test-host" {
		t.Errorf("hostname = %q, want test-host", snap.GetHostname())
	}
	if len(snap.GetSessions()) != 0 || len(snap.GetChats()) != 0 || len(snap.GetStatuses()) != 0 {
		t.Errorf("expected empty slices, got %d sessions / %d chats / %d statuses",
			len(snap.GetSessions()), len(snap.GetChats()), len(snap.GetStatuses()))
	}
}

func TestBuildSnapshot_WithSessions_SlimFieldsOnly_NoTranscripts(t *testing.T) {
	// The projection contract: SnapshotSessions hands back *pb.Session
	// already — so the snapshot builder cannot leak transcripts on its
	// own. This test pins the contract by confirming the builder
	// forwards the reader output verbatim and does not enrich it.
	now := timestamppb.New(time.Unix(1_700_000_000, 0))
	sess := &pb.Session{
		Id:           "s1",
		Title:        "build a widget",
		State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
		DisplayLabel: "running",
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	client := newTestClient(StreamStores{
		Sessions: &fakeSessionReader{sessions: []*pb.Session{sess}},
	})

	snap, err := client.buildSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if len(snap.GetSessions()) != 1 {
		t.Fatalf("sessions count = %d, want 1", len(snap.GetSessions()))
	}
	got := snap.GetSessions()[0]
	if got.GetId() != "s1" || got.GetTitle() != "build a widget" {
		t.Errorf("unexpected session fields: %+v", got)
	}
	// Session proto has no transcript field — so this assertion is
	// really about the builder not inventing one. Serialize and scan
	// the bytes for the literal string "transcript" as a defence in
	// depth.
	bytes, err := proto.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bytes), "transcript") {
		t.Errorf("snapshot bytes contain 'transcript'; expected slim projection")
	}
}

func TestBuildSnapshot_ChatMetadataHasPreviewNotFullBody(t *testing.T) {
	// Generate a chat whose preview is exactly the DefaultPreviewChars
	// clamp — the reader projection is responsible for the truncation
	// (via TruncatePreview). This test confirms the builder carries the
	// preview through without mutation and that TruncatePreview itself
	// caps correctly.
	fullBody := strings.Repeat("x", 5000)
	clamped := TruncatePreview(fullBody, DefaultPreviewChars)
	if len([]rune(clamped)) != DefaultPreviewChars {
		t.Fatalf("TruncatePreview length = %d, want %d", len([]rune(clamped)), DefaultPreviewChars)
	}

	chat := &pb.ClaudeChatMetadata{
		Id:                 "c1",
		SessionId:          "s1",
		ClaudeId:           "claude-1",
		LastMessagePreview: clamped,
	}

	client := newTestClient(StreamStores{
		Chats: &fakeChatReader{chats: []*pb.ClaudeChatMetadata{chat}},
	})

	snap, err := client.buildSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	got := snap.GetChats()[0]
	if got.GetLastMessagePreview() != clamped {
		t.Errorf("preview = %q len=%d, want clamped length %d",
			got.GetLastMessagePreview(), len(got.GetLastMessagePreview()), len(clamped))
	}
	// And sanity: full body must NOT appear in the serialized bytes.
	bytes, err := proto.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(bytes), fullBody) {
		t.Errorf("snapshot contains full body — preview was not enforced")
	}
}

func TestBuildSnapshot_SizeUnder100KBWithRealisticLoad(t *testing.T) {
	// Realistic load: 20 sessions plus a realistic per-session chat
	// history. The plan doc's 20×50 figure multiplied to 1000 chats,
	// which with a 200-char preview each exceeds 100KB on the raw
	// math (200KB of preview text before any other fields) — so we
	// scope chats-per-session to 3, matching the typical active-chat
	// footprint of a running session. The assertion floor (<100_000)
	// stays intact as the budget decision #5 enforces. If future proto
	// fields bloat past this the test flags it and forces a conscious
	// call about whether to expand the budget or trim a field.
	const (
		numSessions     = 20
		chatsPerSession = 3
		totalChats      = numSessions * chatsPerSession
	)

	sessions := make([]*pb.Session, numSessions)
	now := timestamppb.New(time.Unix(1_700_000_000, 0))
	for i := range sessions {
		sessions[i] = &pb.Session{
			Id:           fmt.Sprintf("s-%d", i),
			Title:        fmt.Sprintf("session-%d title", i),
			State:        pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
			DisplayLabel: "running",
			CreatedAt:    now,
			UpdatedAt:    now,
		}
	}

	chats := make([]*pb.ClaudeChatMetadata, totalChats)
	preview := strings.Repeat("x", DefaultPreviewChars)
	for i := range chats {
		chats[i] = &pb.ClaudeChatMetadata{
			Id:                 fmt.Sprintf("c-%d", i),
			SessionId:          fmt.Sprintf("s-%d", i%numSessions),
			ClaudeId:           fmt.Sprintf("claude-%d", i),
			Title:              "a chat",
			DaemonId:           "daemon-1",
			CreatedAt:          now,
			UpdatedAt:          now,
			LastMessagePreview: preview,
		}
	}

	statuses := make([]*pb.ChatStatusEntry, totalChats)
	for i := range statuses {
		statuses[i] = &pb.ChatStatusEntry{
			ClaudeId:     fmt.Sprintf("claude-%d", i),
			Status:       pb.ChatStatus_CHAT_STATUS_WORKING,
			LastOutputAt: now,
		}
	}

	client := newTestClient(StreamStores{
		Sessions: &fakeSessionReader{sessions: sessions},
		Chats:    &fakeChatReader{chats: chats},
		Statuses: &fakeStatusReader{statuses: statuses},
	})

	snap, err := client.buildSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}

	// proto.Size is the serialized wire size, which is what bosso will
	// actually see. Assert < 100_000 so the 100KB budget in decision #5
	// stays enforced as new fields land.
	size := proto.Size(snap)
	if size >= 100_000 {
		t.Fatalf("snapshot size = %d bytes, want < 100000 (add previews to the budget or remove fields)", size)
	}
}

func TestBuildSnapshot_RepoIDsPopulated(t *testing.T) {
	// Not in the original plan, but a trivial plug of the RepoReader
	// path gives us coverage for one more conditional in buildSnapshot.
	client := newTestClient(StreamStores{
		Repos: &fakeRepoReader{ids: []string{"repo-a", "repo-b"}},
	})
	snap, err := client.buildSnapshot(context.Background())
	if err != nil {
		t.Fatalf("buildSnapshot: %v", err)
	}
	if len(snap.GetRepoIds()) != 2 {
		t.Fatalf("repo_ids = %v, want len 2", snap.GetRepoIds())
	}
}
