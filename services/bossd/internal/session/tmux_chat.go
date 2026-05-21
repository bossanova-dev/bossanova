package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

const (
	defaultAgentCommandPrefix      = "/"
	repairChatReclaimIdleThreshold = 30 * time.Minute
)

var (
	ErrRepairChatNotReclaimable  = errors.New("repair chat not reclaimable")
	ErrRepairChatSessionMismatch = errors.New("repair chat belongs to a different session")
	ErrRepairChatActive          = errors.New("repair chat active or not stale")
)

type ReclaimRepairChatResult struct {
	Reclaimed       bool
	TmuxSessionName string
}

const repairChatTitlePrefix = "Repair:"

// IsRepairChatTitle reports whether a chat row belongs to the unattended
// repair workflow. Repair chats have their own stale-reclaim path and should
// not be counted as user activity for repair idle gating.
func IsRepairChatTitle(title string) bool {
	return strings.HasPrefix(title, repairChatTitlePrefix)
}

// RepairChatStaleForReclaim reports whether a repair chat has durable idle
// evidence old enough for the repair plugin to reclaim it.
func RepairChatStaleForReclaim(agentLogsDir string, chat *models.AgentChat, now time.Time) (bool, string, error) {
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
		return true, "repair chat has no live tmux pointer", nil
	}
	if agentLogsDir == "" {
		return false, "agent logs dir not configured; refusing to kill live repair tmux without durable idle evidence", nil
	}

	logPath := agentLogPathFor(agentLogsDir, chat.AgentSessionID)
	st, err := os.Stat(logPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, fmt.Sprintf("agent log for %s is missing; refusing to kill live repair tmux without durable idle evidence", chat.AgentSessionID), nil
		}
		return false, "", fmt.Errorf("stat repair agent log for %s: %w", chat.AgentSessionID, err)
	}

	idleFor := now.Sub(st.ModTime())
	if idleFor < repairChatReclaimIdleThreshold {
		return false, fmt.Sprintf("repair chat last produced output %s ago; threshold is %s", idleFor.Round(time.Second), repairChatReclaimIdleThreshold), nil
	}
	return true, fmt.Sprintf("repair chat log idle for %s; threshold is %s", idleFor.Round(time.Second), repairChatReclaimIdleThreshold), nil
}

// ChatInput is the pane input for a tmux-hosted agent chat. Prompt is raw text;
// Command is a boss command name without an agent-specific prefix.
type ChatInput struct {
	Prompt  string
	Command string
}

func (i ChatInput) render(commandPrefix string) string {
	if i.Command == "" {
		return i.Prompt
	}
	if commandPrefix == "" {
		commandPrefix = defaultAgentCommandPrefix
	}
	return commandPrefix + strings.TrimLeft(i.Command, "/$")
}

// StartTmuxChat boots a Claude (or other agent) run inside a detached tmux
// session and registers it as an agent_chats row so the chat list view can
// surface it. It is the generalized form of the cron-only helper that
// formerly lived under startCronTmuxChat: any caller that needs a tmux-
// hosted chat (cron fire, repair sweep, future interactive UI button)
// should funnel through here so the lifecycle and DB invariants stay in
// one place.
//
// Callers must serialize concurrent invocations for the same sessionID;
// this method is not safe to call from multiple goroutines on the same
// session. The host RPC layer (Task 2's HostServiceServer.StartChatRun)
// and the cron wrapper both satisfy this by holding a per-session lock
// or being single-flight per session.
//
// Steps, all of which must succeed for the call to return cleanly:
//
//  1. Verify tmux is wired up and available on the host AND that the
//     agent runner client for sess.AgentName is loaded. Either failure
//     yields a typed FailedPrecondition error so the caller (e.g. a host
//     RPC) can surface it as gRPC FailedPrecondition without rewrapping.
//  2. Idempotency: if an agent_chats row already exists for this session
//     whose tmux_session_name is alive in tmux, return AlreadyExists with
//     the existing agent_session_id returned in the success-shaped string
//     slot alongside the typed error (caller checks
//     status.Code(err) == codes.AlreadyExists and reads the returned
//     string). Stale rows (tmux exited under us) have their
//     tmux_session_name cleared — the row itself is preserved as a
//     historical chat-list entry — and the call proceeds with a fresh
//     launch.
//  3. Mint a fresh agent_session_id (UUID) and resolve the per-chat tmux
//     session name via tmux.ChatSessionName.
//  4. Resolve argv via the agent plugin's BuildInteractiveCommand. Empty
//     argv is a hard error before any tmux process is spawned.
//  5. Spawn a detached tmux session running that argv in the worktree.
//  6. Persist an agent_chats row using the supplied title.
//  7. Stamp the resolved tmux session name on the row so the chat list
//     surfaces a live "attach" target.
//  8. When hookOpts.Token is non-empty, call ConfigureFinalizeHook with
//     the agent_session_id, the supplied token, and l.hookPort so the
//     claude plugin writes a run-keyed Stop-hook entry in
//     settings.local.json. The hook server then routes Stop POSTs for
//     this run to /hooks/agent-run-complete/<agent_session_id>, which
//     unblocks the matching WaitChatRun call. Skipped when Token is "".
//  9. Inject the supplied input into the tmux session unless the plugin
//     embedded it in argv.
//
// Failures after step 5 (tmux is live) tear tmux down and delete the
// agent_chats row before returning, so a retried sweep doesn't leak.
//
// HookOpts carries the optional run-scoped Stop-hook configuration that
// callers (eg. Task 4's HostServiceServer.StartChatRun) plumb through so
// that the daemon receives a Stop-hook callback for the just-spawned
// agent run. The cron path leaves this zero-valued — the cron hook is
// session-keyed and is wired earlier in StartSession.
//
// Token is the per-run bearer secret stamped into a SIBLING entry in
// settings.local.json (alongside any session-keyed cron entry); when
// Token is empty StartTmuxChat does not call ConfigureFinalizeHook at
// all. The hook port itself is read from l.hookPort so callers don't
// have to know about it.
type HookOpts struct {
	// Token is the per-run bearer token written into settings.local.json
	// for the run-scoped Stop hook. Empty disables run-keyed hook config.
	Token string
}

// Note: this method calls ConfigureFinalizeHook only when hookOpts.Token
// is non-empty. The cron path configures the hook earlier in StartSession
// (keyed by session_id); the repair path passes a run-keyed token here so
// the claude plugin can write a sibling Stop-hook entry.
func (l *Lifecycle) StartTmuxChat(ctx context.Context, sessionID string, input ChatInput, title string, hookOpts HookOpts) (string, error) {
	// Step 1a: tmux must be available; otherwise there's nowhere to host the run.
	if l.tmux == nil || !l.tmux.Available(ctx) {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"tmux unavailable: cannot host tmux chat for session %s", sessionID)
	}

	if l.agentChats == nil {
		// A nil AgentChatStore would leave the tmux session orphaned with
		// no matching DB row, which the boss UI cannot recover. Reject
		// early so callers don't leak a tmux session.
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"agentChats store unavailable: cannot host tmux chat for session %s", sessionID)
	}

	if l.agentLogsDir == "" {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"agent logs dir not configured: SetAgentLogsDir must be called before StartTmuxChat")
	}

	sess, err := l.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("get session %s: %w", sessionID, err)
	}
	if sess.WorktreePath == "" {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"session %s has no worktree path", sessionID)
	}

	// Step 1b: agent runner client for sess.AgentName must be loaded.
	client, err := l.agentClientFor(sess)
	if err != nil {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"agent runner not loaded for session %s: %v", sessionID, err)
	}

	// Step 2: idempotency check — if a previous launch's tmux is still
	// alive, treat this as a no-op and bubble the existing agent_session_id
	// up. Stale rows (tmux already exited) are cleaned out so the caller
	// can retry from a clean slate.
	if existing, found, idemErr := l.findLiveTmuxChat(ctx, sessionID); idemErr != nil {
		return "", idemErr
	} else if found {
		// Return the existing agent_session_id in the success-shaped string
		// slot alongside the typed AlreadyExists error so callers can read
		// it without parsing the human-readable message.
		return existing, grpcstatus.Errorf(codes.AlreadyExists,
			"tmux chat already active for session %s", sessionID)
	}

	// Step 3: mint a fresh agent session UUID and derive the tmux name.
	agentSessionID := uuid.NewString()
	tmuxName := tmux.ChatSessionName(sess.RepoID, agentSessionID)
	logPath := l.agentLogPathFor(agentSessionID)

	// Step 4: resolve argv via the plugin. The plugin owns flags like
	// --dangerously-skip-permissions and the tee-to-log redirect.
	cmdResp, err := client.BuildInteractiveCommand(ctx, &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:      agentSessionID,
		Resume:         false,
		LogPath:        logPath,
		InitialPrompt:  input.Prompt,
		InitialCommand: input.Command,
	})
	if err != nil {
		return "", fmt.Errorf("build interactive command for session %s: %w", sessionID, err)
	}
	if cmdResp == nil || len(cmdResp.Argv) == 0 {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"agent runner for session %s returned empty argv", sessionID)
	}

	if cmdResp.GetConsumesInitialInput() {
		if err := l.configureFinalizeHookForTmuxChat(ctx, client, sess.WorktreePath, sessionID, agentSessionID, hookOpts); err != nil {
			return "", err
		}
	}

	// Step 5: spawn the tmux session.
	if err := l.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    tmuxName,
		WorkDir: sess.WorktreePath,
		Command: cmdResp.Argv,
	}); err != nil {
		return "", fmt.Errorf("create tmux session %q: %w", tmuxName, err)
	}

	// Step 5a: arm pipe-pane so pane output is mirrored to logPath. This
	// replaces the previous in-process `claude … | tee log` wrapping the
	// claude plugin used to apply in BuildInteractiveCommand: piping the
	// agent's stdout through tee made isatty(stdout)=false, and modern
	// claude treats a non-TTY stdout as headless-print mode (it bails
	// with "Input must be provided either through stdin or as a prompt
	// argument when using --print"). pipe-pane attaches the mirror
	// outside the process so claude keeps a real PTY on stdout.
	//
	// Best-effort: a pipe-pane failure must not abort the chat launch.
	// The pane is alive, claude is running, the user can attach and
	// drive it interactively; losing on-disk capture is a degraded but
	// usable state.
	if err := l.tmux.PipePane(ctx, tmuxName, logPath); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Str("tmuxSession", tmuxName).
			Str("logPath", logPath).
			Msg("pipe-pane failed; chat continues without on-disk capture")
	}

	l.logger.Info().
		Str("session", sessionID).
		Str("agentSessionID", agentSessionID).
		Str("tmuxSession", tmuxName).
		Str("worktree", sess.WorktreePath).
		Msg("started agent inside tmux")

	// Step 6: persist the agent_chats row. Failure here orphans the tmux
	// session so we tear it back down before returning.
	if _, err := l.agentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: agentSessionID,
		AgentName:      sess.AgentName,
		Title:          title,
	}); err != nil {
		l.killTmuxChatBestEffort(ctx, sessionID, agentSessionID, tmuxName)
		return "", fmt.Errorf("create agent_chats row for session %s: %w", sessionID, err)
	}

	// Step 7: stamp the tmux session name onto the row.
	if err := l.agentChats.UpdateTmuxSessionName(ctx, agentSessionID, &tmuxName); err != nil {
		l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName,
			"persist tmux session name failed: "+err.Error())
		return "", fmt.Errorf("persist tmux session name for session %s: %w", sessionID, err)
	}

	// Step 8: install a run-scoped Stop-hook entry when the caller asked
	// for one. If argv already embeds startup input, this was done before
	// tmux launch so a fast-finishing agent cannot beat hook configuration.
	if !cmdResp.GetConsumesInitialInput() {
		if err := l.configureFinalizeHookForTmuxChat(ctx, client, sess.WorktreePath, sessionID, agentSessionID, hookOpts); err != nil {
			l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName,
				"configure finalize hook failed: "+err.Error())
			return "", err
		}
	}

	if cmdResp.GetConsumesInitialInput() {
		return agentSessionID, nil
	}

	// Step 9: inject the prompt. Long prompt bodies use bracketed paste.
	// Short command invocations use literal keys plus Enter; Codex has been
	// observed leaving pasted "$boss-repair" text sitting idle in the input box.
	prompt := input.render(cmdResp.GetCommandPrefix())
	if input.Command != "" {
		if err := l.tmux.SendLineWithReadyMarker(ctx, tmuxName, prompt, cmdResp.GetReadyMarker()); err != nil {
			l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName,
				"send command failed: "+err.Error())
			return "", fmt.Errorf("send command to tmux session %q: %w", tmuxName, err)
		}
		return agentSessionID, nil
	}

	// Step 9b: inject the prompt as a bracketed paste. This is the step
	// that broke for every repair attempt before #350: the broken
	// `bash -c "claude … | tee log"` argv caused the pane to show a
	// "--print needs stdin or prompt" error from claude and the ready
	// marker (❯) never appeared, so SendPlan timed out at 5s. We now
	// preserve the chat row with a start_error rather than deleting it,
	// so the operator can see exactly what was attempted even when the
	// agent never came up.
	if err := l.tmux.SendPlanWithReadyMarker(ctx, tmuxName, prompt, cmdResp.GetReadyMarker()); err != nil {
		l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName,
			"send plan failed: "+err.Error())
		return "", fmt.Errorf("send plan to tmux session %q: %w", tmuxName, err)
	}

	return agentSessionID, nil
}

func (l *Lifecycle) configureFinalizeHookForTmuxChat(ctx context.Context, client agent.AgentRunnerClient, workDir, sessionID, agentSessionID string, hookOpts HookOpts) error {
	if hookOpts.Token == "" {
		return nil
	}
	if l.hookPort == 0 {
		return grpcstatus.Errorf(codes.FailedPrecondition,
			"hook port not configured: SetHookPort must be called before StartTmuxChat with a hook token")
	}
	hookResp, err := client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir:        workDir,
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		HookToken:      hookOpts.Token,
		HookPort:       int32(l.hookPort),
	})
	if err != nil {
		return fmt.Errorf("configure finalize hook for run %s: %w", agentSessionID, err)
	}
	if !hookResp.GetIsSupported() {
		l.armPollFallbackForTmuxChat(agentSessionID, client)
	}
	return nil
}

func (l *Lifecycle) armPollFallbackForTmuxChat(agentSessionID string, client agent.AgentRunnerClient) {
	if l.pollArmer == nil || l.daemonCtx == nil {
		return
	}
	l.pollArmer.Arm(l.daemonCtx, agentSessionID, client)
	l.logger.Info().
		Str("agent_session", agentSessionID).
		Msg("armed poll fallback for hookless interactive tmux chat")
}

// failStartBestEffort tears down a tmux session created by a failing
// StartTmuxChat AND stamps the agent_chats row with a short reason so
// the row stays visible in the chat list as a "(failed to start)"
// entry. Replaces the previous teardown shape that deleted the row —
// that made hammer-looping failures (e.g. repair on a PR with a 5×
// SendPlan timeout per minute) silently invisible to the operator.
//
// Best-effort: both the tmux kill and the DB stamp log on failure but
// never bubble up, because the caller is already on a failure path and
// has its own error to surface.
func (l *Lifecycle) failStartBestEffort(ctx context.Context, sessionID, agentSessionID, tmuxName, reason string) {
	l.killTmuxChatBestEffort(ctx, sessionID, agentSessionID, tmuxName)
	if markErr := l.agentChats.MarkStartFailed(ctx, agentSessionID, reason); markErr != nil {
		l.logger.Warn().Err(markErr).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Str("reason", reason).
			Msg("failed to mark agent_chat row as start-failed; row may still show as live")
	}
}

// findLiveTmuxChat scans the session's existing agent_chats rows for an
// entry whose tmux_session_name is still alive. It returns:
//
//   - (agentSessionID, true, nil) when a live tmux chat is found.
//   - ("", false, nil) when no row matches OR the rows that exist have a
//     stale tmux name (those rows have their tmux_session_name cleared
//     as a side effect so they no longer count toward idempotency, but
//     the rows themselves are preserved as historical chat-list
//     entries — eg. completed repair runs the operator should still be
//     able to see).
//   - ("", false, err) when ListBySession itself fails.
//
// Best-effort: if clearing a stale row's tmux_session_name fails, we log
// and continue — the caller will create a new agent_chats row anyway,
// and the next sweep can retry the unlink.
func (l *Lifecycle) findLiveTmuxChat(ctx context.Context, sessionID string) (string, bool, error) {
	chats, err := l.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		return "", false, fmt.Errorf("list chats for session %s: %w", sessionID, err)
	}
	for _, chat := range chats {
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		if l.tmux.HasSession(ctx, *chat.TmuxSessionName) {
			return chat.AgentSessionID, true, nil
		}
		// Stale tmux name — clear it so a retry can mint a fresh
		// agent_session_id and tmux session. The row itself is preserved
		// so the chat list still surfaces the historical run; only the
		// pointer to a now-dead tmux session is removed.
		if updErr := l.agentChats.UpdateTmuxSessionName(ctx, chat.AgentSessionID, nil); updErr != nil {
			l.logger.Warn().Err(updErr).
				Str("session", sessionID).
				Str("agentSessionID", chat.AgentSessionID).
				Msg("failed to clear stale tmux_session_name during idempotency cleanup")
		}
	}
	return "", false, nil
}

func (l *Lifecycle) ReclaimRepairChat(ctx context.Context, sessionID, agentSessionID, reason string) (ReclaimRepairChatResult, error) {
	if sessionID == "" {
		return ReclaimRepairChatResult{}, fmt.Errorf("session_id is required")
	}
	if agentSessionID == "" {
		return ReclaimRepairChatResult{}, fmt.Errorf("agent_session_id is required")
	}
	if reason == "" {
		reason = "reclaimed stale repair chat"
	}
	if l.agentChats == nil {
		return ReclaimRepairChatResult{}, fmt.Errorf("agent chat store not configured")
	}

	chat, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		return ReclaimRepairChatResult{}, err
	}
	if chat == nil {
		return ReclaimRepairChatResult{}, fmt.Errorf("agent chat not found for agent_session_id %q", agentSessionID)
	}
	if chat.SessionID != sessionID {
		return ReclaimRepairChatResult{}, fmt.Errorf("%w: chat session %s requested session %s", ErrRepairChatSessionMismatch, chat.SessionID, sessionID)
	}
	if !strings.HasPrefix(chat.Title, repairChatTitlePrefix) {
		return ReclaimRepairChatResult{}, fmt.Errorf("%w: title %q", ErrRepairChatNotReclaimable, chat.Title)
	}

	result := ReclaimRepairChatResult{Reclaimed: true}
	if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		result.TmuxSessionName = *chat.TmuxSessionName
		if l.tmux == nil {
			return ReclaimRepairChatResult{}, fmt.Errorf("%w: tmux client not configured; cannot verify repair chat %s is dead", ErrRepairChatActive, result.TmuxSessionName)
		}
		tmuxAlive, tmuxErr := l.tmux.HasSessionStatus(ctx, result.TmuxSessionName)
		if tmuxErr != nil {
			return ReclaimRepairChatResult{}, fmt.Errorf("%w: cannot verify tmux session %s liveness: %v", ErrRepairChatActive, result.TmuxSessionName, tmuxErr)
		}
		if tmuxAlive {
			stale, staleReason, staleErr := l.repairChatStaleForReclaim(chat)
			if staleErr != nil {
				return ReclaimRepairChatResult{}, staleErr
			}
			if !stale {
				return ReclaimRepairChatResult{}, fmt.Errorf("%w: %s", ErrRepairChatActive, staleReason)
			}
			l.logger.Info().
				Str("session", sessionID).
				Str("agentSessionID", agentSessionID).
				Str("tmuxSession", result.TmuxSessionName).
				Str("reason", staleReason).
				Msg("repair chat is stale enough to reclaim")
			if err := l.tmux.KillSession(ctx, result.TmuxSessionName); err != nil {
				return ReclaimRepairChatResult{}, fmt.Errorf("kill repair tmux session %s: %w", result.TmuxSessionName, err)
			}
		}
	}
	if err := l.agentChats.MarkStartFailed(ctx, agentSessionID, reason); err != nil {
		return ReclaimRepairChatResult{}, err
	}
	return result, nil
}

func (l *Lifecycle) repairChatStaleForReclaim(chat *models.AgentChat) (bool, string, error) {
	return RepairChatStaleForReclaim(l.agentLogsDir, chat, time.Now())
}

// KillChatTmuxSession tears down the tmux-backed chat identified by
// agentSessionID and only clears the chat row's tmux pointer after that tmux
// session is gone.
func (l *Lifecycle) KillChatTmuxSession(ctx context.Context, sessionID, agentSessionID string) error {
	if l.agentChats == nil {
		return fmt.Errorf("agent chat store not configured")
	}
	chat, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil {
		return fmt.Errorf("get agent chat %s: %w", agentSessionID, err)
	}
	if chat == nil {
		return fmt.Errorf("agent chat not found for agent_session_id %q", agentSessionID)
	}
	if chat.SessionID != sessionID {
		return fmt.Errorf("agent chat %s belongs to session %s, want %s", agentSessionID, chat.SessionID, sessionID)
	}
	if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
		return nil
	}
	tmuxName := *chat.TmuxSessionName
	if l.tmux == nil || !l.tmux.Available(ctx) {
		return fmt.Errorf("tmux unavailable: cannot tear down chat tmux session %s", tmuxName)
	}
	if err := l.tmux.KillSession(ctx, tmuxName); err != nil {
		return fmt.Errorf("kill chat tmux session %s: %w", tmuxName, err)
	}
	if err := l.agentChats.UpdateTmuxSessionName(ctx, agentSessionID, nil); err != nil {
		return fmt.Errorf("clear chat tmux session name for %s: %w", agentSessionID, err)
	}
	return nil
}

// agentLogPathFor returns the bossd-owned log path for an agent session
// inside agentLogsDir. Mirrors the per-session naming used elsewhere
// (PluginRunner.logPathFor) so a single tail can follow either a headless
// or interactive run by agent_session_id.
func (l *Lifecycle) agentLogPathFor(agentSessionID string) string {
	return agentLogPathFor(l.agentLogsDir, agentSessionID)
}

func agentLogPathFor(agentLogsDir, agentSessionID string) string {
	return filepath.Join(agentLogsDir, agentSessionID+".log")
}

// startCronTmuxChat is a thin wrapper around StartTmuxChat that supplies
// the cron-specific prompt (the session plan) and title (`Run "<cron name>"`).
// All actual lifecycle work — tmux spawn, agent_chats row, idempotency,
// cleanup — lives in StartTmuxChat. The cron caller in StartSession can
// keep using this signature without caring about the generalization.
func (l *Lifecycle) startCronTmuxChat(
	ctx context.Context,
	sessionID string,
	_ StartSessionOpts,
	session *models.Session,
	_ *gitpkg.CreateResult,
) (string, error) {
	cronName := session.Title
	// Cron sessions wire their session-keyed Stop hook earlier in
	// StartSession; pass an empty HookOpts so StartTmuxChat doesn't
	// install a duplicate run-keyed entry.
	return l.StartTmuxChat(ctx, sessionID, ChatInput{Prompt: session.Plan}, `Run "`+cronName+`"`, HookOpts{})
}

// killTmuxChatBestEffort tears down a tmux session created during a failed
// StartTmuxChat. Errors are logged but never returned — the caller is
// already on a failure path and the surfaced error wraps the original
// cause.
func (l *Lifecycle) killTmuxChatBestEffort(ctx context.Context, sessionID, agentSessionID, tmuxName string) {
	if l.tmux == nil {
		return
	}
	if err := l.tmux.KillSession(ctx, tmuxName); err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Str("tmuxSession", tmuxName).
			Msg("failed to kill tmux session during failure cleanup")
	}
}
