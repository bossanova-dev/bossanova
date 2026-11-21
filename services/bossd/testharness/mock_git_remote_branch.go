package testharness

import "context"

type remoteBranchProbeCall struct {
	RepoPath string
	Branch   string
}

// RemoteBranchExists records origin probes and answers from the configured set.
func (m *MockWorktreeManager) RemoteBranchExists(_ context.Context, repoPath, branch string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.RemoteBranchExistsCalls = append(m.RemoteBranchExistsCalls, remoteBranchProbeCall{RepoPath: repoPath, Branch: branch})
	return m.RemoteBranches[branch], m.RemoteBranchExistsErr
}
