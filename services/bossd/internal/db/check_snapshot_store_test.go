package db

import (
	"context"
	"testing"
	"time"
)

func TestCheckSnapshotStore_ListBySessionHead(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))
	sessions := NewSessionStore(db)
	newSession := func(title string) string {
		t.Helper()
		sess, err := sessions.Create(ctx, CreateSessionParams{
			RepoID: repo.ID, Title: title, WorktreePath: "/tmp/wt/" + title, BranchName: "feat/" + title, BaseBranch: "main",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return sess.ID
	}
	s1, s2 := newSession("one"), newSession("two")

	store := NewCheckSnapshotStore(db)
	base := time.Date(2026, 10, 13, 23, 0, 0, 0, time.UTC)
	insert := func(sessionID, head string, minute int) {
		t.Helper()
		if err := store.Insert(ctx, CheckSnapshot{SessionID: sessionID, HeadSHA: head, PolledAt: base.Add(time.Duration(minute) * time.Minute), RawJSON: "[]"}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	// Inserted out of order: the read orders by poll time, oldest first.
	insert(s1, "head-b", 30)
	insert(s1, "head-a", 10)
	insert(s1, "head-b", 21)
	insert(s1, "head-b", 25)
	insert(s2, "head-b", 22)

	got, err := store.ListBySessionHead(ctx, s1, "head-b")
	if err != nil {
		t.Fatalf("ListBySessionHead: %v", err)
	}
	var minutes []int
	for _, snap := range got {
		if snap.SessionID != s1 || snap.HeadSHA != "head-b" {
			t.Fatalf("got a snapshot for %s/%s, want only %s/head-b", snap.SessionID, snap.HeadSHA, s1)
		}
		minutes = append(minutes, int(snap.PolledAt.Sub(base)/time.Minute))
	}
	if want := []int{21, 25, 30}; len(minutes) != len(want) || minutes[0] != want[0] || minutes[1] != want[1] || minutes[2] != want[2] {
		t.Fatalf("poll minutes = %v, want %v (oldest first)", minutes, want)
	}

	none, err := store.ListBySessionHead(ctx, s1, "")
	if err != nil || len(none) != 0 {
		t.Fatalf("empty head: got %d snapshots, err %v; want none", len(none), err)
	}
}
