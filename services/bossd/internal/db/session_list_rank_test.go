package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/recurser/bossalib/sqlutil"
)

// listRankFixture builds n sessions in one repo and returns the store plus the
// handle needed to pin created_at and read list_rank back raw.
type listRankFixture struct {
	db     *sql.DB
	store  *SQLiteSessionStore
	repoID string
	ctx    context.Context
}

func newListRankFixture(t *testing.T) *listRankFixture {
	t.Helper()
	database := setupTestDB(t)
	repo := createTestRepo(t, NewRepoStore(database))
	return &listRankFixture{
		db:     database,
		store:  NewSessionStore(database),
		repoID: repo.ID,
		ctx:    context.Background(),
	}
}

// add creates a session and pins its created_at, so a fixture can express an
// explicit natural order (and a deliberate created_at TIE).
func (f *listRankFixture) add(t *testing.T, id string, createdAt time.Time) {
	t.Helper()
	sess, err := f.store.Create(f.ctx, CreateSessionParams{
		RepoID:       f.repoID,
		Title:        id,
		WorktreePath: "/tmp/wt/" + id,
		BranchName:   "feat/" + id,
		BaseBranch:   "main",
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
	if _, err := f.db.Exec(`UPDATE sessions SET id = ?, created_at = ? WHERE id = ?`,
		id, createdAt.UTC().Format(sqlutil.TimeLayout), sess.ID); err != nil {
		t.Fatalf("pin session %s: %v", id, err)
	}
}

func (f *listRankFixture) setRank(t *testing.T, id string, rank int64) {
	t.Helper()
	if _, err := f.db.Exec(`UPDATE sessions SET list_rank = ? WHERE id = ?`, rank, id); err != nil {
		t.Fatalf("set rank on %s: %v", id, err)
	}
}

// rawRanks reads every row's list_rank straight from SQL, so a test can assert
// that a write left the OTHER rows byte-identical.
func (f *listRankFixture) rawRanks(t *testing.T) map[string]*int64 {
	t.Helper()
	rows, err := f.db.Query(`SELECT id, list_rank FROM sessions`)
	if err != nil {
		t.Fatalf("read raw ranks: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]*int64{}
	for rows.Next() {
		var id string
		var rank *int64
		if err := rows.Scan(&id, &rank); err != nil {
			t.Fatalf("scan raw rank: %v", err)
		}
		out[id] = rank
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("raw ranks: %v", err)
	}
	return out
}

// activeIDs is the order ListActive renders — the read behind the TUI list.
func (f *listRankFixture) activeIDs(t *testing.T) []string {
	t.Helper()
	sessions, err := f.store.ListActive(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	ids := make([]string, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return ids
}

// activeWithRepoIDs is the order ListActiveWithRepo renders — the read
// loadSessionsForList actually calls for a non-archived ListSessions.
func (f *listRankFixture) activeWithRepoIDs(t *testing.T) []string {
	t.Helper()
	rows, err := f.store.ListActiveWithRepo(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("list active with repo: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

func equalIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func at(day int) time.Time {
	return time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC)
}

// TestSessionListOrder_AllUnrankedKeepsNaturalOrder is the regression guard for
// R2: adding the column must not reorder an untouched database. The fixture
// deliberately contains a created_at TIE (b1/b2) so the id tie-break is
// exercised rather than assumed — without it the assertion would pass on a
// comparator that never reached the tie-break at all.
func TestSessionListOrder_AllUnrankedKeepsNaturalOrder(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "newest", at(9))
	f.add(t, "b2", at(5))
	f.add(t, "b1", at(5)) // same created_at as b2; id ASC breaks the tie
	f.add(t, "oldest", at(1))

	want := []string{"newest", "b1", "b2", "oldest"}
	for _, tc := range []struct {
		name string
		got  []string
	}{
		{"ListActive", f.activeIDs(t)},
		{"ListActiveWithRepo", f.activeWithRepoIDs(t)},
	} {
		if !equalIDs(tc.got, want) {
			t.Errorf("%s = %v, want created_at DESC then id ASC %v", tc.name, tc.got, want)
		}
	}
}

// TestSessionListOrder_RankedSortsAheadOfEveryUnranked proves R3's first half
// with the hardest fixture the rule allows: the ranked row is the OLDEST
// session, so a comparator that consulted created_at before the rank would
// leave it last instead of first.
func TestSessionListOrder_RankedSortsAheadOfEveryUnranked(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "newest", at(9))
	f.add(t, "middle", at(5))
	f.add(t, "oldest", at(1))
	f.setRank(t, "oldest", ListRankGap)

	want := []string{"oldest", "newest", "middle"}
	if got := f.activeWithRepoIDs(t); !equalIDs(got, want) {
		t.Errorf("order = %v, want the ranked oldest row first: %v", got, want)
	}
}

// TestSessionListOrder_RankedSortByRankAscending uses THREE ranked rows whose
// rank order is the exact reverse of their created_at order, so neither a
// pairwise-only comparator nor one that fell through to created_at can pass.
func TestSessionListOrder_RankedSortByRankAscending(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "r-third", at(9)) // newest, but ranked last
	f.add(t, "r-second", at(5))
	f.add(t, "r-first", at(1)) // oldest, but ranked first
	f.add(t, "plain", at(7))

	f.setRank(t, "r-first", 1*ListRankGap)
	f.setRank(t, "r-second", 2*ListRankGap)
	f.setRank(t, "r-third", 3*ListRankGap)

	want := []string{"r-first", "r-second", "r-third", "plain"}
	if got := f.activeWithRepoIDs(t); !equalIDs(got, want) {
		t.Errorf("order = %v, want rank ascending then the natural block: %v", got, want)
	}
}

// TestSessionListOrder_NewSessionDoesNotDisplaceRanked is R4: a session the
// user pushed to the top stays at the top when a newer session arrives, which
// is the reporter's whole stated use case.
func TestSessionListOrder_NewSessionDoesNotDisplaceRanked(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "pinned", at(1))
	f.add(t, "existing", at(2))
	f.setRank(t, "pinned", ListRankGap)

	before := f.activeWithRepoIDs(t)
	if !equalIDs(before, []string{"pinned", "existing"}) {
		t.Fatalf("order before = %v, want [pinned existing]", before)
	}

	f.add(t, "brand-new", at(28))

	want := []string{"pinned", "brand-new", "existing"}
	if got := f.activeWithRepoIDs(t); !equalIDs(got, want) {
		t.Errorf("order after a newer session = %v, want the pinned row still first: %v", got, want)
	}
}

// TestSetListRanks_WritesExactlyOneRow is R5. It snapshots every row's raw
// list_rank, performs a one-entry write, and asserts every OTHER row's column
// is unchanged — including the rows that were already ranked, which a
// renumbering implementation would have rewritten.
func TestSetListRanks_WritesExactlyOneRow(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "a", at(4))
	f.add(t, "b", at(3))
	f.add(t, "c", at(2))
	f.add(t, "d", at(1))
	f.setRank(t, "a", 1*ListRankGap)
	f.setRank(t, "c", 2*ListRankGap)

	before := f.rawRanks(t)

	target := 3 * ListRankGap
	updated, err := f.store.SetListRanks(f.ctx, map[string]*int64{"d": &target})
	if err != nil {
		t.Fatalf("set list ranks: %v", err)
	}
	if updated != 1 {
		t.Fatalf("rows updated = %d, want 1", updated)
	}

	after := f.rawRanks(t)
	for id, wantRank := range before {
		if id == "d" {
			continue
		}
		gotRank := after[id]
		if (gotRank == nil) != (wantRank == nil) {
			t.Errorf("sibling %s rank presence changed: %v -> %v", id, wantRank, gotRank)
			continue
		}
		if gotRank != nil && *gotRank != *wantRank {
			t.Errorf("sibling %s rank = %d, want unchanged %d", id, *gotRank, *wantRank)
		}
	}
	if after["d"] == nil || *after["d"] != target {
		t.Errorf("moved row rank = %v, want %d", after["d"], target)
	}
}

// TestSetListRanks_ClearsRankAndLeavesUpdatedAtAlone pins the two halves of the
// write contract that nothing else would catch: a nil value really does clear
// the column (the only way to sort below an unranked row), and a reorder does
// not bump updated_at — the column ListByState/ListByStates order the crash
// recovery scans by.
func TestSetListRanks_ClearsRankAndLeavesUpdatedAtAlone(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "a", at(2))
	f.add(t, "b", at(1))
	f.setRank(t, "b", ListRankGap)

	var updatedBefore string
	if err := f.db.QueryRow(`SELECT updated_at FROM sessions WHERE id = 'b'`).Scan(&updatedBefore); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}

	if _, err := f.store.SetListRanks(f.ctx, map[string]*int64{"b": nil}); err != nil {
		t.Fatalf("clear rank: %v", err)
	}

	if got := f.rawRanks(t)["b"]; got != nil {
		t.Errorf("rank after clear = %d, want NULL", *got)
	}
	var updatedAfter string
	if err := f.db.QueryRow(`SELECT updated_at FROM sessions WHERE id = 'b'`).Scan(&updatedAfter); err != nil {
		t.Fatalf("re-read updated_at: %v", err)
	}
	if updatedAfter != updatedBefore {
		t.Errorf("updated_at moved on a reorder: %q -> %q", updatedBefore, updatedAfter)
	}
	if got := f.activeWithRepoIDs(t); !equalIDs(got, []string{"a", "b"}) {
		t.Errorf("order after clear = %v, want the natural order [a b]", got)
	}
}

// TestSessionListOrder_NotADefectReadsKeepTheirOrder pins the enumeration's
// "not a defect" verdicts as BEHAVIOUR rather than as a grep: a ranked row must
// NOT jump to the front of the archived list or the state-recovery scan.
func TestSessionListOrder_NotADefectReadsKeepTheirOrder(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "newer", at(9))
	f.add(t, "older", at(1))
	f.setRank(t, "older", ListRankGap)
	for _, id := range []string{"newer", "older"} {
		if _, err := f.db.Exec(`UPDATE sessions SET archived_at = ? WHERE id = ?`,
			at(10).Format(sqlutil.TimeLayout), id); err != nil {
			t.Fatalf("archive %s: %v", id, err)
		}
	}

	archived, err := f.store.ListArchived(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("list archived: %v", err)
	}
	got := make([]string, 0, len(archived))
	for _, s := range archived {
		got = append(got, s.ID)
	}
	if !equalIDs(got, []string{"newer", "older"}) {
		t.Errorf("archived order = %v, want created_at DESC [newer older] — a manual rank on an archived row has no meaning", got)
	}
}
