package db

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

func TestSetTrackerRank(t *testing.T) {
	database := setupTestDB(t)
	store := NewSessionStore(database)
	repo := createTestRepo(t, NewRepoStore(database))
	ctx := context.Background()
	ticket := "BOS-1"
	sess, err := store.Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: "rank", BranchName: "rank", BaseBranch: "main", TrackerID: &ticket})
	if err != nil {
		t.Fatal(err)
	}
	if sess.TrackerRank != nil {
		t.Fatal("new session is ranked")
	}
	deadline := "2026-11-30"
	rank := models.TrackerRank{Tier: 1, TierSource: "blocks BOS-2", DeadlineOn: &deadline, DeadlineSource: "project", IsScheduled: true, UnblockedTicketCount: 3, RankedAt: time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)}
	for _, expected := range []string{"BOS-2", ticket} {
		applied, err := store.SetTrackerRank(ctx, sess.ID, expected, nil, rank)
		if err != nil || applied != (expected == ticket) {
			t.Fatalf("write %s: %v %v", expected, applied, err)
		}
		got, err := store.Get(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if expected != ticket {
			if got.TrackerRank != nil {
				t.Fatal("mismatched write ranked session")
			}
		} else if !reflect.DeepEqual(got.TrackerRank, &rank) {
			t.Fatalf("rank = %+v, want %+v", got.TrackerRank, rank)
		}
	}
	changed := rank
	changed.Tier = 4
	if ok, err := store.SetTrackerRank(ctx, sess.ID, "BOS-2", nil, changed); err != nil || ok {
		t.Fatalf("stale write %v %v", ok, err)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil || !reflect.DeepEqual(got.TrackerRank, &rank) {
		t.Fatalf("stale write changed rank: %+v %v", got, err)
	}
	rank.DeadlineOn = nil
	rank.DeadlineSource = ""
	rank.IsScheduled = false
	if ok, err := store.SetTrackerRank(ctx, sess.ID, ticket, nil, rank); err != nil || !ok {
		t.Fatalf("undated %v %v", ok, err)
	}
	got, err = store.Get(ctx, sess.ID)
	if err != nil || !reflect.DeepEqual(got.TrackerRank, &rank) {
		t.Fatalf("undated rank: %+v %v", got, err)
	}
}

func TestTrackerRankInvalidatedByLinkCorrection(t *testing.T) {
	database := setupTestDB(t)
	store := NewSessionStore(database)
	repo := createTestRepo(t, NewRepoStore(database))
	ctx := context.Background()
	ticket, url := "GH-12", "https://github.com/acme/repo/issues/12"
	sess, err := store.Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: "rank", BranchName: "rank", BaseBranch: "main", TrackerID: &ticket, TrackerURL: &url})
	if err != nil {
		t.Fatal(err)
	}
	rank := models.TrackerRank{Tier: 1, TierSource: "own", RankedAt: time.Now().UTC()}
	for _, tc := range []struct {
		name     string
		id, url  *string
		wantRank bool
	}{
		{name: "unrelated update", wantRank: true},
		{name: "same link", id: &ticket, url: &url, wantRank: true},
		{name: "changed repository", url: rankURL("https://github.com/other/repo/issues/12")},
		{name: "changed ticket", id: rankURL("GH-13")},
		{name: "removed link", id: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalID, originalURL := &ticket, &url
			if _, err := store.Update(ctx, sess.ID, domain.UpdateSessionParams{TrackerID: &originalID, TrackerURL: &originalURL}); err != nil {
				t.Fatal(err)
			}
			if ok, err := store.SetTrackerRank(ctx, sess.ID, ticket, &url, rank); err != nil || !ok {
				t.Fatalf("seed rank: %v %v", ok, err)
			}
			p := domain.UpdateSessionParams{}
			if tc.name != "unrelated update" && tc.name != "changed repository" {
				p.TrackerID = &tc.id
			}
			if tc.url != nil {
				p.TrackerURL = &tc.url
			}
			got, err := store.Update(ctx, sess.ID, p)
			if err != nil || (got.TrackerRank != nil) != tc.wantRank {
				t.Fatalf("updated rank = %+v, want ranked %v: %v", got, tc.wantRank, err)
			}
			got, err = store.Get(ctx, sess.ID)
			if err != nil || (got.TrackerRank != nil) != tc.wantRank {
				t.Fatalf("stored rank = %+v, want ranked %v: %v", got, tc.wantRank, err)
			}
		})
	}
}

func rankURL(s string) *string { return &s }

func TestTrackerRankRejectsStaleRepositorySnapshot(t *testing.T) {
	database := setupTestDB(t)
	store := NewSessionStore(database)
	repo := createTestRepo(t, NewRepoStore(database))
	ctx := context.Background()
	ticket, url := "GH-12", "https://github.com/acme/repo/issues/12"
	sess, err := store.Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: "rank", BranchName: "rank", BaseBranch: "main", TrackerID: &ticket, TrackerURL: &url})
	if err != nil {
		t.Fatal(err)
	}
	expectedURL := sess.TrackerURL
	correctedURL := rankURL("https://github.com/other/repo/issues/12")
	if _, err := store.Update(ctx, sess.ID, domain.UpdateSessionParams{TrackerURL: &correctedURL}); err != nil {
		t.Fatal(err)
	}
	rank := models.TrackerRank{Tier: 1, TierSource: "own", RankedAt: time.Now().UTC()}
	for _, snapshot := range []*string{expectedURL, nil} {
		if applied, err := store.SetTrackerRank(ctx, sess.ID, ticket, snapshot, rank); err != nil || applied {
			t.Fatalf("accepted stale repository snapshot %v: applied=%v err=%v", snapshot, applied, err)
		}
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil || got.TrackerRank != nil {
		t.Fatalf("stale write changed rank: %+v %v", got, err)
	}
	if applied, err := store.SetTrackerRank(ctx, sess.ID, ticket, correctedURL, rank); err != nil || !applied {
		t.Fatalf("matching repository snapshot rejected: %v %v", applied, err)
	}
}
