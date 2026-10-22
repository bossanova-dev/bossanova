package git

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// remotePushReflogSubject is the reflog subject git writes on a remote-tracking
// ref when a `git push` from this repository moved it. A fetch or pull writes a
// different subject ("fetch: fast-forward", "pull: ...").
const remotePushReflogSubject = "update by push"

// SessionPushedHead reports whether the worktree at worktreePath produced newSHA
// as a push on top of fromSHA (BOS-1452). It is true only when all of these
// hold:
//
//  1. the worktree's HEAD is newSHA;
//
//  2. fromSHA is an ancestor of newSHA (strict descent: a rebase, amend or
//     force-push that rewrote fromSHA away is not a push on top of it);
//
//  3. walking the refs/remotes/origin/<branch> reflog from newest, the first
//     entry is newSHA and every entry before the first one at fromSHA was
//     written by a push. A fetch or pull in between (a foreign push this
//     worktree picked up, or a GitHub "Update branch" merge) breaks the chain,
//     and a reflog that never reaches fromSHA is false.
//
//  4. the worktree's own HEAD reflog shows newSHA was authored here (see
//     headReflogAuthoredLocally), not fast-forwarded or reset onto.
//
// Remote-tracking refs and their reflogs live in the shared common dir, so a
// push from another worktree of the same clone also writes "update by push";
// condition 4 is what ties the chain to this worktree. Every failure — a
// missing worktree, a missing object, an absent reflog — is false or an error,
// never true.
func (m *Manager) SessionPushedHead(ctx context.Context, worktreePath, branch, fromSHA, newSHA string) (bool, error) {
	if worktreePath == "" || branch == "" || fromSHA == "" || newSHA == "" {
		return false, errors.New("session pushed head: worktree path, branch, from and new SHA are required")
	}
	if fromSHA == newSHA {
		return false, nil
	}
	head, err := runGit(ctx, worktreePath, "rev-parse", "HEAD")
	if err != nil {
		return false, fmt.Errorf("session pushed head: resolve worktree HEAD: %w", err)
	}
	if strings.TrimSpace(head) != newSHA {
		return false, nil
	}
	descends, err := m.IsAncestor(ctx, worktreePath, fromSHA, newSHA)
	if err != nil {
		return false, fmt.Errorf("session pushed head: %w", err)
	}
	if !descends {
		return false, nil
	}
	reflog, err := runGit(ctx, worktreePath, "reflog", "show", "--format=%H%x00%gs", "refs/remotes/origin/"+branch)
	if err != nil {
		return false, fmt.Errorf("session pushed head: read origin/%s reflog: %w", branch, err)
	}
	if !reflogIsPushChain(reflog, fromSHA, newSHA) {
		return false, nil
	}
	// The remote-tracking reflog is shared by every worktree of the clone, so a
	// peer worktree's push satisfies the chain too; HEAD's reflog is
	// per-worktree, and it must show this worktree authored newSHA rather than
	// fast-forwarding, merging or resetting onto a ref someone else moved.
	headLog, err := runGit(ctx, worktreePath, "reflog", "show", "--format=%H%x00%gs", "HEAD")
	if err != nil {
		return false, fmt.Errorf("session pushed head: read HEAD reflog: %w", err)
	}
	return headReflogAuthoredLocally(headLog, newSHA), nil
}

// SameTree reports whether commits a and b in the repository at worktreePath
// record the same root tree, i.e. carry identical content. A missing object or
// any other git failure is an error, never true.
func (m *Manager) SameTree(ctx context.Context, worktreePath, a, b string) (bool, error) {
	if worktreePath == "" || a == "" || b == "" {
		return false, errors.New("same tree: worktree path and both commits are required")
	}
	if strings.HasPrefix(a, "-") || strings.HasPrefix(b, "-") {
		return false, fmt.Errorf("same tree: invalid commit %q / %q", a, b)
	}
	out, err := runGit(ctx, worktreePath, "rev-parse", "--verify", "--quiet", a+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("same tree: resolve %s tree: %w", a, err)
	}
	other, err := runGit(ctx, worktreePath, "rev-parse", "--verify", "--quiet", b+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("same tree: resolve %s tree: %w", b, err)
	}
	ta, tb := strings.TrimSpace(out), strings.TrimSpace(other)
	if ta == "" || tb == "" {
		return false, errors.New("same tree: git resolved an empty tree id")
	}
	return ta == tb, nil
}

// headReflogAuthoredLocally reports whether the HEAD reflog entry that first
// moved this worktree to newSHA (the oldest of the newest-first run of entries
// at newSHA) records a commit made here: commit, cherry-pick, revert, or a
// rebase step other than its start. A fast-forward, merge, pull, reset or
// checkout onto newSHA — or no entry at all — is false.
func headReflogAuthoredLocally(reflog, newSHA string) bool {
	arrived := ""
	for _, line := range strings.Split(reflog, "\n") {
		sha, subject, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		if strings.TrimSpace(sha) != newSHA {
			break
		}
		arrived = strings.TrimSpace(subject)
	}
	switch {
	case strings.HasPrefix(arrived, "commit"),
		strings.HasPrefix(arrived, "cherry-pick"),
		strings.HasPrefix(arrived, "revert"):
		return true
	case strings.HasPrefix(arrived, "rebase"):
		return !strings.Contains(arrived, "(start)")
	}
	return false
}

// reflogIsPushChain reports whether a newest-first `%H%x00%gs` reflog starts at
// newSHA and reaches fromSHA through push entries only. The entry AT fromSHA is
// not checked: how the remote-tracking ref first reached the receipted head
// does not matter, only that every later move was a push.
func reflogIsPushChain(reflog, fromSHA, newSHA string) bool {
	first := true
	for _, line := range strings.Split(reflog, "\n") {
		sha, subject, ok := strings.Cut(line, "\x00")
		if !ok {
			continue
		}
		sha, subject = strings.TrimSpace(sha), strings.TrimSpace(subject)
		if first {
			if sha != newSHA {
				return false
			}
			first = false
		} else if sha == fromSHA {
			return true
		}
		if subject != remotePushReflogSubject {
			return false
		}
	}
	return false
}
