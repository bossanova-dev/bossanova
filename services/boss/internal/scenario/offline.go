package scenario

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/recurser/bossalib/contract/cloud/sqlutil"
)

// prepareOffline supplies store inputs without requiring a running daemon. It is
// used only when no live prepare phase has persisted scenario state.
func prepareOffline(doc Document, opts ApplyOptions, st *applyState) error {
	for _, repo := range doc.Repos {
		id, err := sqlutil.NewID()
		if err != nil {
			return err
		}
		path := filepath.Join(opts.Home, "worktrees", id)
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		origin := "scenario/" + id
		st.Spec.Repos = append(st.Spec.Repos, SeedRepo{Key: repo.Key, DisplayName: repo.DisplayName, LocalPath: path, OriginURL: "https://github.com/" + origin + ".git", MergeStrategy: repo.MergeStrategy})
		st.Paths[repo.Key] = path
		st.Origins[repo.Key] = origin
	}
	for i := range st.Spec.Sessions {
		row := &st.Spec.Sessions[i]
		id, err := sqlutil.NewID()
		if err != nil {
			return err
		}
		row.WorktreePath = filepath.Join(opts.Home, "worktrees", id)
		if err := os.MkdirAll(row.WorktreePath, 0o700); err != nil {
			return fmt.Errorf("create offline worktree: %w", err)
		}
		row.BranchName = "scenario/" + id
		st.Branches[row.Key] = row.BranchName
	}
	return nil
}
