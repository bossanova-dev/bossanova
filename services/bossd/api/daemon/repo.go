package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/config"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/domain"
)

// --- Repo Management ---

func (s *Server) RegisterRepo(ctx context.Context, req *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	msg := req.Msg
	if msg.LocalPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path is required"))
	}

	// Validate the path exists, is a directory, and is a git repo.
	info, err := os.Stat(msg.LocalPath)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path does not exist: %s", msg.LocalPath))
	}
	if !info.IsDir() {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path is not a directory: %s", msg.LocalPath))
	}
	if !s.worktrees.IsGitRepo(ctx, msg.LocalPath) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("not a git repository: %s", msg.LocalPath))
	}

	worktreeBaseDir, err := resolveRepoWorktreeBaseDir(msg.WorktreeBaseDir, config.Load)
	if err != nil {
		return nil, err
	}

	var setupScript *string
	if msg.SetupScript != nil {
		setupScript = msg.SetupScript
	}

	// Auto-detect origin URL from git config.
	originURL, _ := s.worktrees.DetectOriginURL(ctx, msg.LocalPath)
	originURL = domain.CloneURLWithoutCredentials(originURL)

	repo, err := s.repos.Create(ctx, domain.CreateRepoParams{
		DisplayName:       msg.DisplayName,
		LocalPath:         msg.LocalPath,
		OriginURL:         originURL,
		DefaultBaseBranch: msg.DefaultBaseBranch,
		WorktreeBaseDir:   worktreeBaseDir,
		SetupScript:       setupScript,
		IsTrusted:         msg.GetIsTrusted(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err))
	}

	return connect.NewResponse(&pb.RegisterRepoResponse{Repo: repoToProto(repo)}), nil
}

func (s *Server) CloneAndRegisterRepo(ctx context.Context, req *connect.Request[pb.CloneAndRegisterRepoRequest]) (*connect.Response[pb.CloneAndRegisterRepoResponse], error) {
	msg := req.Msg
	if msg.CloneUrl == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("clone_url is required"))
	}
	if msg.LocalPath == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path is required"))
	}
	// Refuse option-shaped, <transport>:: and unrecognised URL forms, and a
	// relative destination, before anything touches disk.
	if err := domain.ValidateCloneURL(msg.CloneUrl); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("clone_url: %w", err))
	}
	if !filepath.IsAbs(msg.LocalPath) {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("local_path must be absolute"))
	}

	// Resolve the worktree base before cloning so a bad value never leaves a
	// cloned directory behind with no registration.
	worktreeBaseDir, err := resolveRepoWorktreeBaseDir(msg.WorktreeBaseDir, config.Load)
	if err != nil {
		return nil, err
	}

	// Check if the target path already exists.
	if info, err := os.Stat(msg.LocalPath); err == nil {
		if !info.IsDir() {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("local_path exists and is not a directory"))
		}
		// Directory exists — check if it's already a git repo with matching origin.
		existingOrigin, _ := s.worktrees.DetectOriginURL(ctx, msg.LocalPath)
		if existingOrigin == "" {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("local_path already exists but is not a git repository"))
		}
		// Origin exists but doesn't match — error. Clone records an http(s)
		// URL's origin without its user:password, so a retry with the same
		// credentialed URL matches the stripped origin; an older clone that
		// kept the credentials in origin still matches the URL as given.
		if existingOrigin != msg.CloneUrl && existingOrigin != domain.CloneURLWithoutCredentials(msg.CloneUrl) {
			return nil, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("local_path is a git repo with different origin %q", domain.RedactURLCredentials(existingOrigin)))
		}
		// Matching origin — skip clone, just register.
	} else {
		// Path doesn't exist — clone.
		if err := s.worktrees.Clone(ctx, msg.CloneUrl, msg.LocalPath); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("clone: %w", err))
		}
	}

	// Auto-detect origin URL from the cloned repo.
	originURL, _ := s.worktrees.DetectOriginURL(ctx, msg.LocalPath)
	originURL = domain.CloneURLWithoutCredentials(originURL)

	var setupScript *string
	if msg.SetupScript != nil {
		setupScript = msg.SetupScript
	}

	repo, err := s.repos.Create(ctx, domain.CreateRepoParams{
		DisplayName:       msg.DisplayName,
		LocalPath:         msg.LocalPath,
		OriginURL:         originURL,
		DefaultBaseBranch: msg.DefaultBaseBranch,
		WorktreeBaseDir:   worktreeBaseDir,
		SetupScript:       setupScript,
		IsTrusted:         msg.GetIsTrusted(),
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("create repo: %w", err))
	}

	return connect.NewResponse(&pb.CloneAndRegisterRepoResponse{Repo: repoToProto(repo)}), nil
}

func (s *Server) ListRepos(ctx context.Context, req *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	repos, err := s.repos.List(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list repos: %w", err))
	}

	pbRepos := make([]*pb.Repo, len(repos))
	for i, r := range repos {
		pbRepos[i] = s.liveRepoToProto(r)
	}

	return connect.NewResponse(&pb.ListReposResponse{Repos: pbRepos}), nil
}

// WebhookEvidence reports repos GitHub webhooks evidently never reach this
// daemon for. upstream.WebhookHealth implements it.
type WebhookEvidence interface {
	WebhooksMissing(repo string) bool
}

// liveRepoToProto is repoToProto plus the daemon's live, never-persisted repo
// state. Every response the repo settings view adopts as its repo goes through
// it, so a save never drops what the list showed.
func (s *Server) liveRepoToProto(r *models.Repo) *pb.Repo {
	p := repoToProto(r)
	p.IsMissingWebhooks = s.webhookEvidence != nil && s.webhookEvidence.WebhooksMissing(r.OriginURL)
	return p
}

func (s *Server) RemoveRepo(ctx context.Context, req *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	if err := s.repos.Delete(ctx, req.Msg.Id); err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %s", req.Msg.Id))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("remove repo: %w", err))
	}

	return connect.NewResponse(&pb.RemoveRepoResponse{}), nil
}

func (s *Server) UpdateRepo(ctx context.Context, req *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	msg := req.Msg
	if msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	params := domain.UpdateRepoParams{}
	if msg.DisplayName != nil {
		params.DisplayName = msg.DisplayName
	}
	if msg.CanAutoMerge != nil {
		params.CanAutoMerge = msg.CanAutoMerge
	}
	if msg.CanAutoMergeDependabot != nil {
		params.CanAutoMergeDependabot = msg.CanAutoMergeDependabot
	}
	if msg.CanAutoRepair != nil {
		params.CanAutoRepair = msg.CanAutoRepair
	}
	if msg.ShouldArchiveSessionsAfterMerge != nil {
		params.ShouldArchiveSessionsAfterMerge = msg.ShouldArchiveSessionsAfterMerge
	}
	if msg.ShouldKeepBranchesCurrent != nil {
		params.ShouldKeepBranchesCurrent = msg.ShouldKeepBranchesCurrent
	}
	if msg.CanAutoDeleteBranches != nil {
		params.CanAutoDeleteBranches = msg.CanAutoDeleteBranches
	}
	if msg.IsTrusted != nil {
		params.IsTrusted = msg.IsTrusted
	}
	if msg.MergeStrategy != nil {
		// Normalize at the storage boundary so an empty/unknown legacy string is
		// persisted as the default 'merge' rather than written verbatim (scanRepo
		// normalizes on read, but keep invalid values out of the column too).
		ms := models.ParseMergeStrategy(*msg.MergeStrategy)
		params.MergeStrategy = &ms
	}
	if msg.SetupScript != nil {
		if *msg.SetupScript == "" {
			// Empty string clears the setup command (set DB to NULL).
			params.SetupScript = new(*string)
		} else {
			params.SetupScript = &msg.SetupScript
		}
	}
	// Secret fields: the tri-state SecretUpdate is authoritative and wins over
	// the legacy plaintext field. UNCHANGED/UNSPECIFIED (or a nil message) leaves
	// the param nil so the stored key is never blanked on a save-without-retype.
	params.LinearAPIKey = secretUpdateToParam(msg.LinearKey, msg.LinearApiKey)
	params.SentryAPIKey = secretUpdateToParam(msg.SentryKey, msg.SentryApiKey)
	if msg.SentryOrg != nil {
		params.SentryOrg = msg.SentryOrg
	}
	if msg.WorktreeBaseDir != nil {
		// Present-but-empty is rejected rather than clearing the column: an
		// empty base is exactly the state that blocks session creation.
		dir, err := validateRepoWorktreeBaseDir(*msg.WorktreeBaseDir)
		if err != nil {
			return nil, err
		}
		params.WorktreeBaseDir = &dir
	}
	if msg.ExpectedUpdatedAt != nil {
		t := msg.ExpectedUpdatedAt.AsTime()
		params.ExpectedUpdatedAt = &t
	}
	repo, err := s.repos.Update(ctx, msg.Id, params)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrStaleRepoUpdate):
			return nil, connect.NewError(connect.CodeAborted, fmt.Errorf("repo changed since last read: %w", err))
		case errors.Is(err, domain.ErrNotFound):
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
		default:
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("update repo: %w", err))
		}
	}

	return connect.NewResponse(&pb.UpdateRepoResponse{Repo: s.liveRepoToProto(repo)}), nil
}

// secretUpdateToParam translates a tri-state SecretUpdate into an
// UpdateRepoParams secret pointer, falling back to the legacy plaintext field.
//   - SET   → &value (write the provided secret; "" when no value is supplied)
//   - CLEAR → pointer to "" (clear the stored key)
//   - UNCHANGED/UNSPECIFIED or a nil message → the legacy field (nil = no write)
//
// The SecretUpdate therefore wins over the legacy field whenever it carries an
// actionable SET/CLEAR; only an absent or UNCHANGED action defers to legacy.
func secretUpdateToParam(su *pb.SecretUpdate, legacy *string) *string {
	if su != nil {
		switch su.GetAction() {
		case pb.SecretAction_SECRET_ACTION_SET:
			v := su.GetValue()
			return &v
		case pb.SecretAction_SECRET_ACTION_CLEAR:
			empty := ""
			return &empty
		case pb.SecretAction_SECRET_ACTION_UNSPECIFIED, pb.SecretAction_SECRET_ACTION_UNCHANGED:
			// Defer to the legacy field below (no actionable secret mutation).
		}
	}
	return legacy
}

// GetRepoSettings returns the web-safe, secret-masked settings for a repo. The
// response carries has_linear_key/has_sentry_key booleans and the non-secret
// sentry_org — never the plaintext key material — plus updated_at as the
// optimistic-concurrency token the client echoes back on the next UpdateRepo.
func (s *Server) GetRepoSettings(ctx context.Context, req *connect.Request[pb.GetRepoSettingsRequest]) (*connect.Response[pb.GetRepoSettingsResponse], error) {
	if req.Msg.Id == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("id is required"))
	}

	repo, err := s.repos.Get(ctx, req.Msg.Id)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
		}
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("get repo: %w", err))
	}

	return connect.NewResponse(&pb.GetRepoSettingsResponse{Settings: repoToRepoSettings(repo)}), nil
}

func (s *Server) ListRepoPRs(ctx context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	if req.Msg.RepoId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}

	// Verify the repo exists and get its origin URL.
	repo, err := s.repos.Get(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	if repo.OriginURL == "" {
		return connect.NewResponse(&pb.ListRepoPRsResponse{}), nil
	}

	prs, err := s.provider.ListOpenPRs(ctx, repo.OriginURL)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list PRs: %w", err))
	}

	keys, err := s.activeSessionKeysForRepo(ctx, req.Msg.RepoId)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}
	prs = keys.excludePRs(prs)

	pbPRs := make([]*pb.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &pb.PRSummary{
			Number:     clampInt32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      pb.PRState(clampInt32(int(pr.State))),
			Author:     pr.Author,
		}
	}

	return connect.NewResponse(&pb.ListRepoPRsResponse{PullRequests: pbPRs}), nil
}
