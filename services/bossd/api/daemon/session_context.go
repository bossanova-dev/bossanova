package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
)

// --- Context Resolution ---

func (s *Server) ResolveContext(ctx context.Context, req *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error) {
	wd := req.Msg.WorkingDirectory
	if wd == "" {
		var err error
		wd, err = os.Getwd()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get working directory: %w", err))
		}
	}

	// Resolve to absolute path.
	absWD, err := filepath.Abs(wd)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("resolve absolute path: %w", err))
	}

	repo, sess, err := s.workspaceOwner(ctx, absWD)
	if err != nil {
		return nil, err
	}
	resp := &pb.ResolveContextResponse{}
	if repo != nil {
		resp.Repo = repoToProto(repo)
	}
	if sess != nil {
		resp.Session = SessionToProto(sess)
	}
	return connect.NewResponse(resp), nil
}

// workspaceOwner shares the registered repo/worktree boundary used by context and evidence RPCs.
func (s *Server) workspaceOwner(ctx context.Context, path string) (*domain.Repo, *domain.Session, error) {
	repos, err := s.repos.List(ctx)
	if err != nil {
		return nil, nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
	}
	// Prefer the deepest registered boundary so a nested untrusted repo cannot borrow a parent's trust.
	var owner *domain.Repo
	var session *domain.Session
	longest := 0
	for _, repo := range repos {
		repoRoot := canonicalWorkspacePath(repo.LocalPath)
		if repo.LocalPath != "" && workspaceContains(path, repoRoot) && len(repoRoot) > longest {
			owner, session, longest = repo, nil, len(repoRoot)
		}
		sessions, err := s.sessions.List(ctx, repo.ID)
		if err != nil {
			continue
		}
		for _, sess := range sessions {
			if sess == nil {
				continue
			}
			sessionRoot := canonicalWorkspacePath(sess.WorktreePath)
			if sess.WorktreePath != "" && workspaceContains(path, sessionRoot) && len(sessionRoot) >= longest {
				owner, session, longest = repo, sess, len(sessionRoot)
			}
		}
	}
	if owner != nil {
		return owner, session, nil
	}

	return nil, nil, nil
}

func workspaceContains(path, root string) bool {
	return isSubdirOf(canonicalWorkspacePath(path), canonicalWorkspacePath(root))
}

func canonicalWorkspacePath(path string) string {
	// ResolveContext also accepts paths that do not yet exist.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}
