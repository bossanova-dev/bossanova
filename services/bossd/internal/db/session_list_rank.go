package db

import (
	"context"
	"fmt"
	"sort"

	"github.com/recurser/bossd/internal/db/sqlcgen"
)

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
	queries := sqlcgen.New(tx)
	for _, id := range ids {
		n, err := queries.SetSessionListRank(ctx, sqlcgen.SetSessionListRankParams{ID: id, ListRank: ranks[id]})
		if err != nil {
			return 0, fmt.Errorf("set session list rank %s: %w", id, err)
		}
		updated += int(n)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit session list ranks: %w", err)
	}
	return updated, nil
}
