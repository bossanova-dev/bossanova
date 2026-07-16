package server

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/db"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// seedRepoWithSecrets creates a repo and stamps distinctive secret sentinels +
// non-secret settings via the real store, returning the repo id.
func seedRepoWithSecrets(t *testing.T, store db.RepoStore) string {
	t.Helper()
	ctx := context.Background()
	repo, err := store.Create(ctx, db.CreateRepoParams{
		DisplayName:       "secret-repo",
		LocalPath:         "/tmp/secret-repo",
		OriginURL:         "https://github.com/acme/secret-repo.git",
		DefaultBaseBranch: "main",
		WorktreeBaseDir:   "/tmp/worktrees",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	lin := "lin_SECRET_MARKER_L"
	sen := "sntry_SECRET_MARKER_S"
	org := "acme-org"
	strategy := models.MergeStrategyRebase
	if _, err := store.Update(ctx, repo.ID, db.UpdateRepoParams{
		LinearAPIKey:  &lin,
		SentryAPIKey:  &sen,
		SentryOrg:     &org,
		MergeStrategy: &strategy,
	}); err != nil {
		t.Fatalf("seed secrets: %v", err)
	}
	return repo.ID
}

func TestGetRepoSettings_MasksSecrets(t *testing.T) {
	store := db.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})
	id := seedRepoWithSecrets(t, store)

	resp, err := s.GetRepoSettings(context.Background(), connect.NewRequest(&pb.GetRepoSettingsRequest{Id: id}))
	if err != nil {
		t.Fatalf("GetRepoSettings() error = %v", err)
	}
	settings := resp.Msg.GetSettings()
	if settings == nil {
		t.Fatal("GetRepoSettings() returned nil settings")
	}

	// The masked view reports presence, never the material.
	if !settings.GetHasLinearKey() {
		t.Error("HasLinearKey = false, want true")
	}
	if !settings.GetHasSentryKey() {
		t.Error("HasSentryKey = false, want true")
	}
	if settings.GetSentryOrg() != "acme-org" {
		t.Errorf("SentryOrg = %q, want %q", settings.GetSentryOrg(), "acme-org")
	}
	if settings.GetMergeStrategy() != pb.MergeStrategy_MERGE_STRATEGY_REBASE {
		t.Errorf("MergeStrategy = %v, want REBASE", settings.GetMergeStrategy())
	}
	if settings.GetUpdatedAt() == nil {
		t.Error("UpdatedAt is nil, want the optimistic-concurrency token")
	}

	// Security invariant: no secret substring survives either serialization.
	binOut, err := proto.Marshal(resp.Msg)
	if err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
	jsonOut, err := protojson.Marshal(resp.Msg)
	if err != nil {
		t.Fatalf("protojson.Marshal: %v", err)
	}
	for _, marker := range []string{"SECRET_MARKER_L", "SECRET_MARKER_S"} {
		if bytes.Contains(binOut, []byte(marker)) {
			t.Errorf("proto.Marshal output leaked secret marker %q", marker)
		}
		if bytes.Contains(jsonOut, []byte(marker)) {
			t.Errorf("protojson.Marshal output leaked secret marker %q", marker)
		}
	}
}

func TestGetRepoSettings_EmptyID(t *testing.T) {
	store := db.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})

	_, err := s.GetRepoSettings(context.Background(), connect.NewRequest(&pb.GetRepoSettingsRequest{Id: ""}))
	if got := connect.CodeOf(err); got != connect.CodeInvalidArgument {
		t.Fatalf("GetRepoSettings(empty id) code = %v, want InvalidArgument (err=%v)", got, err)
	}
}

func TestGetRepoSettings_MissingID(t *testing.T) {
	store := db.NewRepoStore(setupServerTestDB(t))
	s := New(Config{Repos: store})

	_, err := s.GetRepoSettings(context.Background(), connect.NewRequest(&pb.GetRepoSettingsRequest{Id: "does-not-exist"}))
	if got := connect.CodeOf(err); got != connect.CodeNotFound {
		t.Fatalf("GetRepoSettings(missing id) code = %v, want NotFound (err=%v)", got, err)
	}
}
