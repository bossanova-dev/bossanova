package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/contract/daemon/trackerprompt"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossd/internal/domain"
	"github.com/recurser/bossd/internal/session"
)

// CreateSession is the thin ConnectRPC handler: it delegates to the
// streaming core StreamCreateSession, wiring the connect server stream's
// Send as the emit sink. All validation/behaviour lives in the core so the
// reverse-stream path (bosso → daemon CreateSessionCommand) can reuse it
// without a non-mockable *connect.ServerStream.
func (s *Server) CreateSession(ctx context.Context, req *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	return s.StreamCreateSession(ctx, req.Msg, stream.Send)
}

// StreamCreateSession holds the full CreateSession body. Each result frame is
// delivered through emit (the connect handler passes stream.Send; the reverse-
// stream adapter passes a channel-pushing closure). Behaviour and validation
// are identical to the original handler — this is a pure extract-method.
func (s *Server) StreamCreateSession(ctx context.Context, msg *pb.CreateSessionRequest, emit func(*pb.CreateSessionResponse) error) error {
	requestStarted := time.Now()
	// zeroOutput runs the agent from the repo checkout with no worktree, branch
	// or PR (StartSessionOpts.ZeroOutput). Like quick chat it needs no worktree
	// base dir and cuts no branch, but unlike quick chat it still goes through
	// StartSession and its bootstrap runner.
	zeroOutput := msg.GetIsZeroOutput()
	branchMode := "fresh-branch"
	switch {
	case msg.IsQuickChat:
		branchMode = "quick-chat"
	case zeroOutput:
		branchMode = "zero-output"
	case msg.PrNumber != nil:
		branchMode = "existing-pr"
	}

	if msg.RepoId == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo_id is required"))
	}
	if msg.Title == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("title is required"))
	}
	// branch_name becomes a path component of the worktree directory; refuse a
	// name git would reject before anything is stored or touched on disk.
	if branch := msg.GetBranchName(); branch != "" {
		if err := s.checkBranchName(ctx, branch); err != nil {
			if errors.Is(err, domain.ErrInvalidBranchName) {
				return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("branch_name: %w", err))
			}
			return connect.NewError(connect.CodeInternal, err)
		}
	}
	if zeroOutput {
		// Zero output never checks out a branch, and quick chat is a different
		// shape (no StartSession at all); refuse the combinations rather than
		// silently dropping one side of them.
		switch {
		case msg.PrNumber != nil:
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("is_zero_output cannot be combined with pr_number"))
		case msg.GetBranchName() != "":
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("is_zero_output cannot be combined with branch_name"))
		case msg.IsQuickChat:
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("is_zero_output cannot be combined with is_quick_chat"))
		case !msg.GetIsTmuxUnattended():
			// Finalize hard-deletes a zero-output run (and the system prompt
			// forbids commits) only for an unattended session; an attended one
			// would be classified against the user's checkout and could open a
			// PR from it. Bare detach depends on host tmux availability, so
			// require an explicit tmux-unattended launch before accepting.
			return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("is_zero_output requires is_tmux_unattended"))
		}
	}
	// needsWorktree is false for the two shapes that run from the repo checkout.
	needsWorktree := !msg.IsQuickChat && !zeroOutput

	// Verify repo exists.
	repo, err := s.repos.Get(ctx, msg.RepoId)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("repo not found: %w", err))
	}

	if needsWorktree && strings.TrimSpace(repo.WorktreeBaseDir) == "" {
		return connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("repo %s (%s) has no worktree base directory configured; set one with 'boss repo update %s --worktree-base-dir <dir>' (or the MCP update_repo tool's worktree_base_dir argument) before creating a session",
				repo.ID, repo.DisplayName, repo.ID))
	}

	baseBranch := msg.BaseBranch
	if baseBranch == "" {
		baseBranch = repo.DefaultBaseBranch
	}

	var prNumber *int
	var prURL *string
	var headBranch string
	if msg.PrNumber != nil {
		n := int(*msg.PrNumber)
		prNumber = &n

		// Fetch PR metadata to get head branch and construct PR URL.
		if repo.OriginURL != "" {
			prStatus, prErr := s.provider.GetPRStatus(ctx, repo.OriginURL, n)
			if prErr == nil {
				headBranch = prStatus.HeadBranch
				baseBranch = prStatus.BaseBranch
			}
			// Construct PR URL from origin.
			u := constructPRURL(repo.OriginURL, n)
			if u != "" {
				prURL = &u
			}
		}
	} else if msg.BranchName != nil && *msg.BranchName != "" {
		// Use the branch name from the request (e.g., from Linear's suggested branch name).
		headBranch = *msg.BranchName
	}
	if needsWorktree && strings.TrimSpace(baseBranch) == "" {
		return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("repo %s has no default base branch; update repo settings before creating a session", repo.ID))
	}

	// BOS-289: guard against a ticket whose PR already merged (or is in flight on
	// a sibling branch with no live session row). Scan GitHub for a PR carrying
	// the tracker's "[<id>]" title tag in an open or merged state and short-
	// circuit with CodeAlreadyExists. force / quick_chat / no-tracker skip the
	// scan entirely. Two attach/resume cases also skip it, so the ticket's own
	// open tagged PR never turns a documented attach into an AlreadyExists
	// refusal: an explicit PrNumber (the Linear-ticket-with-open-PR TUI flow
	// co-sets TrackerId and PrNumber), and a headBranch already owned by a live
	// session (the branch_name attach that sendExistingSessionForBranch resolves
	// below with attached_existing=true). A bare tracker_id create with a fresh
	// branch has no such local owner and still scans — that is the sibling-branch
	// duplicate-work case the guard exists to catch. Run before AcquireStartPath
	// so a network round-trip never holds the start-path lock; the
	// in-flight local dedup (BOS-236) still runs atomically under that lock
	// below. Fail open on scan error so a transient GitHub outage cannot wedge
	// the daemon's whole create path.
	if !msg.Force && !msg.IsQuickChat && msg.PrNumber == nil && msg.TrackerId != nil && *msg.TrackerId != "" &&
		repo.OriginURL != "" && !s.activeSessionOwnsBranch(ctx, msg.RepoId, headBranch) {
		prs, scanErr := s.provider.SearchPRsByTitleTag(ctx, repo.OriginURL, *msg.TrackerId)
		if scanErr != nil {
			s.logger.Warn().Err(scanErr).
				Str("tracker_id", *msg.TrackerId).
				Str("repo", repo.ID).
				Msg("already-shipped scan failed; proceeding with session creation (fail-open)")
		} else if shipped, ok := session.AlreadyShippedTaggedPR(prs, *msg.TrackerId); ok {
			return createSessionConnectError(shipped)
		}
	}

	// Two locks, in the order session/start_lock.go documents: the TARGET lock
	// (per repo+branch/PR) is held for the whole call, including StartSession and
	// its failure cleanup, so an overlapping create for the same target waits for
	// this one's outcome; the START-PATH lock (per repo) is released the moment
	// the row is inserted, so a wedged bootstrap can never block a create for
	// another target (BOS-717).
	var unlockTarget func()
	targetLocked := false
	var unlockStart func()
	var startLockWait time.Duration
	startLocked := false
	// Registered before acquisition (the flags gate them) so an acquire failure
	// on the second lock still returns without leaking the first.
	defer func() {
		if targetLocked {
			unlockTarget()
		}
	}()
	defer func() {
		if startLocked {
			unlockStart()
		}
	}()
	if !msg.IsQuickChat {
		// Log BEFORE acquiring anything (BOS-717). start_lock_wait is measured
		// only once a lock has been taken, so a wait that never ends produced no
		// log line at all — which is exactly why the 2026-08-06 wedge left zero
		// diagnostic signal: a create that hung here was invisible, with no
		// session row and nothing in the daemon log. This line is the trace that
		// says which repo and branch a stuck create was waiting on.
		s.logger.Info().
			Str("repo", msg.RepoId).
			Str("branch", headBranch).
			Msg("acquiring session start locks")

		var lockErr error
		var targetLockWait time.Duration
		unlockTarget, targetLockWait, lockErr = session.AcquireTargetStart(ctx, msg.RepoId, headBranch, prNumber)
		if lockErr != nil {
			s.logger.Error().Err(lockErr).
				Str("repo", msg.RepoId).
				Str("branch", headBranch).
				Dur("target_lock_wait", targetLockWait).
				Msg("session start lock contention: giving up waiting for the target lock")
			return connect.NewError(connect.CodeUnavailable, lockErr)
		}
		targetLocked = true

		unlockStart, startLockWait, lockErr = session.AcquireStartPath(ctx, msg.RepoId)
		if lockErr != nil {
			s.logger.Error().Err(lockErr).
				Str("repo", msg.RepoId).
				Str("branch", headBranch).
				Dur("start_lock_wait", startLockWait).
				Msg("session start lock contention: giving up waiting for the start-path lock")
			return connect.NewError(connect.CodeUnavailable, lockErr)
		}
		startLocked = true

		// A slow-but-successful acquire is the early warning for the failure this
		// ticket is about; the success line at the end of the RPC records the wait
		// either way, but only at Info and only once the create has finished.
		if targetLockWait > session.SlowStartLockWaitThreshold || startLockWait > session.SlowStartLockWaitThreshold {
			s.logger.Warn().
				Str("repo", msg.RepoId).
				Str("branch", headBranch).
				Dur("target_lock_wait", targetLockWait).
				Dur("start_lock_wait", startLockWait).
				Msg("session start locks were contended")
		}
	}

	if !msg.IsQuickChat {
		if handled, err := s.sendExistingSessionForBranch(ctx, msg.RepoId, headBranch, emitSender{emit}); handled || err != nil {
			return err
		}

		if prNumber != nil && isDependabotBranch(headBranch) {
			if err := session.EnsureNoActivePRSession(ctx, s.sessions, msg.RepoId, prNumber); err != nil {
				return createSessionConnectError(err)
			}
		}

		// BOS-236: dedup against an existing active session for the same tracker issue
		// (or its PR / branch) so N dispatches for one ticket can't spawn N sessions.
		// This runs under the per-repo start-path lock acquired above, so the
		// check-then-Create below is atomic w.r.t. concurrent daemon-local creates — the
		// race that triplicated BOS-229. A DB unique index was deliberately NOT used: it
		// cannot coexist with `force`, which must be able to create a second session for
		// the same tracker. `force` skips the guard entirely.
		//
		// Neither this guard nor BOS-720's async bootstrap is expressed as an
		// apiversion (lib/bossalib/apiversion) dated bump, but NOT for the reason
		// this comment used to give.
		//
		// The old wording ("response-side, unary-only … applied only in the
		// interceptor's WrapUnary path") was overtaken by 1d63becc5, which added
		// transformingStreamingHandlerConn: apiversion.Interceptor DOES wrap
		// streaming handlers and applies TransformResponse to every frame a stream
		// sends (lib/bossalib/apiversion/interceptor.go). ProxyCreateSession is not
		// skipped.
		//
		// Two reasons survive, and they are the real ones:
		//
		//  1. This RPC lives in the bossd DaemonService, whose handler installs
		//     only socketauth and errortrack (see Listen). apiversion.Interceptor
		//     is installed ONLY on the bosso OrchestratorService handler
		//     (services/bosso/cmd/main.go), as transform.go's own comment states.
		//     A daemon-resident RPC cannot be version-gated at all.
		//  2. On the bosso leg that IS intercepted, a VersionChange can only mutate
		//     a message IN PLACE — Send() calls changes.Apply and then forwards
		//     unconditionally, with no way to say "drop this frame". So neither a
		//     refused create nor a NEW response event could be down-converted, and
		//     a change in frame ORDERING is outside the mechanism entirely. BOS-720
		//     therefore adds no new event type and no new field.
		//
		// `force` is a wire-additive field, so no deployed client breaks at the
		// protocol level; the only behavior removed — dispatch-N-get-N-sessions —
		// was the BOS-229 defect.
		if !msg.Force {
			keys, err := s.activeSessionKeysForRepo(ctx, msg.RepoId)
			if err != nil {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("dedup active sessions: %w", err))
			}
			if existingID, existingPlan, kind, ok := keys.duplicateSessionID(msg.TrackerId, prNumber, headBranch); ok {
				return connect.NewError(connect.CodeAlreadyExists, duplicateSessionAlreadyExistsError(existingID, existingPlan, kind, msg.RepoId))
			}
		}
	}

	// When the client supplies a selected tracker issue but no explicit plan
	// (web new-session Linear/Sentry flow), format the plan server-side from the
	// shared formatter — single source of truth with the TUI (trackerprompt).
	plan := msg.GetPlan()
	if plan == "" && msg.GetTrackerIssue() != nil {
		plan = trackerprompt.Format(msg.GetTrackerIssue(), msg.GetTrackerSource())
	}

	// Resolve the agent name. The proto field is a oneof (*string), so an
	// unset request resolves via the shared rule (single-loaded runner, then
	// Settings.DefaultAgent) — see resolveAgentName.
	agentName := s.resolveAgentName(msg.GetAgentName())

	createParams := domain.CreateSessionParams{
		RepoID:     msg.RepoId,
		Title:      msg.Title,
		Plan:       plan,
		BranchName: headBranch,
		BaseBranch: baseBranch,
		AgentName:  agentName,
		// Opaque agent model id (e.g. an Opus id); "" → plugin default. The
		// headless run path passes session.Model straight to StartByAgent, so
		// this is what makes `create_session {model:…}` / `boss new --model`
		// run `claude … --model <id>`.
		Model:           msg.GetModel(),
		EffectiveModel:  effectiveAgentSetting(agentName, "model", msg.GetModel()),
		EffectiveEffort: effectiveAgentSetting(agentName, "effort", msg.GetEffort()),
		PRNumber:        prNumber,
		TrackerID:       msg.TrackerId,
		TrackerURL:      msg.TrackerUrl,
	}
	if prURL != nil {
		createParams.PRURL = prURL
	}

	// Account binding: honor an explicit account_id (validated against the
	// registry and the resolved agent's provider), an explicit empty account_id
	// (account 0, opting out of the policy), or — when the field is absent — apply
	// the default-account policy. Pass the raw optional pointer so presence is
	// preserved: msg.GetAccountId() would collapse absent and explicit-empty. An
	// explicit empty result leaves AccountID present-empty (account 0) so later
	// chat backfill can distinguish explicit opt-out from legacy nil rows. A
	// resolver error never fails creation — only an invalid explicit account_id
	// does.
	accountID, err := s.resolveSessionAccount(ctx, msg.AccountId, agentName, msg.GetModel())
	if err != nil {
		return err
	}
	if accountID != "" || msg.AccountId != nil {
		createParams.AccountID = &accountID
	}

	sess, err := s.sessions.Create(ctx, createParams)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("create session: %w", err))
	}
	// The start-path lock exists to make the duplicate check above and this
	// insert atomic against concurrent daemon-local creates (BOS-236). That
	// invariant is satisfied the moment the row lands, so release it here —
	// BEFORE StartSession's multi-minute bootstrap (BOS-717). Holding it across
	// StartSession is what let one hung bootstrap block every subsequent
	// CreateSession in the daemon. The target lock stays held.
	if startLocked {
		unlockStart()
		startLocked = false
	}

	// Start the session lifecycle: create worktree, start Claude, fire state machine.
	// Quick chat sessions skip worktree/branch/PR creation entirely.
	var startErr error
	// bootstrapOwnsCleanup records that the failure cleanup has moved to the
	// BootstrapRunner's OnFailure hook, so the shared failure exit below must
	// NOT run it a second time. A second pass would be worse than redundant: the
	// runner releases the target lock once its hooks have run, so by the time
	// this RPC observes the error an overlapping create for the same target may
	// already hold the lock and have recreated the branch — which the stale
	// cleanup would then reap out from under it.
	bootstrapOwnsCleanup := false
	if msg.IsQuickChat {
		// Persist the planning-only signal (BOS-322). Quick chats have no
		// worktree/branch/PR and skip finalize by design; the flag is the
		// defensive backstop so any path that does reach FinalizeSession treats
		// the expected no-change result as benign rather than a failed
		// implementation run. Mirrors how tmux_unattended is set post-create.
		quickChat := true
		if _, updErr := s.sessions.Update(ctx, sess.ID, domain.UpdateSessionParams{IsQuickChat: &quickChat}); updErr != nil {
			// Match the other post-Create early returns in this function: a
			// persisted-but-never-started session row must be cleaned up rather
			// than left orphaned in CreatingWorktree.
			s.cleanupFailedCreateSession(context.WithoutCancel(ctx), sess.ID, nil)
			return connect.NewError(connect.CodeInternal, fmt.Errorf("persist quick_chat flag: %w", updErr))
		}
		startErr = s.lifecycle.StartQuickChatSession(ctx, sess.ID)
	} else {
		// Build the start options. A tmux_unattended session (e.g. /boss-epic) and
		// a `boss new --detach` run both go through the durable tmux-hosted path
		// (detach only when tmux is available) and carry a freshly-minted HookToken
		// so their finalize Stop hook installs (and re-arms across a daemon
		// restart). Mint it up front so a crypto/rand failure surfaces as a clean
		// connect error rather than being swallowed inside the goroutine. On the
		// detach fallback path (tmux unavailable) the token is simply unused —
		// StartSession gates hook install on the tmux-hosted branch — which is
		// harmless.
		startOpts := session.StartSessionOpts{
			ForceBranch:      msg.ForceBranch,
			Detach:           msg.GetDetach(),
			IsTmuxUnattended: msg.GetIsTmuxUnattended(),
			// DeferPR suppresses the up-front draft PR; StartSession gates
			// createDraftPR on !opts.DeferPR, and the finalize EnsurePR hook
			// opens a PR later only if the run produces commits.
			DeferPR: msg.GetDeferPr(),
			// An absent should_run_setup_command (a caller that predates the
			// field) keeps running the repo's setup script; only an explicit
			// false skips it.
			SkipSetupScript: msg.ShouldRunSetupCommand != nil && !msg.GetShouldRunSetupCommand(),
			// ZeroOutput runs from the repo checkout; StartSession persists that
			// checkout as the worktree path, so the settled frame below still
			// carries a non-empty worktree_path.
			ZeroOutput: zeroOutput,
			// SetupOutput is deliberately unset: BootstrapRunner.Start
			// overwrites it with the bootstrap's own non-blocking bus. Setting
			// it here would reintroduce a writer this caller could stall.
		}
		if msg.PrNumber != nil {
			startOpts.ExistingBranch = headBranch
		} else {
			startOpts.BranchName = headBranch
		}
		if msg.GetIsTmuxUnattended() || msg.GetDetach() {
			token, tokenErr := newHookToken()
			if tokenErr != nil {
				s.cleanupFailedCreateSession(context.WithoutCancel(ctx), sess.ID, nil)
				return createSessionConnectError(fmt.Errorf("mint hook token: %w", tokenErr))
			}
			startOpts.HookToken = token
		}

		// Hand the bootstrap to the daemon-owned runner, and hand it the target
		// lock with it. From here this RPC is a SUBSCRIBER: it can return, or be
		// cancelled, without touching the bootstrap.
		//
		// Ownership of both the target lock and the failure cleanup moves with
		// the bootstrap. Releasing the lock here instead would let an
		// overlapping create for the same target start before this one had
		// cleaned up its half-started row — the refusal BOS-717's target lock
		// exists to prevent.
		releaseTarget := unlockTarget
		targetLocked = false        // the runner owns it now; the deferred release must not also fire
		bootstrapOwnsCleanup = true // ...and so does the failure cleanup
		bootstrap := s.bootstrapRunner.Start(sess.ID, startOpts, session.BootstrapHooks{
			ReleaseTarget: releaseTarget,
			OnFailure: func(cleanupCtx context.Context, sessionID string, bootstrapErr error) {
				s.cleanupFailedCreateSession(cleanupCtx, sessionID, bootstrapErr)
			},
		})

		// Subscribe FIRST, before the accepted emit below. The setup bus only
		// replays setupBusReplayLines to a late subscriber, and that constant is
		// sized against "StreamCreateSession subscribes immediately after Start
		// returns, so the window is microseconds". emit is a network send on the
		// create stream, so emitting first would stretch that window to a round
		// of client-side flow control and let a chatty setup script overrun the
		// ring — silently eating the first lines of output, the exact regression
		// the replay buffer exists to prevent. Subscribe only registers a
		// channel; it sends the client nothing, so it cannot reorder what the
		// client sees.
		lines, cancelSub := bootstrap.Subscribe()
		defer cancelSub()

		// The accepted frame (BOS-720). The client now holds a usable session
		// id in milliseconds; the bootstrap is a background worker whose
		// progress is `state` + `updated_at`.
		//
		// Deliberately the EXISTING SessionCreated event rather than a new
		// oneof member. Two code facts force that:
		//   - services/boss/api/tui/newsession/newsession_cmds.go's default arm
		//     used to turn an unrecognised event into a stream error, so a new
		//     member would hard-break an older `boss` binary against a newer
		//     bossd;
		//   - on the bosso leg a new frame type could not be down-converted
		//     away — apiversion's streaming transform mutates a message in
		//     place and has no way to drop it.
		// Re-emitting SessionCreated degrades instead of erroring: an old client
		// that treats the first as terminal closes early. For the drain-to-EOF
		// consumers (`boss new`, the MCP socket backend, the hosted MCP
		// gateway) that costs only the setup output. A pre-BOS-720 *web* bundle
		// against a new bosso is the one client that also ACTS on it — its
		// machine went straight to `done` on the first `created` and its launch
		// effect calls RecordChat — but that fails closed rather than doing
		// harm: the accepted row's worktree_path is still empty, and
		// spawnChatTmux stats it and returns ErrWorktreeMissing.
		//
		// `boss new` (services/boss/cmd/handlers.go) and MCP create_session
		// (services/mcp/internal/adapter/socketbackend) already drain to EOF with
		// last-value-wins, so both stay correct against this frame WITHOUT
		// modification — including the agent_launched signal MCP derives from
		// Session.agent_session_id, which only the settled frame populates.
		//
		// The client still sees the session id before the first line of setup
		// output: relaySetupOutput runs after this emit, so the ordering comes
		// from the drain, not from where Subscribe sits.
		if err := emit(&pb.CreateSessionResponse{
			Event: &pb.CreateSessionResponse_SessionCreated{
				SessionCreated: &pb.SessionCreated{Session: SessionToProto(sess)},
			},
		}); err != nil {
			s.logger.Info().Str("session", sess.ID).
				Msg("create-session client went away before the accepted frame; bootstrap continues")
			return err
		}

		if err := relaySetupOutput(ctx, lines, bootstrap.Done(), func(line string) error {
			return emit(&pb.CreateSessionResponse{Event: &pb.CreateSessionResponse_SetupOutput{SetupOutput: &pb.SetupScriptOutput{Text: line}}})
		}); err != nil {
			// The client went away (or its stream errored). The bootstrap keeps
			// running on the daemon's context, and the runner owns its outcome
			// — cleanup on failure and the target-lock release included.
			//
			// This is the behavior change BOS-720 exists for: before it, this
			// exit deleted the session row and reaped the branch of a bootstrap
			// that was still minutes from finishing.
			s.logger.Info().Str("session", sess.ID).
				Msg("create-session client went away; bootstrap continues in the background")
			return err
		}
		// Err is only valid after Done, and relay returns nil on the BUS
		// closing — which the runner does one defer BEFORE it closes done. So
		// wait for settlement rather than relying on that ordering: it costs
		// microseconds here, and without it any future reshuffle of the
		// runner's defers (flushing setup output before the failure hook, say)
		// would turn this into a read of a stale nil and report a session
		// CREATED that in fact failed to start.
		<-bootstrap.Done()
		startErr = bootstrap.Err()
	}
	if err := startErr; err != nil {
		if !bootstrapOwnsCleanup {
			s.cleanupFailedCreateSession(context.WithoutCancel(ctx), sess.ID, nil)
		}
		if errors.Is(err, domain.ErrBranchExists) {
			return connect.NewError(connect.CodeAlreadyExists, fmt.Errorf("branch already exists for this session title"))
		}
		s.logger.Error().Err(err).
			Str("session", sess.ID).
			Str("title", msg.Title).
			Msg("start session failed")
		return createSessionConnectError(fmt.Errorf("start session: %w", err))
	}

	// Re-fetch the session to get updated fields from lifecycle.
	sess, err = s.sessions.Get(ctx, sess.ID)
	if err != nil {
		return connect.NewError(connect.CodeInternal, fmt.Errorf("get session: %w", err))
	}

	resp := &pb.CreateSessionResponse{
		Event: &pb.CreateSessionResponse_SessionCreated{
			SessionCreated: &pb.SessionCreated{
				Session: SessionToProto(sess),
			},
		},
	}
	if err := emit(resp); err != nil {
		return err
	}
	s.logger.Info().
		Str("session", sess.ID).
		Bool("quick_chat", msg.IsQuickChat).
		Str("branch_mode", branchMode).
		Dur("startup_duration", time.Since(requestStarted)).
		Dur("start_lock_wait", startLockWait).
		Msg("session startup complete")
	return nil
}

type createSessionSender interface {
	Send(*pb.CreateSessionResponse) error
}

// emitSender adapts an emit func(*pb.CreateSessionResponse) error to the
// createSessionSender interface so the extracted streaming core can pass its
// sink to helpers (sendExistingSessionForBranch, streamSetupOutput) that were
// written against the Send-method shape.
type emitSender struct {
	emit func(*pb.CreateSessionResponse) error
}

func (e emitSender) Send(resp *pb.CreateSessionResponse) error { return e.emit(resp) }

// relaySetupOutput forwards setup-output lines to the client until the
// bootstrap settles or the client goes away.
//
// It replaces streamSetupOutput, which read an io.Pipe. The difference is not
// cosmetic: that pipe read was the only thing letting the bootstrap's
// pw.Write() return, so a stalled client blocked the bootstrap. Here the
// bootstrap has already moved on and this function only decides what THIS
// client sees.
//
// A line longer than maxSetupOutputLineBytes is truncated rather than failing
// the stream: the bootstrap is no longer this stream's to fail.
//
// ctx releases the handler when the caller goes away without the transport
// surfacing a send error — the reverse-stream command context, chiefly. Without
// it a silent setup script could pin this goroutine and its stream slot for the
// bootstrap's full deadline. Returning early is safe precisely because the
// bootstrap is no longer coupled to this stream.
func relaySetupOutput(ctx context.Context, lines <-chan string, done <-chan struct{}, sendLine func(string) error) error {
	send := func(line string) error {
		if len(line) > maxSetupOutputLineBytes {
			line = line[:maxSetupOutputLineBytes]
		}
		return sendLine(line)
	}
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				// Bus closed — the bootstrap has settled and flushed its tail.
				return nil
			}
			if err := send(line); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			// Settled. Drain what the bus buffered before it closed rather than
			// returning here: the runner closes the bus and THEN done, microseconds
			// apart, so both cases are ready at once and select picks at random —
			// returning on done would drop the tail of the setup output at random.
			for line := range lines {
				if err := send(line); err != nil {
					return err
				}
			}
			return nil
		}
	}
}
