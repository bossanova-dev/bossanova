package db

import (
	"context"
	"fmt"

	"github.com/recurser/bossd/internal/db/sqlcgen"
)

// SetPRAuthorAndTarget stores the PR author's login and the PR's base branch
// for the session while its PR number is still prNumber, so a result read for
// a PR the session has since replaced never lands. An empty value keeps the
// stored one, and an unchanged pair writes nothing. Like SetListRanks it does
// not touch updated_at: the values are observations of GitHub, not edits.
func (s *SQLiteSessionStore) SetPRAuthorAndTarget(ctx context.Context, id string, prNumber int, author, target string) error {
	if _, err := sqlcgen.New(s.db).SetSessionPRAuthorAndTarget(ctx, sqlcgen.SetSessionPRAuthorAndTargetParams{
		ID:             id,
		PrNumber:       int64(prNumber),
		PrAuthorLogin:  author,
		PrTargetBranch: target,
	}); err != nil {
		return fmt.Errorf("set session %s PR author and target: %w", id, err)
	}
	return nil
}
