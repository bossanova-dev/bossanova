package db

import (
	"context"
	"fmt"
	"sort"

	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/sqlutil"
)

// ListRankGap is the spacing left between adjacent manual list ranks
// (BOS-1230). The rank is a SPARSE ordering key, not a dense index: a fresh
// pin takes `highest rank + ListRankGap`, and slotting a session between two
// existing ranks takes the midpoint of their gap. That is what lets a move
// write exactly one row instead of renumbering every sibling.
//
// 1<<32 is chosen so exhaustion is a handled case rather than a hoped-for one.
// Halving a gap of 2^32 leaves no integer strictly between its endpoints only
// after 32 interleaves BETWEEN THE SAME ADJACENT PAIR, with no intervening
// unpin — unreachable by keyboard use in practice. respace below is the
// backstop for the case that is nonetheless not impossible, and the rank
// column is INTEGER (int64), so the ceiling on fresh pins is 2^31 of them.
//
// SCOPE: the rank is a SINGLE GLOBAL NAMESPACE. sessionListOrderSQL compares
// list_rank across every session; a repo id is a WHERE filter, not a partition
// of the key. MoveSession therefore computes from the rows of the scope the
// caller is looking at, so a move made inside one repo's list — an ordinary
// one-row move as much as a re-space — can change how that repo's ranked rows
// interleave with another repo's in the all-repos list. That is the ordering
// model as designed, not an artefact of re-spacing; a per-repo rank would need
// its own column and its own comparator.
const ListRankGap int64 = 1 << 32

// SessionListRankRow is the minimal view of a session the ordering comparator
// and the move arithmetic need: an identity, the optional manual rank, and the
// two natural-order keys. It exists so the move arithmetic can be unit-tested
// against plain values rather than against whole session rows.
type SessionListRankRow struct {
	ID       string
	ListRank *int64
	// CreatedAt is the ISO-8601 creation timestamp used by the natural order.
	CreatedAt string
}

// SessionListRankRowsFrom projects sessions onto the comparator's view.
func SessionListRankRowsFrom(sessions []*models.Session) []SessionListRankRow {
	rows := make([]SessionListRankRow, 0, len(sessions))
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		rows = append(rows, SessionListRankRow{
			ID:        sess.ID,
			ListRank:  sess.ListRank,
			CreatedAt: sess.CreatedAt.UTC().Format(sqlutil.TimeLayout),
		})
	}
	return rows
}

// SessionListRankLess is the Go expression of sessionListOrderSQL, and the two
// MUST agree: a ranked row sorts before an unranked one; two ranked rows sort
// by rank ascending; the rest fall back to created_at descending and then id
// ascending, which is the pre-existing rule plus the tie-break that makes the
// order total. No two distinct rows ever compare equal in both directions.
func SessionListRankLess(a, b SessionListRankRow) bool {
	if (a.ListRank == nil) != (b.ListRank == nil) {
		// Ranked before unranked.
		return a.ListRank != nil
	}
	if a.ListRank != nil && *a.ListRank != *b.ListRank {
		return *a.ListRank < *b.ListRank
	}
	if a.CreatedAt != b.CreatedAt {
		// Newest first.
		return a.CreatedAt > b.CreatedAt
	}
	return a.ID < b.ID
}

// SortSessionListRankRows orders rows by the rendered session-list rule.
func SortSessionListRankRows(rows []SessionListRankRow) {
	sort.SliceStable(rows, func(i, j int) bool { return SessionListRankLess(rows[i], rows[j]) })
}

// SetListRanks writes sessions.list_rank for the named sessions in a single
// transaction. A nil value CLEARS that row's rank, returning the session to the
// natural block — the only way the ordering model can express "sort below an
// unranked neighbour". Rows not named are left byte-identical, so a move that
// passes one entry provably does not renumber siblings.
//
// It deliberately does NOT touch updated_at. A reorder is not a content change,
// and ListByState / ListByStates — the daemon-startup and stranded-cron
// recovery scans — order by updated_at, so bumping it here would silently
// reshuffle crash recovery. The upstream sync ships a full snapshot rather than
// an updated_at delta, so nothing downstream needs the bump to notice the move.
//
// Returns the number of rows actually updated.
func (s *SQLiteSessionStore) SetListRanks(ctx context.Context, ranks map[string]*int64) (int, error) {
	if len(ranks) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Iterate deterministically so a failure reports the same row every run.
	ids := make([]string, 0, len(ranks))
	for id := range ranks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	updated := 0
	for _, id := range ids {
		rank := ranks[id]
		var res interface {
			RowsAffected() (int64, error)
		}
		var execErr error
		if rank == nil {
			res, execErr = tx.ExecContext(ctx, `UPDATE sessions SET list_rank = NULL WHERE id = ?`, id)
		} else {
			res, execErr = tx.ExecContext(ctx, `UPDATE sessions SET list_rank = ? WHERE id = ?`, *rank, id)
		}
		if execErr != nil {
			return 0, fmt.Errorf("set session list rank %s: %w", id, execErr)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("set session list rank %s rows affected: %w", id, err)
		}
		updated += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit session list ranks: %w", err)
	}
	return updated, nil
}

// ListRankMove is the outcome of the move arithmetic: the rows whose
// list_rank must be written for the requested move to take effect.
type ListRankMove struct {
	// Ranks maps session id to the rank to write; a nil value clears the rank.
	// Empty means the move is a successful NO-OP — the session was already at
	// the boundary of the list, or the requested position is not expressible
	// in the ordering model (see ComputeListRankMove).
	//
	// For every expressible move this holds exactly ONE entry. It holds more
	// only on the re-space path below, which rewrites the ranked block without
	// changing the order any of its rows renders in.
	Ranks map[string]*int64

	// IsRespaced reports that the sparse key ran out of room between two
	// adjacent ranks, so the whole ranked block was re-numbered at full spacing
	// instead of one row being slotted into a gap. Order-preserving, and
	// unreachable without ~32 interleaves between one adjacent pair.
	IsRespaced bool
}

// IsNoop reports a move that writes nothing.
func (m ListRankMove) IsNoop() bool { return len(m.Ranks) == 0 }

// ComputeListRankMove computes the list_rank write that moves rows[index] one
// position in the rendered order. rows MUST already be in rendered order (see
// SortSessionListRankRows). up selects the direction.
//
// The ordering model is "ranked rows form a block above the natural block", and
// R5 of the plan requires a move to write exactly one row. Those two facts
// together decide every case here, including the two that are deliberately
// no-ops rather than errors:
//
//   - Moving UP past an UNRANKED neighbour cannot be a one-position swap,
//     because any rank at all sorts above EVERY unranked row. The session
//     therefore joins the END of the ranked block. From the first or second
//     unranked row that is exactly one position; from deeper in the natural
//     block it rises further, which is the behaviour the reporter asked for
//     ("shuffle this session to the top, leave the rest alone").
//   - Moving DOWN past an unranked neighbour is only expressible when the
//     session is the LAST ranked row: clearing its rank drops it back into the
//     natural block. An already-unranked session has no rank that sorts it
//     below another unranked row, so that move is a successful no-op — the same
//     principle as the boundary no-op, since a held-down key must not start
//     failing.
func ComputeListRankMove(rows []SessionListRankRow, index int, up bool) ListRankMove {
	n := len(rows)
	if index < 0 || index >= n {
		return ListRankMove{}
	}
	// The ranked block is the leading run of rows carrying a rank.
	rankedCount := 0
	for rankedCount < n && rows[rankedCount].ListRank != nil {
		rankedCount++
	}
	moved := rows[index]

	if up {
		if index == 0 {
			return ListRankMove{} // already first
		}
		prev := rowAt(rows, index-1)
		if prev.ListRank == nil {
			// Join the end of the ranked block.
			if rankedCount == 0 {
				return singleRank(moved.ID, ListRankGap)
			}
			last := rowAt(rows, rankedCount-1)
			if last.ListRank == nil {
				return singleRank(moved.ID, ListRankGap)
			}
			if v, ok := addGap(*last.ListRank); ok {
				return singleRank(moved.ID, v)
			}
			return respace(append(rankedIDs(rows, rankedCount), moved.ID))
		}
		// Slot strictly between the neighbour above prev (if ranked) and prev.
		var lo *int64
		// Bounds spelled out on a local so the slice index is provably in
		// range (gosec G602 cannot follow `index >= 2` into the subscript).
		if above := index - 2; above >= 0 && above < len(rows) {
			lo = rowAt(rows, above).ListRank
		}
		if v, ok := between(lo, prev.ListRank); ok {
			return singleRank(moved.ID, v)
		}
		// The gap is exhausted, so re-space the ranked block with the moved row
		// in its NEW position. When the moved row is unranked it sits directly
		// below the ranked block (index == rankedCount) and is not in
		// rankedIDs at all: swapping two ranked slots there would renumber the
		// block without moving it, and the non-empty Ranks map would report
		// is_moved=true for a move that did not happen. Insert it instead, the
		// mirror of the append on the join-the-end path above.
		if index >= rankedCount {
			return respace(insertedAt(rankedIDs(rows, rankedCount), index-1, moved.ID))
		}
		reordered, ok := swapped(rankedIDs(rows, rankedCount), index, index-1)
		if !ok {
			// Unreachable: prev is ranked, so index <= rankedCount and the
			// branch above took the equal case. Degrade to a reported no-op
			// rather than a re-space that claims a move it did not make.
			return ListRankMove{}
		}
		return respace(reordered)
	}

	if index == n-1 {
		return ListRankMove{} // already last
	}
	if moved.ListRank == nil {
		// Unranked, with an unranked successor: not expressible (see above).
		return ListRankMove{}
	}
	next := rowAt(rows, index+1)
	if next.ListRank == nil {
		// The last ranked row stepping down into the natural block. Clearing
		// the rank is the ONLY expressible way down past an unranked
		// neighbour, because every rank sorts above every unranked row, so the
		// FALL IS MULTI-POSITION by design — the exact mirror of the
		// multi-position rise documented above. The row lands at its own
		// created_at position in the natural block, which may be several slots
		// lower, or none at all when it is the newest session in that block.
		// The none-at-all case is reported as a no-op: a write that renumbers
		// a row while the rendered list is byte-identical would tell the
		// caller is_moved=true for a move the user cannot see.
		if !clearingMovesRow(rows, index) {
			return ListRankMove{}
		}
		return ListRankMove{Ranks: map[string]*int64{moved.ID: nil}}
	}
	var hi *int64
	if below := index + 2; below >= 0 && below < len(rows) {
		hi = rowAt(rows, below).ListRank
	}
	if hi == nil && clearedRowRendersOnePositionLower(rows, index) {
		// The target slot is the head of the natural block, so the moved row
		// has two representations that render IDENTICALLY: a rank below its
		// ranked successor, or no rank at all. Prefer no rank. A row carries a
		// rank only where one is needed to express its position, which is what
		// makes "move up, then move down" restore a previously-unranked
		// session's unranked state instead of silently pinning it above every
		// session created after it.
		return ListRankMove{Ranks: map[string]*int64{moved.ID: nil}}
	}
	if v, ok := between(next.ListRank, hi); ok {
		return singleRank(moved.ID, v)
	}
	reordered, ok := swapped(rankedIDs(rows, rankedCount), index, index+1)
	if !ok {
		// Unreachable: moved and next are both ranked, so both indices are
		// inside the ranked block. Degrade to a reported no-op rather than a
		// re-space that claims a move it did not make.
		return ListRankMove{}
	}
	return respace(reordered)
}

// clearedRow is r as it would compare once its rank is cleared.
func clearedRow(r SessionListRankRow) SessionListRankRow {
	r.ListRank = nil
	return r
}

// clearingMovesRow reports whether clearing rows[index]'s rank changes the
// position it renders in. rows[index] must be the LAST ranked row, so the rows
// below it are the natural block already in its own order: clearing moves the
// row exactly when the head of that block sorts above the cleared row.
func clearingMovesRow(rows []SessionListRankRow, index int) bool {
	if index+1 >= len(rows) {
		return false
	}
	return SessionListRankLess(rowAt(rows, index+1), clearedRow(rowAt(rows, index)))
}

// clearedRowRendersOnePositionLower reports whether clearing rows[index]'s rank
// renders it exactly one position lower — at the head of the natural block that
// begins below its ranked successor. rows[index+2], when present, is that head;
// with nothing below, the cleared row renders last, which is one lower.
func clearedRowRendersOnePositionLower(rows []SessionListRankRow, index int) bool {
	if index+2 >= len(rows) {
		return true
	}
	return SessionListRankLess(clearedRow(rowAt(rows, index)), rowAt(rows, index+2))
}

// rowAt returns rows[i], or a zero row when i is out of range. Every call site
// below has already established its index is valid; the helper exists so the
// bound is provable AT THE SUBSCRIPT, which is what gosec's G602 requires. A
// zero row reads as unranked, so an impossible out-of-range read degrades to
// the unranked branch rather than panicking.
func rowAt(rows []SessionListRankRow, i int) SessionListRankRow {
	if i < 0 || i >= len(rows) {
		return SessionListRankRow{}
	}
	return rows[i]
}

func singleRank(id string, rank int64) ListRankMove {
	v := rank
	return ListRankMove{Ranks: map[string]*int64{id: &v}}
}

// rankedIDs returns the ids of the leading ranked block, in order.
func rankedIDs(rows []SessionListRankRow, rankedCount int) []string {
	ids := make([]string, 0, rankedCount)
	for i := 0; i < rankedCount; i++ {
		ids = append(ids, rowAt(rows, i).ID)
	}
	return ids
}

// swapped returns ids with positions i and j exchanged. An out-of-range index
// is a CALLER BUG, and returning the list unchanged would hide it behind a
// re-space that renumbers the block without moving anything, so the second
// return value makes it observable rather than silent.
func swapped(ids []string, i, j int) ([]string, bool) {
	if i < 0 || j < 0 || i >= len(ids) || j >= len(ids) {
		return nil, false
	}
	out := make([]string, len(ids))
	copy(out, ids)
	out[i], out[j] = out[j], out[i]
	return out, true
}

// insertedAt returns ids with id inserted at position at, the rest in order.
func insertedAt(ids []string, at int, id string) []string {
	if at < 0 {
		at = 0
	}
	if at > len(ids) {
		at = len(ids)
	}
	out := make([]string, 0, len(ids)+1)
	out = append(out, ids[:at]...)
	out = append(out, id)
	out = append(out, ids[at:]...)
	return out
}

// respace renumbers the whole ranked block at full spacing, in the given order.
// It preserves the order every row renders in; it exists only so an exhausted
// gap is a handled case instead of a silently-duplicated rank.
func respace(orderedIDs []string) ListRankMove {
	ranks := make(map[string]*int64, len(orderedIDs))
	for i, id := range orderedIDs {
		v := int64(i+1) * ListRankGap
		ranks[id] = &v
	}
	return ListRankMove{Ranks: ranks, IsRespaced: true}
}

// addGap returns max+ListRankGap, or false when that would overflow int64.
func addGap(max int64) (int64, bool) {
	if max > maxInt64-ListRankGap {
		return 0, false
	}
	return max + ListRankGap, true
}

const (
	maxInt64 int64 = 1<<63 - 1
	minInt64 int64 = -1 << 63
)

// between returns a rank strictly between lo and hi, or false when the sparse
// key has no room left there. A nil bound is open: below the lowest rank the
// value is hi-gap, above the highest it is lo+gap.
func between(lo, hi *int64) (int64, bool) {
	switch {
	case lo == nil && hi == nil:
		return ListRankGap, true
	case lo == nil:
		if *hi < minInt64+ListRankGap {
			return 0, false
		}
		return *hi - ListRankGap, true
	case hi == nil:
		return addGap(*lo)
	}
	if *hi-*lo < 2 {
		return 0, false
	}
	// *lo + half the gap: computed as a delta so the sum cannot overflow.
	return *lo + (*hi-*lo)/2, true
}
