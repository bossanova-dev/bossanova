package db

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/recurser/bossalib/kit/vcs"
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
		if err := store.Create(ctx, CheckSnapshot{SessionID: sessionID, HeadSHA: head, PolledAt: base.Add(time.Duration(minute) * time.Minute), RawJSON: "[]"}); err != nil {
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

func TestCheckSnapshotStore_ListBySessionHeadNewestFirst(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	repo := createTestRepo(t, NewRepoStore(db))
	sessions := NewSessionStore(db)
	var ids []string
	for _, title := range []string{"one", "two"} {
		sess, err := sessions.Create(ctx, CreateSessionParams{
			RepoID: repo.ID, Title: title, WorktreePath: "/tmp/wt/" + title, BranchName: "feat/" + title, BaseBranch: "main",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		ids = append(ids, sess.ID)
	}
	s1, s2 := ids[0], ids[1]

	store := NewCheckSnapshotStore(db)
	base := time.Date(2026, 10, 13, 23, 0, 0, 0, time.UTC)
	insert := func(sessionID, head string, minute, status int) {
		t.Helper()
		if err := store.Create(ctx, CheckSnapshot{SessionID: sessionID, HeadSHA: head, PolledAt: base.Add(time.Duration(minute) * time.Minute), RawJSON: `["checks"]`, ComputedStatus: status}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	insert(s1, "head-b", 21, 1)
	insert(s1, "head-a", 40, 9)
	insert(s1, "head-b", 30, 3)
	insert(s1, "head-b", 25, 2)
	insert(s2, "head-b", 35, 9)

	collect := func(withRawJSON bool, limit int) (minutes, statuses []int, raws []string) {
		t.Helper()
		err := store.ListBySessionHeadNewestFirst(ctx, s1, "head-b", withRawJSON, func(snap CheckSnapshot) bool {
			if snap.SessionID != s1 || snap.HeadSHA != "head-b" {
				t.Fatalf("visited %s/%s, want only %s/head-b", snap.SessionID, snap.HeadSHA, s1)
			}
			minutes = append(minutes, int(snap.PolledAt.Sub(base)/time.Minute))
			statuses = append(statuses, snap.ComputedStatus)
			raws = append(raws, snap.RawJSON)
			return len(minutes) < limit
		})
		if err != nil {
			t.Fatalf("ListBySessionHeadNewestFirst: %v", err)
		}
		return minutes, statuses, raws
	}
	minutes, statuses, raws := collect(true, 10)
	if !slices.Equal(minutes, []int{30, 25, 21}) || !slices.Equal(statuses, []int{3, 2, 1}) || !slices.Equal(raws, []string{`["checks"]`, `["checks"]`, `["checks"]`}) {
		t.Fatalf("visited minutes %v statuses %v raws %q, want newest first with checks", minutes, statuses, raws)
	}
	if minutes, _, raws := collect(false, 10); !slices.Equal(minutes, []int{30, 25, 21}) || !slices.Equal(raws, []string{"", "", ""}) {
		t.Fatalf("without checks: minutes %v raws %q, want every poll and no checks", minutes, raws)
	}
	if minutes, _, _ := collect(true, 2); !slices.Equal(minutes, []int{30, 25}) {
		t.Fatalf("a visit that stops at the second poll saw %v", minutes)
	}
	visited := 0
	if err := store.ListBySessionHeadNewestFirst(ctx, s1, "", true, func(CheckSnapshot) bool { visited++; return true }); err != nil || visited != 0 {
		t.Fatalf("empty head: visited %d, err %v; want none", visited, err)
	}
}

// TestCheckSnapshotStore_HeadReadUsesTheHeadIndex pins the plan session
// hydration relies on: the head read narrows by the head index, and without
// the checks it reads that index alone.
func TestCheckSnapshotStore_HeadReadUsesTheHeadIndex(t *testing.T) {
	db := setupTestDB(t)
	const index = "idx_session_check_snapshots_session_id_head_sha_polled_at_computed_status"
	for _, tc := range []struct {
		columns string
		want    string
	}{
		{"id, session_id, polled_at, head_sha, '', computed_status", "USING COVERING INDEX " + index},
		{"id, session_id, polled_at, head_sha, raw_json, computed_status", "USING INDEX " + index},
		{"computed_status, status_run_started_at, verified_run_started_at", "USING INDEX " + index},
	} {
		rows, err := db.Query(`EXPLAIN QUERY PLAN SELECT ` + tc.columns + ` FROM session_check_snapshots
			WHERE session_id = 's' AND head_sha = 'h' ORDER BY polled_at DESC, id DESC LIMIT 1`)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "SCAN") {
				t.Errorf("plan scans rows: %s", detail)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if !slices.ContainsFunc(plan, func(d string) bool { return strings.Contains(d, "SEARCH") && strings.Contains(d, tc.want) }) {
			t.Errorf("plan for %s = %q, want %q", tc.columns, plan, tc.want)
		}
	}
}

func TestCheckSnapshotStore_RunStartsMatchWalk(t *testing.T) {
	park, active := int(vcs.DisplayStatusNeedsHuman), int(vcs.DisplayStatusChecking)
	type poll struct {
		session, minute, status int
		head, raw               string
		legacy, untracked       bool
	}
	success := vcs.CheckConclusionSuccess
	passed, err := json.Marshal([]vcs.CheckResult{{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success}})
	if err != nil {
		t.Fatal(err)
	}
	green := string(passed)
	for _, tc := range []struct {
		name  string
		polls []poll
	}{
		{"park resumes", []poll{
			{minute: 1, status: park}, {minute: 2, status: park},
			{minute: 3, status: active}, {minute: 4, status: park}, {minute: 5, status: park},
		}},
		{"verified resumes", []poll{
			{minute: 1, raw: green}, {minute: 2, raw: green}, {minute: 3, raw: "[]"},
			{minute: 4, raw: green}, {minute: 5, raw: green},
		}},
		{"interleaved heads and sessions", []poll{
			{session: 0, head: "a", minute: 1, status: park, raw: green},
			{session: 1, head: "a", minute: 2, status: active},
			{session: 0, head: "b", minute: 3, status: active, raw: green},
			{session: 1, head: "b", minute: 4, status: park},
			{session: 0, head: "a", minute: 5, status: park, raw: green},
			{session: 1, head: "a", minute: 6, status: active, raw: green},
			{session: 0, head: "b", minute: 7, status: park, raw: green},
			{session: 1, head: "b", minute: 8, status: park, raw: green},
		}},
		{"out of order then reseeded", []poll{
			{minute: 1, status: park, raw: green}, {minute: 3, status: park, raw: green},
			{minute: 2, status: active, untracked: true},
			{minute: 4, status: park, raw: green}, {minute: 5, status: park, raw: green},
		}},
		{"legacy then seeded", []poll{
			{minute: 1, status: park, raw: green},
			{minute: 2, status: park, raw: green, legacy: true, untracked: true},
			{minute: 3, status: park, raw: green}, {minute: 4, status: park, raw: green},
		}},
		{"legacy status and verify runs differ", []poll{
			{minute: 1, status: active, raw: green}, {minute: 2, status: park, raw: green},
			{minute: 3, status: park, legacy: true, untracked: true},
			{minute: 4, status: park, raw: green},
		}},
		{"equal timestamps follow insertion order", []poll{
			{minute: 1, status: park, raw: green}, {minute: 1, status: active},
			{minute: 1, status: park, raw: green}, {minute: 2, status: park, raw: green},
		}},
		{"invalid and unrelated checks break verify", []poll{
			{minute: 1, raw: green}, {minute: 2, raw: "invalid boss/verify"},
			{minute: 3, raw: green}, {minute: 4, raw: `[{"Name":"ci","Status":2,"Conclusion":0}]`},
			{minute: 5, raw: green},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupTestDB(t)
			ctx := context.Background()
			repo := createTestRepo(t, NewRepoStore(db))
			var sessionIDs []string
			for _, title := range []string{"first", "second"} {
				sess, err := NewSessionStore(db).Create(ctx, CreateSessionParams{RepoID: repo.ID, Title: title, WorktreePath: "/tmp/wt/" + title, BranchName: "feat/" + title, BaseBranch: "main"})
				if err != nil {
					t.Fatal(err)
				}
				sessionIDs = append(sessionIDs, sess.ID)
			}
			store := NewCheckSnapshotStore(db)
			status, since, verified, tracked, err := store.GetAttentionStartsBySessionHead(ctx, sessionIDs[0], "missing")
			if err != nil || !tracked || status != 0 || !since.IsZero() || !verified.IsZero() {
				t.Fatalf("missing head = %d %v %v tracked %v err %v", status, since, verified, tracked, err)
			}
			base := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
			for i, p := range tc.polls {
				head := p.head
				if head == "" {
					head = "a"
				}
				sessionID := sessionIDs[p.session]
				if err := store.Create(ctx, CheckSnapshot{SessionID: sessionID, HeadSHA: head, PolledAt: base.Add(time.Duration(p.minute) * time.Minute), ComputedStatus: p.status, RawJSON: p.raw}); err != nil {
					t.Fatal(err)
				}
				if p.legacy {
					if _, err := db.ExecContext(ctx, `UPDATE session_check_snapshots SET status_run_started_at = NULL, verified_run_started_at = NULL WHERE session_id = ? AND head_sha = ?`, sessionID, head); err != nil {
						t.Fatal(err)
					}
				}
				gotStatus, gotSince, gotVerified, tracked, err := store.GetAttentionStartsBySessionHead(ctx, sessionID, head)
				if err != nil || tracked == p.untracked {
					t.Fatalf("poll %d: tracked %v, err %v; want tracked %v", i, tracked, err, !p.untracked)
				}
				if !tracked {
					continue
				}
				rows, err := store.ListBySessionHead(ctx, sessionID, head)
				if err != nil {
					t.Fatal(err)
				}
				wantStatus, wantSince, wantVerified := snapshotRunStartsWalk(rows)
				if gotStatus != wantStatus || !gotSince.Equal(wantSince) || !gotVerified.Equal(wantVerified) {
					t.Fatalf("poll %d: status/start/verified = %d/%v/%v, want %d/%v/%v", i, gotStatus, gotSince, gotVerified, wantStatus, wantSince, wantVerified)
				}
			}
		})
	}
}

// snapshotRunStartsWalk derives both runs independently from the stored timestamps.
func snapshotRunStartsWalk(rows []CheckSnapshot) (status int, statusStarted, verifiedStarted time.Time) {
	if len(rows) == 0 {
		return
	}
	status = rows[len(rows)-1].ComputedStatus
	for i := len(rows) - 1; i >= 0 && rows[i].ComputedStatus == status; i-- {
		statusStarted = rows[i].PolledAt
	}
	for i := len(rows) - 1; i >= 0; i-- {
		var checks []vcs.CheckResult
		if json.Unmarshal([]byte(rows[i].RawJSON), &checks) != nil || !vcs.VerifyPassed(checks) {
			break
		}
		verifiedStarted = rows[i].PolledAt
	}
	return
}
