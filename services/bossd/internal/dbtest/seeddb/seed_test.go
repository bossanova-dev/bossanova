package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/sqlutil"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
	"github.com/stretchr/testify/require"
)

func TestSeedRoundTripEverySpecField(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	repo, err := db.NewRepoStore(database).Create(ctx, db.CreateRepoParams{
		DisplayName: "fixture", LocalPath: t.TempDir(), OriginURL: "https://github.com/example/fixture.git", DefaultBaseBranch: "main",
	})
	require.NoError(t, err)
	at := time.Date(2026, 10, 24, 1, 2, 3, 0, time.UTC)
	providerID := "provider-id"
	want := Spec{Version: 1, Sessions: []SessionSpec{{
		Key: "blocked-session", RepoID: repo.ID, Title: "Needs attention", Plan: "the plan",
		WorktreePath: t.TempDir(), BranchName: "fixture/blocked", BaseBranch: "main",
		State: int(machine.Blocked), IsQuickChat: true, ArchivedAt: &at,
		DisplayLabel: "Blocked", DisplayIntent: 4, HasDisplaySpinner: true,
		BlockedReason: "checks failed", RunBlockedReason: "human decision needed",
		SetupError: "setup failed", IsAutomationEnabled: true,
		Chats: []ChatSpec{
			{AgentSessionID: "failed-chat", ProviderSessionID: &providerID, Title: "Failed chat", Model: "fixture-model", Role: "operator", StartError: "start failed"},
			{AgentSessionID: "reclaimed-chat", Title: "Replaced chat", Model: "other-model", Role: "repair", ReclaimReason: "replaced by repair"},
		},
		CheckSnapshots: []CheckSnapshotSpec{{PolledAt: at, HeadSHA: "aabbcc", RawJSON: `[{"name":"test","state":"FAILURE"}]`, ComputedStatus: 3}},
		Timeline:       []TimelineSpec{{ID: "rotation-id", ChatID: "failed-chat", Provider: "stub", Trigger: "ROTATION_TRIGGER_USAGE_LIMITED", FromAccount: "first", ToAccount: "second", ResetAt: &at, Outcome: "ROTATION_OUTCOME_ROTATED", Detail: "fixture detail", CreatedAt: at}},
	}}}
	// Include JSON decoding in the proof: the boss module passes this contract
	// across a process boundary and cannot import bossd's fixture package.
	encoded, err := json.Marshal(want)
	require.NoError(t, err)
	var input Spec
	require.NoError(t, json.Unmarshal(encoded, &input))
	result, err := Seed(ctx, database, input)
	require.NoError(t, err)
	require.Len(t, result.SessionIDs, 1)
	id := result.SessionIDs[want.Sessions[0].Key]
	require.NotEmpty(t, id)
	session, err := db.NewSessionStore(database).Get(ctx, id)
	require.NoError(t, err)
	require.Equal(t, "stub", session.AgentName)
	got := SessionSpec{
		Key: want.Sessions[0].Key, RepoID: session.RepoID, Title: session.Title, Plan: session.Plan,
		WorktreePath: session.WorktreePath, BranchName: session.BranchName, BaseBranch: session.BaseBranch,
		State: int(session.State), IsQuickChat: session.IsQuickChat, ArchivedAt: session.ArchivedAt,
		DisplayLabel: session.DisplayLabel, DisplayIntent: session.DisplayIntent, HasDisplaySpinner: session.DisplaySpinner,
		BlockedReason: *session.BlockedReason, RunBlockedReason: *session.RunBlockedReason,
		SetupError: session.SetupError, IsAutomationEnabled: session.IsAutomationEnabled,
	}
	for _, expected := range want.Sessions[0].Chats {
		chat, err := db.NewAgentChatStore(database).GetByAgentSessionID(ctx, expected.AgentSessionID)
		require.NoError(t, err)
		require.Equal(t, id, chat.SessionID)
		require.Equal(t, "stub", chat.AgentName)
		actual := ChatSpec{AgentSessionID: chat.AgentSessionID, ProviderSessionID: chat.ProviderSessionID, Title: chat.Title, Model: chat.Model, Role: string(chat.Role)}
		if chat.StartError != nil {
			actual.StartError = *chat.StartError
		}
		if chat.ReclaimReason != nil {
			actual.ReclaimReason = *chat.ReclaimReason
			require.NotNil(t, chat.ReclaimedAt)
		}
		got.Chats = append(got.Chats, actual)
	}
	snapshots, err := db.NewCheckSnapshotStore(database).ListBySession(ctx, id, 10)
	require.NoError(t, err)
	for _, snapshot := range snapshots {
		got.CheckSnapshots = append(got.CheckSnapshots, CheckSnapshotSpec{PolledAt: snapshot.PolledAt, HeadSHA: snapshot.HeadSHA, RawJSON: snapshot.RawJSON, ComputedStatus: snapshot.ComputedStatus})
	}
	events, err := db.NewRotationEventStore(database).ListBySession(ctx, id, 10)
	require.NoError(t, err)
	for _, event := range events {
		got.Timeline = append(got.Timeline, TimelineSpec{ID: event.ID, ChatID: event.ChatID, Provider: event.Provider, Trigger: event.Trigger, FromAccount: event.FromAccount, ToAccount: event.ToAccount, ResetAt: event.ResetAt, Outcome: event.Outcome, Detail: event.Detail, CreatedAt: event.CreatedAt})
	}
	require.Equal(t, want, Spec{Version: input.Version, Sessions: []SessionSpec{got}})
	archived, err := db.NewSessionStore(database).ListArchived(ctx, repo.ID)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, id, archived[0].ID)
}

func TestSeedRejectsUnknownVersion(t *testing.T) {
	_, err := Seed(context.Background(), dbtest.New(t), Spec{Version: 2})
	require.ErrorContains(t, err, "unsupported seed spec version")
}

func TestSeedRejectsInvalidSessionBeforeWriting(t *testing.T) {
	ctx := context.Background()
	database := dbtest.New(t)
	repo, err := db.NewRepoStore(database).Create(ctx, db.CreateRepoParams{
		DisplayName: "fixture", LocalPath: t.TempDir(), OriginURL: "https://github.com/example/fixture.git", DefaultBaseBranch: "main",
	})
	require.NoError(t, err)
	valid := SessionSpec{Key: "one", RepoID: repo.ID, Title: "valid", State: int(machine.Blocked)}
	for _, tc := range []struct {
		name   string
		change func(*SessionSpec)
	}{
		{"missing key", func(s *SessionSpec) { s.Key = "" }},
		{"unknown state", func(s *SessionSpec) { s.State = -1 }},
		{"missing repo", func(s *SessionSpec) { s.RepoID = "absent" }},
		{"missing worktree", func(s *SessionSpec) { s.WorktreePath = filepath.Join(t.TempDir(), "absent") }},
		{"unknown role", func(s *SessionSpec) { s.Chats = []ChatSpec{{AgentSessionID: "chat", Role: "invented"}} }},
		{"missing chat identity", func(s *SessionSpec) { s.Chats = []ChatSpec{{Role: "operator"}} }},
		{"conflicting chat history", func(s *SessionSpec) {
			s.Chats = []ChatSpec{{AgentSessionID: "chat", StartError: "failed", ReclaimReason: "replaced"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := valid
			tc.change(&invalid)
			_, err := Seed(ctx, database, Spec{Version: 1, Sessions: []SessionSpec{invalid}})
			require.Error(t, err)
		})
	}
	_, err = Seed(ctx, database, Spec{Version: 1, Sessions: []SessionSpec{valid, valid}})
	require.ErrorContains(t, err, "duplicate session key")
	sessions, err := db.NewSessionStore(database).List(ctx, repo.ID)
	require.NoError(t, err)
	require.Empty(t, sessions)
}

func TestCommandMigratesTemplate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "template.db")
	require.NoError(t, run([]string{"--db", path, "--migrate-only"}, os.Stdout))
	database, err := sqlutil.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, database.Close()) })
	repos, err := db.NewRepoStore(database).List(context.Background())
	require.NoError(t, err)
	require.Empty(t, repos)
}

func TestCommandRejectsInvalidInput(t *testing.T) {
	for _, args := range [][]string{nil, {"--db", "unused"}, {"--db", "unused", "--migrate-only", "--spec", "unused"}, {"--unknown"}} {
		require.Error(t, run(args, os.Stdout))
	}
	for _, content := range []string{`{"version":1,"unknown":true}`, `{"version":1} {"version":1}`} {
		path := filepath.Join(t.TempDir(), "spec.json")
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		require.Error(t, run([]string{"--db", filepath.Join(t.TempDir(), "unused.db"), "--spec", path}, os.Stdout))
	}
}
