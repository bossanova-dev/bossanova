package testharness

import (
	"context"
	"fmt"
	"strings"
	"sync"

	gitpkg "github.com/recurser/bossd/internal/git"
)

// MockWorktreeManager is a mock WorktreeManager that records calls and
// returns configurable results without touching the filesystem.
type MockWorktreeManager struct {
	mu sync.Mutex

	CreateCalls    []gitpkg.CreateOpts
	ArchiveCalls   []string
	ResurrectCalls []gitpkg.ResurrectOpts
	PushCalls      []pushCall
	EmptyTrashCalls []emptyTrashCall

	// CreateFunc overrides the default Create behavior when set.
	CreateFunc func(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error)

	// PushFunc overrides the default Push behavior when set.
	PushFunc func(ctx context.Context, worktreePath, branch string) error

	// DetectOriginURLResult is returned by DetectOriginURL.
	DetectOriginURLResult string
}

type pushCall struct {
	WorktreePath string
	Branch       string
}

type emptyTrashCall struct {
	RepoPath string
	Branches []string
}

// NewMockWorktreeManager creates a mock worktree manager with sensible defaults.
func NewMockWorktreeManager() *MockWorktreeManager {
	return &MockWorktreeManager{
		DetectOriginURLResult: "https://github.com/test/repo.git",
	}
}

func (m *MockWorktreeManager) Create(ctx context.Context, opts gitpkg.CreateOpts) (*gitpkg.CreateResult, error) {
	m.mu.Lock()
	m.CreateCalls = append(m.CreateCalls, opts)
	m.mu.Unlock()

	if m.CreateFunc != nil {
		return m.CreateFunc(ctx, opts)
	}

	// Default: generate a worktree path and branch name from the title.
	branch := "boss/" + sanitize(opts.Title)
	return &gitpkg.CreateResult{
		WorktreePath: fmt.Sprintf("/tmp/worktrees/%s", branch),
		BranchName:   branch,
	}, nil
}

func (m *MockWorktreeManager) Archive(ctx context.Context, worktreePath string) error {
	m.mu.Lock()
	m.ArchiveCalls = append(m.ArchiveCalls, worktreePath)
	m.mu.Unlock()
	return nil
}

func (m *MockWorktreeManager) Resurrect(ctx context.Context, opts gitpkg.ResurrectOpts) error {
	m.mu.Lock()
	m.ResurrectCalls = append(m.ResurrectCalls, opts)
	m.mu.Unlock()
	return nil
}

func (m *MockWorktreeManager) EmptyTrash(ctx context.Context, repoPath string, branches []string) error {
	m.mu.Lock()
	m.EmptyTrashCalls = append(m.EmptyTrashCalls, emptyTrashCall{RepoPath: repoPath, Branches: branches})
	m.mu.Unlock()
	return nil
}

func (m *MockWorktreeManager) Push(ctx context.Context, worktreePath, branch string) error {
	m.mu.Lock()
	m.PushCalls = append(m.PushCalls, pushCall{WorktreePath: worktreePath, Branch: branch})
	m.mu.Unlock()

	if m.PushFunc != nil {
		return m.PushFunc(ctx, worktreePath, branch)
	}
	return nil
}

func (m *MockWorktreeManager) DetectOriginURL(ctx context.Context, repoPath string) (string, error) {
	return m.DetectOriginURLResult, nil
}

// sanitize converts a title to a simple branch-safe string.
func sanitize(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	return s
}
