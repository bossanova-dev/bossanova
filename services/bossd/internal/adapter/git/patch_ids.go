package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"

	"github.com/recurser/bossalib/kit/vcs"
)

// FetchBaseTip fetches origin/<base> and returns the commit it now points at.
func (m *Manager) FetchBaseTip(ctx context.Context, localPath, base string) (string, error) {
	if err := m.FetchBase(ctx, localPath, base); err != nil {
		return "", err
	}
	return runGit(ctx, localPath, "rev-parse", "--verify", "refs/remotes/origin/"+base+"^{commit}")
}

// PatchIDs returns the stable patch ids of each head's own commits: those in
// baseTip...head that the default branch does not already carry under another
// SHA (git's cherry check), merges left out, sorted and without repeats. A
// commit the default branch took by rebase-merge thus drops out of every
// branch still carrying its old copy. Heads missing locally are fetched
// from origin by SHA first, in one fetch, then one at a time if that fails.
// A head that still cannot be read is absent from the result and named in
// the error, which never discards the heads that were read.
func (m *Manager) PatchIDs(ctx context.Context, localPath, baseTip string, heads []string) (map[string][]string, error) {
	if localPath == "" {
		return nil, fmt.Errorf("local path is required")
	}
	tip, ok := vcs.NormalizeHeadSHA(baseTip)
	if !ok {
		return nil, fmt.Errorf("base tip %q is not a full commit SHA", baseTip)
	}
	valid := make([]string, 0, len(heads))
	var errs []error
	for _, head := range heads {
		if sha, ok := vcs.NormalizeHeadSHA(head); ok {
			valid = append(valid, sha)
		} else {
			errs = append(errs, fmt.Errorf("head %q is not a full commit SHA", head))
		}
	}
	m.fetchMissingCommits(ctx, localPath, valid)
	ids := make(map[string][]string, len(valid))
	for _, head := range valid {
		got, err := m.rangePatchIDs(ctx, localPath, tip, head)
		if err != nil {
			errs = append(errs, fmt.Errorf("patch ids for %s: %w", head, err))
			continue
		}
		ids[head] = got
	}
	return ids, errors.Join(errs...)
}

func (m *Manager) fetchMissingCommits(ctx context.Context, localPath string, heads []string) {
	var missing []string
	for _, head := range heads {
		if _, err := runGit(ctx, localPath, "cat-file", "-e", head+"^{commit}"); err != nil {
			missing = append(missing, head)
		}
	}
	if len(missing) == 0 {
		return
	}
	fetch := func(shas ...string) error {
		_, err := fetchWithRefLockRetry(ctx, localPath, runGitRemote, append([]string{"fetch", "--no-tags", "origin"}, shas...)...)
		return err
	}
	if err := fetch(missing...); err == nil || len(missing) == 1 {
		return
	}
	for _, head := range missing {
		if fetch(head) != nil {
			m.logger.Debug().Str("head", head).Msg("fetch PR head for patch ids failed")
		}
	}
}

func (m *Manager) rangePatchIDs(ctx context.Context, localPath, tip, head string) ([]string, error) {
	log, err := m.runGitBytes(ctx, localPath, nil, "log", "-p", "--no-merges", "--no-color", "--no-ext-diff", "--cherry-pick", "--right-only", tip+"..."+head, "--")
	if err != nil {
		return nil, err
	}
	out, err := m.runGitBytes(ctx, localPath, bytes.NewReader(log), "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if id, _, ok := strings.Cut(line, " "); ok && id != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids), nil
}

// runGitBytes is runGitWithTimeout for output that must reach the next
// command byte for byte: it feeds stdin and never trims stdout.
func (m *Manager) runGitBytes(parent context.Context, dir string, stdin io.Reader, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, GitCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitCommandEnv(m.logger)
	cmd.Stdin = stdin
	cmd.WaitDelay = gitWaitDelay
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", redactedGitArgv(args), err, redactURLCredentials(strings.TrimSpace(stderr.String())))
	}
	return stdout.Bytes(), nil
}
