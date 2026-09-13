package db

import (
	"context"
	"testing"
)

func rankPtr(v int64) *int64 { return &v }

// row is a compact constructor for comparator fixtures.
func row(id string, rank *int64, createdAt string) SessionListRankRow {
	return SessionListRankRow{ID: id, ListRank: rank, CreatedAt: createdAt}
}

// renderIDs applies the move to the fixture and returns the resulting order, so
// a case can assert the RENDERED consequence rather than a bare rank value.
func renderIDs(rows []SessionListRankRow, move ListRankMove) []string {
	next := make([]SessionListRankRow, len(rows))
	copy(next, rows)
	for i := range next {
		if rank, ok := move.Ranks[next[i].ID]; ok {
			next[i].ListRank = rank
		}
	}
	SortSessionListRankRows(next)
	ids := make([]string, 0, len(next))
	for _, r := range next {
		ids = append(ids, r.ID)
	}
	return ids
}

func idsOf(rows []SessionListRankRow) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}

// TestComputeListRankMove is the table of every branch the move arithmetic has,
// asserted on the RENDERED order and on how many rows the move writes — the
// second half being R5, which a rank-value assertion alone would not catch.
func TestComputeListRankMove(t *testing.T) {
	// A ranked block of three above a natural block of three.
	ranked := func() []SessionListRankRow {
		return []SessionListRankRow{
			row("p1", rankPtr(1*ListRankGap), "2026-09-01T12:00:00.000Z"),
			row("p2", rankPtr(2*ListRankGap), "2026-09-02T12:00:00.000Z"),
			row("p3", rankPtr(3*ListRankGap), "2026-09-03T12:00:00.000Z"),
			row("n1", nil, "2026-09-09T12:00:00.000Z"),
			row("n2", nil, "2026-09-08T12:00:00.000Z"),
			row("n3", nil, "2026-09-07T12:00:00.000Z"),
		}
	}
	plain := func() []SessionListRankRow {
		return []SessionListRankRow{
			row("a", nil, "2026-09-09T12:00:00.000Z"),
			row("b", nil, "2026-09-08T12:00:00.000Z"),
			row("c", nil, "2026-09-07T12:00:00.000Z"),
		}
	}

	tests := []struct {
		name      string
		rows      []SessionListRankRow
		index     int
		up        bool
		wantOrder []string
		wantWrite int // rows the move writes
	}{
		{
			name: "up at the top is a no-op", rows: ranked(), index: 0, up: true,
			wantOrder: []string{"p1", "p2", "p3", "n1", "n2", "n3"}, wantWrite: 0,
		},
		{
			name: "down at the bottom is a no-op", rows: ranked(), index: 5, up: false,
			wantOrder: []string{"p1", "p2", "p3", "n1", "n2", "n3"}, wantWrite: 0,
		},
		{
			name: "up inside the ranked block swaps one position", rows: ranked(), index: 2, up: true,
			wantOrder: []string{"p1", "p3", "p2", "n1", "n2", "n3"}, wantWrite: 1,
		},
		{
			name: "up to the very top of the ranked block", rows: ranked(), index: 1, up: true,
			wantOrder: []string{"p2", "p1", "p3", "n1", "n2", "n3"}, wantWrite: 1,
		},
		{
			name: "down inside the ranked block swaps one position", rows: ranked(), index: 0, up: false,
			wantOrder: []string{"p2", "p1", "p3", "n1", "n2", "n3"}, wantWrite: 1,
		},
		{
			name: "down off the end of the ranked block unranks the row", rows: ranked(), index: 2, up: false,
			// p3 loses its rank and rejoins the natural block at its own
			// created_at position (2026-09-03, the oldest of the four).
			wantOrder: []string{"p1", "p2", "n1", "n2", "n3", "p3"}, wantWrite: 1,
		},
		{
			name: "the first unranked row rises into the ranked block by one", rows: ranked(), index: 3, up: true,
			wantOrder: []string{"p1", "p2", "n1", "p3", "n2", "n3"}, wantWrite: 1,
		},
		{
			name: "the second unranked row rises to the end of the ranked block", rows: ranked(), index: 4, up: true,
			wantOrder: []string{"p1", "p2", "p3", "n2", "n1", "n3"}, wantWrite: 1,
		},
		{
			name: "an unranked row moving down is a no-op", rows: ranked(), index: 3, up: false,
			wantOrder: []string{"p1", "p2", "p3", "n1", "n2", "n3"}, wantWrite: 0,
		},
		{
			name: "with nothing ranked, up pins the row to the top", rows: plain(), index: 2, up: true,
			wantOrder: []string{"c", "a", "b"}, wantWrite: 1,
		},
		{
			name: "with nothing ranked, the second row rises to the top", rows: plain(), index: 1, up: true,
			wantOrder: []string{"b", "a", "c"}, wantWrite: 1,
		},
		{
			name: "with nothing ranked, down is a no-op", rows: plain(), index: 0, up: false,
			wantOrder: []string{"a", "b", "c"}, wantWrite: 0,
		},
		{
			name: "an out-of-range index is a no-op", rows: plain(), index: 7, up: true,
			wantOrder: []string{"a", "b", "c"}, wantWrite: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			move := ComputeListRankMove(tc.rows, tc.index, tc.up)
			if len(move.Ranks) != tc.wantWrite {
				t.Errorf("rows written = %d, want %d (%v)", len(move.Ranks), tc.wantWrite, move.Ranks)
			}
			if got := renderIDs(tc.rows, move); !equalIDs(got, tc.wantOrder) {
				t.Errorf("order after move = %v, want %v", got, tc.wantOrder)
			}
			if move.IsRespaced {
				t.Errorf("a fixture at full spacing must not re-space")
			}
		})
	}
}

// TestComputeListRankMove_UpThenDownRestoresOrder is R6 at the arithmetic
// layer: every starting position in a mixed list must round-trip. The handler
// test proves the same property end to end through the RPC.
func TestComputeListRankMove_UpThenDownRestoresOrder(t *testing.T) {
	base := []SessionListRankRow{
		row("p1", rankPtr(1*ListRankGap), "2026-09-01T12:00:00.000Z"),
		row("p2", rankPtr(2*ListRankGap), "2026-09-02T12:00:00.000Z"),
		row("n1", nil, "2026-09-09T12:00:00.000Z"),
		row("n2", nil, "2026-09-08T12:00:00.000Z"),
	}
	want := idsOf(base)

	apply := func(rows []SessionListRankRow, move ListRankMove) []SessionListRankRow {
		next := make([]SessionListRankRow, len(rows))
		copy(next, rows)
		for i := range next {
			if rank, ok := move.Ranks[next[i].ID]; ok {
				next[i].ListRank = rank
			}
		}
		SortSessionListRankRows(next)
		return next
	}

	roundTripped := 0
	for start := range base {
		id := base[start].ID
		rows := make([]SessionListRankRow, len(base))
		copy(rows, base)

		upMove := ComputeListRankMove(rows, start, true)
		if upMove.IsNoop() {
			// Already first: "up" is a no-op, so the following "down" is a
			// real single move and the order is CORRECTLY not restored. Only
			// a row that actually moved up has a round-trip to assert.
			continue
		}
		roundTripped++
		rows = apply(rows, upMove)
		moved := -1
		for i, r := range rows {
			if r.ID == id {
				moved = i
				break
			}
		}
		if moved < 0 {
			t.Fatalf("%s vanished after moving up", id)
		}
		rows = apply(rows, ComputeListRankMove(rows, moved, false))

		if got := idsOf(rows); !equalIDs(got, want) {
			t.Errorf("moving %s up then down = %v, want the original order %v", id, got, want)
		}
		// The rendered order alone cannot see a STICKY PIN: a previously
		// unranked row that came back to its slot carrying a real rank renders
		// identically today and outranks every session created after it
		// forever. The round trip must restore the ranks byte-identically.
		for i := range rows {
			var startRank *int64
			for _, b := range base {
				if b.ID == rows[i].ID {
					startRank = b.ListRank
				}
			}
			if (rows[i].ListRank == nil) != (startRank == nil) {
				t.Errorf("moving %s up then down left %s ranked=%v, want ranked=%v",
					id, rows[i].ID, rows[i].ListRank != nil, startRank != nil)
				continue
			}
			if rows[i].ListRank != nil && *rows[i].ListRank != *startRank {
				t.Errorf("moving %s up then down changed %s's rank %d -> %d",
					id, rows[i].ID, *startRank, *rows[i].ListRank)
			}
		}
	}
	// Non-vacuity: a fixture where nothing ever moved would pass trivially.
	if roundTripped != len(base)-1 {
		t.Fatalf("rows round-tripped = %d, want every row but the first (%d)", roundTripped, len(base)-1)
	}
}

// TestComputeListRankMove_RespacesAnExhaustedGap covers the one case that
// writes more than one row. Two ranks one apart leave no integer between them,
// so the arithmetic re-numbers the ranked block instead of silently writing a
// duplicate rank (which would leave the move a no-op that reported success).
func TestComputeListRankMove_RespacesAnExhaustedGap(t *testing.T) {
	// p3 must land strictly between p1 (100) and p2 (101). No integer fits, so
	// the sparse key is exhausted exactly there.
	rows := []SessionListRankRow{
		row("p1", rankPtr(100), "2026-09-01T12:00:00.000Z"),
		row("p2", rankPtr(101), "2026-09-02T12:00:00.000Z"),
		row("p3", rankPtr(102), "2026-09-03T12:00:00.000Z"),
		row("n1", nil, "2026-09-09T12:00:00.000Z"),
	}

	move := ComputeListRankMove(rows, 2, true)
	if !move.IsRespaced {
		t.Fatalf("an exhausted gap must re-space; got %v", move.Ranks)
	}
	if len(move.Ranks) != 3 {
		t.Fatalf("re-space wrote %d rows, want the 3 ranked ones", len(move.Ranks))
	}
	if got := renderIDs(rows, move); !equalIDs(got, []string{"p1", "p3", "p2", "n1"}) {
		t.Errorf("order after re-space = %v, want [p1 p3 p2 n1]", got)
	}
	// A re-space must restore full spacing, or the very next move re-exhausts.
	for id, rank := range move.Ranks {
		if rank == nil || *rank%ListRankGap != 0 {
			t.Errorf("re-spaced %s to %v, want a multiple of the full gap", id, rank)
		}
	}
	// The unranked row must not be dragged into the ranked block.
	if _, ok := move.Ranks["n1"]; ok {
		t.Error("re-space wrote a rank onto an unranked row")
	}
}

// TestSessionListRankLess_MatchesSQL pins the Go comparator to the SQL clause.
// The handler sorts in Go and the store sorts in SQL; if the two ever disagree,
// the daemon computes a neighbour the user cannot see.
func TestSessionListRankLess_MatchesSQL(t *testing.T) {
	f := newListRankFixture(t)
	f.add(t, "a-tie", at(5))
	f.add(t, "b-tie", at(5))
	f.add(t, "newest", at(9))
	f.add(t, "ranked-old", at(1))
	f.add(t, "ranked-new", at(8))
	f.setRank(t, "ranked-old", 2*ListRankGap)
	f.setRank(t, "ranked-new", 1*ListRankGap)

	sessions, err := f.store.ListActive(context.Background(), f.repoID)
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	fromSQL := make([]string, 0, len(sessions))
	for _, s := range sessions {
		fromSQL = append(fromSQL, s.ID)
	}

	// Feed the same rows to the Go comparator in a deliberately wrong order.
	shuffled := SessionListRankRowsFrom(sessions)
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	SortSessionListRankRows(shuffled)

	if got := idsOf(shuffled); !equalIDs(got, fromSQL) {
		t.Errorf("Go comparator = %v, SQL clause = %v — the two must agree", got, fromSQL)
	}
}

// TestComputeListRankMove_DoesNotRespaceWhenThereIsRoomBelow guards the other
// side of the re-space branch: a tight pair with nothing beneath it still has
// the whole space below the lowest rank, so the move stays a one-row write.
func TestComputeListRankMove_DoesNotRespaceWhenThereIsRoomBelow(t *testing.T) {
	rows := []SessionListRankRow{
		row("p1", rankPtr(100), "2026-09-01T12:00:00.000Z"),
		row("p2", rankPtr(101), "2026-09-02T12:00:00.000Z"),
		row("n1", nil, "2026-09-09T12:00:00.000Z"),
	}
	move := ComputeListRankMove(rows, 1, true)
	if move.IsRespaced {
		t.Error("re-spaced although there was room below the lowest rank")
	}
	if len(move.Ranks) != 1 {
		t.Errorf("rows written = %d, want 1", len(move.Ranks))
	}
	if got := renderIDs(rows, move); !equalIDs(got, []string{"p2", "p1", "n1"}) {
		t.Errorf("order = %v, want [p2 p1 n1]", got)
	}
}

// TestComputeListRankMove_UpIntoAnExhaustedGapMovesTheRow covers the one UP
// case the re-space path used to drop: an UNRANKED row sitting directly below
// the ranked block, whose target gap has no integer left in it. The moved row
// is not in the ranked block at all, so a re-space that only reorders that
// block renumbers three rows without moving the fourth — and reports success.
func TestComputeListRankMove_UpIntoAnExhaustedGapMovesTheRow(t *testing.T) {
	// n1 must land strictly between p2 (200) and p3 (201): no integer fits.
	rows := []SessionListRankRow{
		row("p1", rankPtr(100), "2026-09-01T12:00:00.000Z"),
		row("p2", rankPtr(200), "2026-09-02T12:00:00.000Z"),
		row("p3", rankPtr(201), "2026-09-03T12:00:00.000Z"),
		row("n1", nil, "2026-09-09T12:00:00.000Z"),
	}

	move := ComputeListRankMove(rows, 3, true)
	if move.IsNoop() {
		t.Fatal("moving the row below the ranked block up wrote nothing")
	}
	if !move.IsRespaced {
		t.Fatalf("an exhausted gap must re-space; got %v", move.Ranks)
	}
	// The moved row is the point of the move: a re-space that leaves it out
	// renumbers the block while the list renders unchanged.
	if _, ok := move.Ranks["n1"]; !ok {
		t.Error("the re-space did not write the moved row, so it did not move")
	}
	if got := renderIDs(rows, move); !equalIDs(got, []string{"p1", "p2", "n1", "p3"}) {
		t.Errorf("order after move = %v, want [p1 p2 n1 p3]", got)
	}
	for id, rank := range move.Ranks {
		if rank == nil || *rank%ListRankGap != 0 {
			t.Errorf("re-spaced %s to %v, want a multiple of the full gap", id, rank)
		}
	}
}

// TestComputeListRankMove_DownFromTheLastRankedRowReportsItsLandingSlot is MF1
// at the arithmetic layer. Clearing the rank is the only way down past an
// unranked neighbour, so the fall is multi-position — but when the cleared row
// renders where it already was, nothing moved and the move must say so.
func TestComputeListRankMove_DownFromTheLastRankedRowReportsItsLandingSlot(t *testing.T) {
	tests := []struct {
		name      string
		rows      []SessionListRankRow
		index     int
		wantWrite int
		wantOrder []string
	}{
		{
			// p1 is older than both unranked rows, so clearing drops it past
			// BOTH of them. Multi-position, and intentional.
			name: "an old ranked row falls past the whole natural block",
			rows: []SessionListRankRow{
				row("p1", rankPtr(1*ListRankGap), "2026-09-01T12:00:00.000Z"),
				row("n1", nil, "2026-09-09T12:00:00.000Z"),
				row("n2", nil, "2026-09-08T12:00:00.000Z"),
			},
			index: 0, wantWrite: 1,
			wantOrder: []string{"n1", "n2", "p1"},
		},
		{
			// p1 is the NEWEST session, so clearing renders it at index 0 —
			// exactly where it already is. The user would see nothing move.
			name: "the newest ranked row cannot fall, so the move is a no-op",
			rows: []SessionListRankRow{
				row("p1", rankPtr(1*ListRankGap), "2026-09-09T12:00:00.000Z"),
				row("n1", nil, "2026-09-08T12:00:00.000Z"),
				row("n2", nil, "2026-09-07T12:00:00.000Z"),
			},
			index: 0, wantWrite: 0,
			wantOrder: []string{"p1", "n1", "n2"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			move := ComputeListRankMove(tc.rows, tc.index, false)
			if len(move.Ranks) != tc.wantWrite {
				t.Errorf("rows written = %d, want %d (%v)", len(move.Ranks), tc.wantWrite, move.Ranks)
			}
			if got := renderIDs(tc.rows, move); !equalIDs(got, tc.wantOrder) {
				t.Errorf("order after move = %v, want %v", got, tc.wantOrder)
			}
		})
	}
}

// TestComputeListRankMove_DownDoesNotPinAnUnrankedRow is MF0: moving a row down
// into the head of the natural block must leave it UNRANKED, because a rank
// there is indistinguishable in the rendered order from no rank at all — and a
// rank outranks every session created later, forever.
func TestComputeListRankMove_DownDoesNotPinAnUnrankedRow(t *testing.T) {
	rows := []SessionListRankRow{
		row("p1", rankPtr(1*ListRankGap), "2026-09-01T12:00:00.000Z"),
		row("x", rankPtr(3*ListRankGap), "2026-09-09T12:00:00.000Z"),
		row("p2", rankPtr(4*ListRankGap), "2026-09-02T12:00:00.000Z"),
		row("n1", nil, "2026-09-08T12:00:00.000Z"),
	}

	move := ComputeListRankMove(rows, 1, false)
	rank, ok := move.Ranks["x"]
	if !ok {
		t.Fatalf("moving x down wrote nothing: %v", move.Ranks)
	}
	if rank != nil {
		t.Errorf("x was left ranked at %d; it renders the same unranked, so it must be cleared", *rank)
	}
	if got := renderIDs(rows, move); !equalIDs(got, []string{"p1", "p2", "x", "n1"}) {
		t.Errorf("order after move = %v, want [p1 p2 x n1]", got)
	}
}
