package server

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	dbpkg "github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func newRepoWithLinear(t *testing.T, store dbpkg.RepoStore, key string) string {
	t.Helper()
	ctx := context.Background()
	repo, err := store.Create(ctx, dbpkg.CreateRepoParams{
		DisplayName:       "r",
		LocalPath:         "/tmp/r",
		OriginURL:         "https://github.com/acme/r.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/wt",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if key != "" {
		if _, err := store.Update(ctx, repo.ID, dbpkg.UpdateRepoParams{LinearAPIKey: &key}); err != nil {
			t.Fatalf("seed linear: %v", err)
		}
	}
	return repo.ID
}

func TestUpdateRepo_SecretActionUnchangedLeavesKey(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "keep-me")

	// UNCHANGED action must not blank the key on a save that does not re-supply it.
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:        id,
		LinearKey: &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_UNCHANGED},
	})); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if got, _ := store.Get(context.Background(), id); got.LinearAPIKey != "keep-me" {
		t.Fatalf("LinearAPIKey = %q, want unchanged %q", got.LinearAPIKey, "keep-me")
	}
}

func TestUpdateRepo_SecretActionSetWrites(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	val := "new-secret"
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:        id,
		LinearKey: &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_SET, Value: &val},
	})); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if got, _ := store.Get(context.Background(), id); got.LinearAPIKey != "new-secret" {
		t.Fatalf("LinearAPIKey = %q, want %q", got.LinearAPIKey, "new-secret")
	}
}

func TestUpdateRepo_SecretActionClearEmpties(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "delete-me")

	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:        id,
		LinearKey: &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_CLEAR},
	})); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if got, _ := store.Get(context.Background(), id); got.LinearAPIKey != "" {
		t.Fatalf("LinearAPIKey = %q, want empty", got.LinearAPIKey)
	}
}

func TestUpdateRepo_SecretUpdateWinsOverLegacy(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	action := "via-action"
	legacy := "via-legacy"
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:           id,
		LinearApiKey: &legacy,
		LinearKey:    &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_SET, Value: &action},
	})); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if got, _ := store.Get(context.Background(), id); got.LinearAPIKey != "via-action" {
		t.Fatalf("LinearAPIKey = %q, want SecretUpdate value %q to win", got.LinearAPIKey, "via-action")
	}
}

func TestUpdateRepo_LegacyKeyHonoredWhenNoAction(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	legacy := "legacy-only"
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:           id,
		LinearApiKey: &legacy,
	})); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	if got, _ := store.Get(context.Background(), id); got.LinearAPIKey != "legacy-only" {
		t.Fatalf("LinearAPIKey = %q, want legacy %q", got.LinearAPIKey, "legacy-only")
	}
}

func TestUpdateRepo_StaleTokenAborted(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	stale := timestamppb.New(time.Now().Add(-time.Hour))
	name := "should-not-apply"
	_, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                id,
		DisplayName:       &name,
		ExpectedUpdatedAt: stale,
	}))
	if got := connect.CodeOf(err); got != connect.CodeAborted {
		t.Fatalf("stale UpdateRepo code = %v, want Aborted (err=%v)", got, err)
	}
}

func TestUpdateRepo_FreshTokenSucceeds(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "")

	// Read the current optimistic-concurrency token via the masked settings.
	getResp, err := s.GetRepoSettings(context.Background(), connect.NewRequest(&pb.GetRepoSettingsRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetRepoSettings: %v", err)
	}
	token := getResp.Msg.GetSettings().GetUpdatedAt()

	name := "renamed"
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                id,
		DisplayName:       &name,
		ExpectedUpdatedAt: token,
	})); err != nil {
		t.Fatalf("fresh-token UpdateRepo: %v", err)
	}
	if got, _ := store.Get(context.Background(), id); got.DisplayName != "renamed" {
		t.Fatalf("display_name = %q, want renamed", got.DisplayName)
	}
}

// TestUpdateRepo_FreshTokenWithSecretClearCommits exercises the intersection of
// the two new load-bearing mechanisms: the optimistic-concurrency guard
// (ExpectedUpdatedAt != nil routes through the BEGIN IMMEDIATE transaction) AND
// a tri-state secret action in the same request. A fresh token plus a CLEAR must
// commit both the secret mutation and the row atomically.
func TestUpdateRepo_FreshTokenWithSecretClearCommits(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := newRepoWithLinear(t, store, "clear-me")

	getResp, err := s.GetRepoSettings(context.Background(), connect.NewRequest(&pb.GetRepoSettingsRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetRepoSettings: %v", err)
	}
	token := getResp.Msg.GetSettings().GetUpdatedAt()

	name := "renamed-with-clear"
	if _, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                id,
		DisplayName:       &name,
		LinearKey:         &pb.SecretUpdate{Action: pb.SecretAction_SECRET_ACTION_CLEAR},
		ExpectedUpdatedAt: token,
	})); err != nil {
		t.Fatalf("fresh-token+CLEAR UpdateRepo: %v", err)
	}
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LinearAPIKey != "" {
		t.Errorf("LinearAPIKey = %q, want cleared through the guarded path", got.LinearAPIKey)
	}
	if got.DisplayName != "renamed-with-clear" {
		t.Errorf("display_name = %q, want renamed-with-clear", got.DisplayName)
	}
}

func TestUpdateRepo_MissingIDNotFound(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})

	token := timestamppb.New(time.Now())
	name := "x"
	_, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:                "does-not-exist",
		DisplayName:       &name,
		ExpectedUpdatedAt: token,
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("missing-id UpdateRepo code = %v, want NotFound (err=%v)", got, err)
	}
}

// TestUpdateRepo_FastPathMissingIDNotFound locks in the non-transactional
// fast-path (no ExpectedUpdatedAt, no OriginURL) mapping of a missing row to
// CodeNotFound. The store returns sql.ErrNoRows from RowsAffected==0 and the
// handler must surface NotFound, not the pre-BOS-386 CodeInternal.
func TestUpdateRepo_FastPathMissingIDNotFound(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})

	name := "x"
	_, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{
		Id:          "does-not-exist",
		DisplayName: &name,
	}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("fast-path missing-id UpdateRepo code = %v, want NotFound (err=%v)", got, err)
	}
}

func TestUpdateRepo_EmptyIDInvalidArgument(t *testing.T) {
	store := dbpkg.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})

	_, err := s.UpdateRepo(context.Background(), connect.NewRequest(&pb.UpdateRepoRequest{Id: ""}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("empty-id UpdateRepo code = %v, want InvalidArgument (err=%v)", got, err)
	}
}
