package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
	"github.com/recurser/bossalib/kit/vcs"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dbtest"
)

// Sizes measured on an operator's daemon database (#3210's verdict, finding
// 5): up to 2,672 snapshots on one head and 3,144 on one session, with
// checks of about 6 KB a poll.
const (
	benchHeadPolls     = 2672
	benchSessionPolls  = 3144
	benchOtherSessions = 120
	benchOtherPolls    = 300
	benchChecks        = 24
)

// benchChecksJSON is one poll's checks: benchChecks ordinary checks and a
// boss/verify status, about 6 KB.
func benchChecksJSON(b *testing.B, verify vcs.CheckResult) string {
	b.Helper()
	success := vcs.CheckConclusionSuccess
	checks := make([]vcs.CheckResult, 0, benchChecks+1)
	for i := range benchChecks {
		checks = append(checks, vcs.CheckResult{
			ID: fmt.Sprintf("check-run-%08d", i), Name: fmt.Sprintf("ci / build-and-test (shard %d of %d, ubuntu-latest, go 1.26)", i, benchChecks),
			Status: vcs.CheckStatusCompleted, Conclusion: &success,
			Description: "All tests passed in the sharded workspace run, including race detection and coverage collection for every module",
		})
	}
	raw, err := json.Marshal(append(checks, verify))
	if err != nil {
		b.Fatal(err)
	}
	return string(raw)
}

// benchSnapshotDB is a file-backed daemon database holding benchOtherSessions
// sessions of benchOtherPolls polls each, and session "hot" whose newest
// benchHeadPolls of benchSessionPolls polls are on head "head" with status
// headStatus and checks headChecks.
func benchSnapshotDB(b *testing.B, headStatus vcs.DisplayStatus, headChecks string) *sql.DB {
	b.Helper()
	conn, err := sqlutil.Open(filepath.Join(b.TempDir(), "bossd.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = conn.Close() })
	conn.SetMaxOpenConns(1)
	dbtest.Apply(b, conn)
	if _, err := conn.Exec(`PRAGMA foreign_keys = OFF`); err != nil {
		b.Fatal(err)
	}
	pending := benchChecksJSON(b, vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusInProgress, Description: "verifying"})
	tx, err := conn.Begin()
	if err != nil {
		b.Fatal(err)
	}
	insert, err := tx.Prepare(`INSERT INTO session_check_snapshots (session_id, polled_at, head_sha, raw_json, computed_status) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		b.Fatal(err)
	}
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	add := func(session, head string, poll int, raw string, status vcs.DisplayStatus) {
		if _, err := insert.Exec(session, sqlutil.FormatTime(base.Add(time.Duration(poll)*30*time.Second)), head, raw, int(status)); err != nil {
			b.Fatal(err)
		}
	}
	// Sessions poll in turn, as the display poller does, so one session's
	// rows are spread through the table.
	for poll := range benchSessionPolls {
		for s := range benchOtherSessions {
			if poll < benchOtherPolls {
				add(fmt.Sprintf("other-%03d", s), fmt.Sprintf("head-%03d", s), poll, pending[:2048], vcs.DisplayStatusPassing)
			}
		}
		if poll < benchSessionPolls-benchHeadPolls {
			add("hot", "older-head", poll, pending, vcs.DisplayStatusChecking)
		} else {
			add("hot", "head", poll, headChecks, headStatus)
		}
	}
	if err := insert.Close(); err != nil {
		b.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	return conn
}

// BenchmarkHydrateHeadSince measures the snapshot read session hydration does
// for a parked head and a verified head: before, the session index and every
// row's checks; after, the head index and only the trailing runs; tracked,
// one indexed read of persisted run starts.
//
//	go test -run '^$' -bench HydrateHeadSince -benchtime 20x ./api/daemon/
func BenchmarkHydrateHeadSince(b *testing.B) {
	success := vcs.CheckConclusionSuccess
	ctx := context.Background()
	for _, head := range []struct {
		name   string
		status vcs.DisplayStatus
		verify vcs.CheckResult
	}{
		{"parked", vcs.DisplayStatusNeedsHuman, vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusInProgress, Description: "needs human: human-review:ui"}},
		{"verified", vcs.DisplayStatusPassing, vcs.CheckResult{Name: vcs.VerifyStatusContext, Status: vcs.CheckStatusCompleted, Conclusion: &success, Description: "verified"}},
	} {
		conn := benchSnapshotDB(b, head.status, benchChecksJSON(b, head.verify))
		store := db.NewCheckSnapshotStore(conn)
		start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(benchSessionPolls-benchHeadPolls) * 30 * time.Second)
		var wantPark, wantVerified time.Time
		if head.status == vcs.DisplayStatusNeedsHuman {
			wantPark = start
		} else {
			wantVerified = start
		}
		b.Run(head.name+"/before", func(b *testing.B) {
			if _, err := conn.Exec(`DROP INDEX IF EXISTS idx_session_check_snapshots_session_id_head_sha_polled_at_computed_status`); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				// The read and walk hydration did before the head index: every
				// poll of the head with its checks, each verified one decoded.
				snaps, err := store.ListBySessionHead(ctx, "hot", "head")
				if err != nil || len(snaps) != benchHeadPolls {
					b.Fatalf("read %d snapshots, err %v", len(snaps), err)
				}
				wantPark, wantVerified = time.Time{}, time.Time{}
				if vcs.DisplayStatus(snaps[len(snaps)-1].ComputedStatus) == vcs.DisplayStatusNeedsHuman {
					wantPark = trailingRunStart(snaps)
				}
				for i := len(snaps) - 1; i >= 0 && vcs.VerifyPassed(snapshotChecks(snaps[i])); i-- {
					wantVerified = snaps[i].PolledAt
				}
			}
		})
		b.Run(head.name+"/after", func(b *testing.B) {
			if _, err := conn.Exec(`CREATE INDEX IF NOT EXISTS idx_session_check_snapshots_session_id_head_sha_polled_at_computed_status ON session_check_snapshots (session_id, head_sha, polled_at, computed_status)`); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				park, verified := storedHeadSince(ctx, store, "hot", "head")
				if !park.Equal(wantPark) || !verified.Equal(wantVerified) || park.IsZero() == verified.IsZero() {
					b.Fatalf("park %v verified %v, want %v and %v", park, verified, wantPark, wantVerified)
				}
			}
		})
		b.Run(head.name+"/tracked", func(b *testing.B) {
			start := sqlutil.FormatTime(start)
			var verified any
			if head.name == "verified" {
				verified = start
			}
			if _, err := conn.Exec(`UPDATE session_check_snapshots SET status_run_started_at = ?, verified_run_started_at = ? WHERE session_id = 'hot' AND head_sha = 'head'`, start, verified); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				park, verified := storedHeadSince(ctx, store, "hot", "head")
				if !park.Equal(wantPark) || !verified.Equal(wantVerified) || park.IsZero() == verified.IsZero() {
					b.Fatalf("park %v verified %v, want %v and %v", park, verified, wantPark, wantVerified)
				}
			}
		})
	}
}
