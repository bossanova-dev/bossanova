package server

import (
	"context"
	"errors"
	"fmt"
	"os"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// --- Session Lifecycle ---

// acquireRepoMerge serializes user-initiated merges per repo (BOS-439). It
// blocks until it holds the repo's capacity-1 gate, then returns a release func
// the caller must defer. Acquisition is context-aware: if ctx is cancelled while
// the gate is held by another merge, it abandons the wait (decrementing its
// ref/reaping the entry) and returns ctx.Err() with a nil release — the caller
// must not merge. An empty repoID (no repo to guard) returns a no-op release and
// nil error. The ref-counted map lifecycle and the select on a buffered channel
// are the same shape as bossalib/keyedgate (and taskorchestrator.autoMergeSem);
// it is deliberately NOT converted to that package, because unlike the session
// locks this wait is unbounded by design, and bounding it would change merge
// behaviour rather than tidy it.
func (s *Server) acquireRepoMerge(ctx context.Context, repoID string) (release func(), err error) {
	if repoID == "" {
		return func() {}, nil
	}

	// Honor an already-cancelled/expired context before contending: a caller
	// that has given up must never acquire the gate. Without this, the select
	// below can pick the (ready) sem-send over a (ready) ctx.Done and proceed.
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mergeMu.Lock()
	if s.repoMergeGates == nil {
		s.repoMergeGates = map[string]*repoMergeGate{}
	}
	gate := s.repoMergeGates[repoID]
	if gate == nil {
		gate = &repoMergeGate{sem: make(chan struct{}, 1)}
		s.repoMergeGates[repoID] = gate
	}
	gate.refs++
	s.mergeMu.Unlock()

	// Decrement the ref (and reap the entry when it hits zero) — used both when
	// acquisition is abandoned and inside the successful-acquisition release.
	unref := func() {
		s.mergeMu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(s.repoMergeGates, repoID)
		}
		s.mergeMu.Unlock()
	}

	select {
	case gate.sem <- struct{}{}:
		// select picks a ready case at random, so the sem-send can win even when
		// ctx is already done (holder released at the same instant the caller's
		// deadline fired). Re-check so an abandoned merge releases the gate and
		// bails instead of proceeding into the live gate + MergePR.
		if err := ctx.Err(); err != nil {
			<-gate.sem
			unref()
			return nil, err
		}
		return func() {
			<-gate.sem
			unref()
		}, nil
	case <-ctx.Done():
		unref()
		return nil, ctx.Err()
	}
}

// activeSessionOwnsBranch reports whether an active session already owns branch
// in repo. The BOS-289 already-shipped scan uses it to skip when the create is
// an attach/resume to a live session (sendExistingSessionForBranch attaches
// below with attached_existing=true) — the ticket's own open tagged PR must not
// pre-empt that attach, mirroring the pr_number skip. A list error is treated as
// "no owner" so a transient store hiccup never suppresses the dedup scan; a truly
// broken store surfaces at sendExistingSessionForBranch regardless.
func (s *Server) activeSessionOwnsBranch(ctx context.Context, repoID, branch string) bool {
	if branch == "" {
		return false
	}
	active, err := s.sessions.ListActive(ctx, repoID)
	if err != nil {
		return false
	}
	for _, sess := range active {
		if sess.BranchName == branch {
			return true
		}
	}
	return false
}

func (s *Server) sendExistingSessionForBranch(ctx context.Context, repoID, branch string, emit createSessionSender) (bool, error) {
	if branch == "" {
		return false, nil
	}
	active, err := s.sessions.ListActive(ctx, repoID)
	if err != nil {
		return false, connect.NewError(connect.CodeInternal, fmt.Errorf("list active sessions: %w", err))
	}
	for _, sess := range active {
		if sess.BranchName != branch {
			continue
		}
		if sess.WorktreePath == "" {
			return true, connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("session already starting for branch %q", branch))
		}
		if _, err := os.Stat(sess.WorktreePath); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("session already exists for branch %q but worktree directory is missing", branch))
			}
			return true, connect.NewError(connect.CodeInternal, fmt.Errorf("stat existing session worktree: %w", err))
		}
		return true, emit.Send(&pb.CreateSessionResponse{
			Event: &pb.CreateSessionResponse_SessionCreated{
				SessionCreated: &pb.SessionCreated{
					Session:          SessionToProto(sess),
					AttachedExisting: true,
				},
			},
		})
	}
	return false, nil
}

func createSessionConnectError(err error) error {
	if session.IsDuplicateActiveSessionError(err) || session.IsAlreadyShippedError(err) {
		return connect.NewError(connect.CodeAlreadyExists, err)
	}
	// No dated API bump: CreateSession is a daemon-local API consumed by boss.
	// The version-skewed hosted ProxyCreateSession relay forwards only diagnostic
	// text in its existing error frame, not this transport code. No version gate
	// is needed for actionable failure text; the frame shape and meaning stay the
	// same. See docs/structure/contracts.md#skipping-a-bump-justify-it-in-the-diff.
	var kept *session.PreparedWorktreeKeptError
	if errors.As(err, &kept) {
		message := kept.Error()
		return connect.NewError(connect.CodeUnavailable, &createSessionStartError{message: message, cause: err})
	}
	var failed *session.AgentStartFailedError
	if errors.As(err, &failed) && failed.Transient() {
		message := fmt.Sprintf("agent start failed under host load after %d attempts; nothing was kept", failed.Attempts)
		return connect.NewError(connect.CodeUnavailable, &createSessionStartError{message: message, cause: err})
	}
	// A create that lost the race to write refs/remotes/origin/<base> is a
	// timing failure, not a bad request: the same call, made again, is expected
	// to succeed. Left in the CodeInternal bucket an unattended caller cannot
	// tell it apart from a missing branch or a full disk and gives up.
	// Unavailable rather than Aborted because Connect/gRPC clients and the MCP
	// layer already read Unavailable as retry-the-same-call, which is exactly
	// the caller behaviour this needs.
	if errors.Is(err, domain.ErrRefLockContended) {
		return connect.NewError(connect.CodeUnavailable, err)
	}
	// A branch git rejects (e.g. a PR head or a title that sanitizes to
	// nothing) is a bad request, not a daemon fault.
	if errors.Is(err, domain.ErrInvalidBranchName) {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	if refusal, ok := injectionRefusalConnectError(err, "start session"); ok {
		return refusal
	}
	return connect.NewError(connect.CodeInternal, err)
}

// createSessionStartError exposes retry guidance without losing the typed cause.
type createSessionStartError struct {
	message string
	cause   error
}

func (e *createSessionStartError) Error() string { return e.message }
func (e *createSessionStartError) Unwrap() error { return e.cause }

// cleanupFailedCreateSession tears down a create that never produced a usable
// session: the artifacts on disk, then the row itself.
//
// The artifact half is session.CleanUpBootstrapArtifacts — the same function the
// task-orchestrator path reaches through Lifecycle.CleanUpFailedBootstrapArtifacts,
// rather than a second copy of the ownership rule (reap the branch only when the
// row carries a worktree path) that would have to be kept in step by hand. What
// remains here is what only the server owns: dropping the row and publishing the
// deletion.
//
// BOS-717 is what makes that rule useful rather than theoretical: gitpkg's
// OnWorktreeReady hook records the worktree (and the branch it settled on) the
// moment `git worktree add` lands, so a bootstrap killed later — in the setup
// script, or by the deadline — still cleans up after itself.
func (s *Server) cleanupFailedCreateSession(ctx context.Context, sessionID string, bootstrapErr error) {
	// A background draft-PR create may still be pushing this session's branch
	// (BOS-540). Stop it before deleting the branch and purging the worktree, or
	// the two collide: the create would push a branch this is about to delete,
	// and open a PR for a session that no longer exists. Almost every caller
	// reaches here before StartSession ever spawned the step, where this is an
	// immediate no-op; it matters on the one path that can race — the client
	// disconnecting mid-create.
	//
	// StopBackgroundDraftPR cancels before joining, which is what keeps this
	// affordable: waiting out a push and a `gh pr create` whose result we do not
	// want would add that latency to every failed create — the exact latency
	// BOS-540 removed. It no longer stalls creates for OTHER targets: since
	// BOS-717 the start-path lock is per-repo and is released as soon as the
	// session row is inserted, so callers reach here holding no start-path lock.
	// The per-target lock IS still held across this whole function, by design —
	// that is what makes an overlapping create for the same target wait for this
	// one's outcome, and it is the hold session.TargetStartLockTimeout's margin
	// is budgeted against.
	if s.lifecycle != nil {
		s.lifecycle.StopBackgroundDraftPR(ctx, sessionID)
	}

	// The stores are passed explicitly rather than going through
	// s.lifecycle.CleanUpFailedBootstrapArtifacts, for two reasons: s.lifecycle
	// is optional here (it is nil in tests and in cut-down wirings, and losing
	// artifact cleanup with it would be a silent regression), and that method
	// also stops the background draft PR, which the line above has already done.
	session.CleanUpBootstrapArtifacts(ctx, s.logger, s.sessions, s.repos, s.worktrees, sessionID, bootstrapErr)

	_ = s.sessions.Delete(ctx, sessionID)
	if s.onSessionDeleted != nil {
		s.onSessionDeleted(ctx, sessionID)
	}
}
