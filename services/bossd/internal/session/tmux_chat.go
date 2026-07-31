package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/recurser/bossalib/config"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	libskillinstall "github.com/recurser/bossalib/skillinstall"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/dotenv"
	gitpkg "github.com/recurser/bossd/internal/git"
	"github.com/recurser/bossd/internal/tmux"
)

const defaultAgentCommandPrefix = "/"

var (
	ErrRepairChatNotReclaimable  = errors.New("repair chat not reclaimable")
	ErrRepairChatSessionMismatch = errors.New("repair chat belongs to a different session")
	ErrRepairChatActive          = errors.New("repair chat active or not stale")
)

// capabilityPreflightError marks a headless-capability preflight failure raised
// by the authoritative probe in startTmuxChat. Unlike the post-setup probe in
// StartSession, that one runs after the worktree and branch have already been
// persisted, so StartSession matches this type with errors.As to roll both back
// (the cron caller in taskorchestrator drops only the session row, which would
// otherwise strand them and make the next attempt collide with the branch).
// Every other startTmuxChat failure keeps its existing no-rollback behavior.
//
// Error() returns the wrapped message verbatim, so wrapping in this type does
// not change what callers log or surface.
type capabilityPreflightError struct{ err error }

func (e capabilityPreflightError) Error() string { return e.err.Error() }

func (e capabilityPreflightError) Unwrap() error { return e.err }

type ReclaimRepairChatResult struct {
	Reclaimed       bool
	TmuxSessionName string
}

const repairChatTitlePrefix = "Repair:"

// IsRepairChatTitle reports whether a chat row belongs to the unattended
// repair workflow. Repair chats have their own displacement policy and should
// not be counted as user activity for repair idle gating.
func IsRepairChatTitle(title string) bool {
	return strings.HasPrefix(title, repairChatTitlePrefix)
}

// DeliveryIntent is how a ChatInput's payload is delivered to the tmux composer.
type DeliveryIntent int

const (
	// DeliveryPrefillOnly prefills the composer without submitting — the safe
	// zero value: a caller that forgets to set intent never triggers an
	// unwanted auto-run.
	DeliveryPrefillOnly DeliveryIntent = iota
	// DeliverySubmit types/pastes the payload, presses Enter, and verifies it
	// left the composer (retry once, then loud error). Set by unattended
	// provenance.
	DeliverySubmit
)

// ChatInput is the pane input for a tmux-hosted agent chat. Prompt is raw text;
// Command is a boss command name without an agent-specific prefix.
type ChatInput struct {
	Prompt  string
	Command string
	// Delivery is the explicit delivery intent for this input: whether the
	// payload is submitted-and-verified (DeliverySubmit) or merely prefilled
	// into the composer for someone else to submit (DeliveryPrefillOnly, the
	// zero value). Intent is derived from session provenance at the call site,
	// never guessed from the payload's content.
	Delivery DeliveryIntent
	// ResumeAgentSessionID, when set, resumes this prior agent session instead
	// of minting a fresh one (claude `--resume <id>`), preserving the agent's
	// memory of earlier attempts. The id is reused as the bossd correlation key
	// (log path, finalize hook, tmux name, completion arming) so everything
	// stays keyed on a single id.
	ResumeAgentSessionID string
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

// chatInputMechanicsFromPrompt selects the delivery MECHANICS for a stored plan:
// whether the payload is carried as a boss command (the Command field, which the
// tmux layer delivers via literal send-keys with the agent's command prefix) or
// as raw prompt text (the Prompt field, delivered via bracketed paste). It makes
// no decision about whether the payload is submitted — that delivery intent is
// set by the caller from session provenance (ChatInput.Delivery).
//
// The payload is carried as a Command when, after trimming surrounding
// whitespace, its leading token begins with "/" or "$", with or without
// arguments — e.g. "/bs-sweep-mutation" or "/wc-merge-review headless". The whole
// single line (command + args) is preserved. Multi-line payloads, and free-text
// whose leading token is not "/" or "$" (including text that merely contains an
// embedded slash/dollar such as a path, URL, or price), are carried as a Prompt.
// Detection is deliberately leading-token-only: matching an embedded "/" or "$"
// would truncate legitimate free-text into the command field and change how it is
// keyed for delivery.
func chatInputMechanicsFromPrompt(prompt string) ChatInput {
	trimmed := strings.TrimSpace(prompt)
	if strings.ContainsAny(trimmed, "\r\n") {
		return ChatInput{Prompt: prompt}
	}
	if strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "$") {
		return ChatInput{Command: trimmed}
	}
	return ChatInput{Prompt: prompt}
}

// isInstalledSkillCommand reports whether command's leading token (after its
// "/" or "$" prefix) names an installed custom skill for the target agent. The
// check is name-based instead of namespace-based so project-local commands such
// as api-review/tui-qa and shipped bs-* commands are adapted too, while native
// agent built-ins ("/model", "/status") are delivered verbatim when no matching
// skill exists on disk.
func isInstalledSkillCommand(command, commandPrefix, worktreePath string) bool {
	name := skillCommandName(command)
	if name == "" {
		return false
	}
	for _, dir := range skillSearchDirs(commandPrefix, worktreePath) {
		if skillExistsInDir(dir, name) {
			return true
		}
	}
	return false
}

func skillCommandName(command string) string {
	body := strings.TrimLeft(command, "/$")
	fields := strings.Fields(body)
	if len(fields) == 0 {
		return ""
	}
	name := fields[0]
	if strings.ContainsAny(name, `/\`) {
		return ""
	}
	return name
}

func skillSearchDirs(commandPrefix, worktreePath string) []string {
	agentDir := ".claude"
	if commandPrefix == "$" {
		agentDir = ".codex"
	}
	var dirs []string
	if worktreePath != "" {
		dirs = append(dirs, filepath.Join(worktreePath, agentDir, "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, agentDir, "skills"))
	}
	return dirs
}

func skillExistsInDir(dir, name string) bool {
	if dir == "" {
		return false
	}
	if hasSkillFile(filepath.Join(dir, name)) {
		return true
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*", name))
	if err != nil {
		return false
	}
	for _, match := range matches {
		if hasSkillFile(match) {
			return true
		}
	}
	return false
}

func hasSkillFile(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && !info.IsDir()
}

// RenderBossCommandPrefix rewrites an installed custom skill command's leading
// prefix to the given agent command prefix, leaving everything else untouched:
// native agent built-ins ("/model", "/status"), free text, and multi-line input
// all pass through verbatim. It is the live-send analogue of the plan-launch
// rendering (chatInputMechanicsFromPrompt + ChatInput.render), scoped to the
// custom skill names installed for the target agent so a caller hands an
// agent-neutral "/boss-repair watch" or "/api-review" to a codex chat (prefix
// "$") and the chat receives "$boss-repair watch" or "$api-review", while a
// codex user's "/status" reaches codex unchanged. An empty prefix defaults to
// claude's "/".
func RenderBossCommandPrefix(message, commandPrefix, worktreePath string) string {
	input := chatInputMechanicsFromPrompt(message)
	if input.Command == "" || !isInstalledSkillCommand(input.Command, commandPrefix, worktreePath) {
		return message
	}
	return input.render(commandPrefix)
}

// StartTmuxChat boots a Claude (or other agent) run inside a detached tmux
// session and registers it as an agent_chats row so the chat list view can
// surface it. It is the generalized form of the cron-only helper that
// formerly lived under startTmuxChat: any caller that needs a tmux-
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

	// AllowSiblingChat bypasses the per-session live-chat idempotency check
	// for callers that intentionally launch an additional chat next to an
	// existing primary chat, such as cron finalization.
	AllowSiblingChat bool

	// HeadlessCapabilityProfile is the profile StartSession deferred until this
	// worktree's effective tmux environment was available. Direct chat callers
	// leave it unspecified and retain their existing launch path.
	HeadlessCapabilityProfile bossanovav1.HeadlessCapabilityProfile
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

	// Step 2: resolve the requested resume target before idempotency. A
	// completed repair pass can leave its tmux pane alive at the prompt; the
	// next repair iteration should send the resumed command into that same
	// pane instead of tripping the generic duplicate-chat guard.
	resuming := input.ResumeAgentSessionID != ""
	agentSessionID := input.ResumeAgentSessionID

	// Step 3: idempotency check — if a previous launch's tmux is still
	// alive, treat this as a no-op and bubble the existing agent_session_id
	// up. Stale rows (tmux already exited) are cleaned out so the caller
	// can retry from a clean slate. Some callers intentionally launch a
	// sibling chat next to an existing primary chat; they opt out via
	// HookOpts.AllowSiblingChat.
	if !hookOpts.AllowSiblingChat {
		if existing, found, idemErr := l.findLiveTmuxChat(ctx, sessionID); idemErr != nil {
			return "", idemErr
		} else if found {
			if resuming && liveChatMatchesResumeTarget(existing, agentSessionID) {
				return l.sendInputToLiveTmuxChat(ctx, sess, client, input, existing, agentSessionID, hookOpts)
			}
			// Return the existing agent_session_id in the success-shaped string
			// slot alongside the typed AlreadyExists error so callers can read
			// it without parsing the human-readable message.
			return existing.AgentSessionID, grpcstatus.Errorf(codes.AlreadyExists,
				"tmux chat already active for session %s", sessionID)
		}
	}

	// Step 4: resolve the agent session id. A resume request reuses the prior
	// id so logs/hooks/tmux/completion all stay keyed on one id; otherwise mint
	// a fresh one.
	if !resuming {
		agentSessionID = l.newTmuxChatAgentSessionID()
	}
	tmuxName := tmux.ChatSessionName(sess.RepoID, agentSessionID)
	logPath := l.agentLogPathFor(agentSessionID)

	// BOS-381: provider/account/model authority lives on the chat. For a fresh
	// launch this is the first chat, so the session's resolved seed governs. For
	// a resume/respawn (e.g. a cross-agent switch_account respawn) the existing
	// chat row already carries its own provider/account/model — read them so the
	// respawn dispatches under the chat's runner, credentials, and model rather
	// than the session's stale seed. A chat that never bound its own account
	// (nil) or model ("") inherits the session's mirrored value.
	spawnAgentName, spawnModel, spawnAccountID := sess.AgentName, sess.Model, sess.AccountID
	if resuming && l.agentChats != nil {
		if existing, gerr := l.agentChats.GetByAgentSessionID(ctx, agentSessionID); gerr == nil && existing != nil {
			if existing.AgentName != "" {
				spawnAgentName = existing.AgentName
			}
			if existing.Model != "" {
				spawnModel = existing.Model
			}
			if existing.AccountID != nil {
				spawnAccountID = existing.AccountID
			}
		}
	}
	// Re-select the runner plugin when the chat's provider differs from the
	// session's — a cross-agent chat spawns its own runner.
	if spawnAgentName != sess.AgentName {
		if c, cerr := l.clientForAgentName(spawnAgentName); cerr == nil && c != nil {
			client = c
		} else if cerr != nil {
			return "", grpcstatus.Errorf(codes.FailedPrecondition,
				"agent runner not loaded for chat %s: %v", agentSessionID, cerr)
		}
	}
	// Build a session view carrying the chat's provider/account for the env
	// overlay resolvers (account materialization + failover proxy are keyed off
	// AgentName/AccountID). Shares all other fields by value.
	spawnSess := sess
	if spawnAgentName != sess.AgentName || spawnAccountID != sess.AccountID {
		cp := *sess
		cp.AgentName = spawnAgentName
		cp.AccountID = spawnAccountID
		spawnSess = &cp
	}

	// Step 5: resolve argv via the plugin. The plugin owns flags like
	// --dangerously-skip-permissions and the tee-to-log redirect.
	mcpConfigPath := l.writeSessionMcpConfig(sess, agentSessionID, sessionID)
	cmdResp, err := client.BuildInteractiveCommand(ctx, &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:          agentSessionID,
		Resume:             resuming,
		LogPath:            logPath,
		InitialPrompt:      input.Prompt,
		InitialCommand:     input.Command,
		WorktreePath:       sess.WorktreePath,
		AppendSystemPrompt: AppendSystemPromptFor(sess, agentSessionID, spawnAgentName, mcpConfigPath),
		Model:              spawnModel,
		McpConfigPath:      mcpConfigPath,
		StrictMcpConfig:    isCronSession(sess),
	})
	if err != nil {
		return "", fmt.Errorf("build interactive command for session %s: %w", sessionID, err)
	}
	if cmdResp == nil || len(cmdResp.Argv) == 0 {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"agent runner for session %s returned empty argv", sessionID)
	}

	// Build the env once, then validate and launch against that exact map. A
	// tmux-hosted launch can receive CODEX_HOME or HOME from its worktree .env,
	// so this is the only environment the tmux child actually inherits.
	//
	// StartSession has already run the same check before creating the worktree,
	// against the repo checkout's .env — the file this worktree's .env is copied
	// from — so a bad profile is normally rejected without stranding artifacts.
	// This re-check is authoritative for the case those two can diverge (a repo
	// whose setup script is skipped or does not copy .env), and it still runs
	// before hook configuration and tmux creation. Unlike the first probe it
	// runs after the worktree and branch have been persisted, so both failures
	// below are wrapped in capabilityPreflightError — StartSession matches that
	// type and applies the same rollback rather than stranding them.
	repo := RepoForSessionEnv(ctx, l.repos, sess.RepoID, sess.ID, "start tmux chat", l.logger)
	tmuxEnv := resolveWorktreeRelativeHomes(dotenv.OverlayWithRepo(mergeSessionEnv(ManagedSessionEnv(sess, agentSessionID, spawnAgentName), l.resolveAccountEnv(ctx, spawnSess), l.resolveProofEnv()), sess.WorktreePath, repo), sess.WorktreePath)
	if hookOpts.HeadlessCapabilityProfile != bossanovav1.HeadlessCapabilityProfile_HEADLESS_CAPABILITY_PROFILE_UNSPECIFIED {
		dispatcher, ok := l.agentRunner.(agent.HeadlessCapabilityProfilePreflightDispatcher)
		if !ok {
			return "", capabilityPreflightError{fmt.Errorf("headless capability profile %s for agent %q requires profile-aware agent dispatcher", hookOpts.HeadlessCapabilityProfile, spawnAgentName)}
		}
		if err := dispatcher.PreflightByAgentWithHeadlessCapabilityProfile(ctx, spawnAgentName, spawnModel, tmuxEnv, hookOpts.HeadlessCapabilityProfile); err != nil {
			return "", capabilityPreflightError{fmt.Errorf("preflight headless capabilities for agent %q with profile %s: %w", spawnAgentName, hookOpts.HeadlessCapabilityProfile, err)}
		}
	}

	finalizeHookSupported := true
	if cmdResp.GetConsumesInitialInput() {
		hookSupported, err := l.configureFinalizeHookForTmuxChat(ctx, client, sess.WorktreePath, sessionID, agentSessionID, hookOpts)
		if err != nil {
			return "", err
		}
		finalizeHookSupported = hookSupported
	}

	// Step 6: spawn the tmux session. Cron-spawned sessions get BOSS_CRON=true
	// (and job id/name) in the session environment so the agent — and any skill
	// it runs — can detect that it is an unattended, autonomous run.
	// The session environment merges three layers with precedence managed
	// (BOSS_*) > account > proof: the allowlisted proof env (proof
	// credentials + non-secret proof constants) so the agentic proof
	// pipeline can run in this chat — including unattended cron worktrees,
	// which reach here and nowhere else — and, beneath managed but above
	// proof, an account env overlay for account rotation (empty/no-op in
	// this ticket; BOS-170 wires real per-session account binding). Managed
	// BOSS_* keys always win on conflict; secrets are never placed on
	// SessionFacts (which feeds the system prompt) and never logged. The
	// worktree's repo-local .env is overlaid beneath everything
	// (dotenv.Overlay: absent .env is a no-op) so ${VAR} expansions in the
	// worktree's .mcp.json resolve like they would in a developer's direnv
	// shell, and the repo's stored LINEAR_API_KEY / SENTRY_* secrets are filled
	// beneath the .env (dotenv.OverlayWithRepo) so a session authenticates to
	// its own repo's Linear workspace instead of the daemon's ambient one.
	//
	// A repo lookup failure is non-fatal: OverlayWithRepo(nil) still guarantees
	// LINEAR_API_KEY is present so the daemon's ambient value can never leak.
	if err := l.tmux.NewSession(ctx, tmux.NewSessionOpts{
		Name:    tmuxName,
		WorkDir: sess.WorktreePath,
		Command: cmdResp.Argv,
		Env:     tmuxEnv,
	}); err != nil {
		// tmux's error carries its own stderr (e.g. a missing-terminfo
		// "missing or unsuitable terminal" failure). The normal agent_chats row
		// (Step 7) does not exist yet, so create-and-stamp a "(failed to start)"
		// row here best-effort — that surfaces the real reason to boss show, the
		// TUI badge, and external clients instead of only logging it.
		l.recordFailedStartChat(ctx, sessionID, agentSessionID, spawnAgentName, title,
			fmt.Sprintf("tmux launch failed: %v", err))
		return "", fmt.Errorf("create tmux session %q: %w", tmuxName, err)
	}

	// Step 6a: arm pipe-pane so pane output is mirrored to logPath. This
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

	// On resume the prior chat row still exists (findLiveTmuxChat preserves it,
	// only clearing its tmux pointer) and agent_session_id is not unique, so a
	// fresh Create would insert a duplicate row. Delete the stale row first so
	// exactly one row carries the reused id.
	if resuming {
		if err := l.agentChats.DeleteByAgentSessionID(ctx, agentSessionID); err != nil {
			l.killTmuxChatBestEffort(ctx, sessionID, agentSessionID, tmuxName)
			return "", fmt.Errorf("delete stale agent_chats row for resume of session %s: %w", sessionID, err)
		}
	}

	// Step 7: persist the agent_chats row. Failure here orphans the tmux
	// session so we tear it back down before returning.
	if _, err := l.agentChats.Create(ctx, db.CreateAgentChatParams{
		SessionID:      sessionID,
		AgentSessionID: agentSessionID,
		AgentName:      spawnAgentName,
		Title:          title,
		AccountID:      spawnAccountID,
		Model:          spawnModel,
	}); err != nil {
		l.killTmuxChatBestEffort(ctx, sessionID, agentSessionID, tmuxName)
		return "", fmt.Errorf("create agent_chats row for session %s: %w", sessionID, err)
	}

	// Step 8: stamp the tmux session name onto the row.
	if err := l.agentChats.UpdateTmuxSessionName(ctx, agentSessionID, &tmuxName); err != nil {
		l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName,
			"persist tmux session name failed: "+err.Error())
		return "", fmt.Errorf("persist tmux session name for session %s: %w", sessionID, err)
	}

	// Step 9: install a run-scoped Stop-hook entry when the caller asked
	// for one. If argv already embeds startup input, this was done before
	// tmux launch so a fast-finishing agent cannot beat hook configuration.
	if !cmdResp.GetConsumesInitialInput() {
		hookSupported, err := l.configureFinalizeHookForTmuxChat(ctx, client, sess.WorktreePath, sessionID, agentSessionID, hookOpts)
		if err != nil {
			l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName,
				"configure finalize hook failed: "+err.Error())
			return "", err
		}
		finalizeHookSupported = hookSupported
	}

	if !finalizeHookSupported {
		l.armTmuxCompletionForHooklessRun(sessionID, agentSessionID, sess.RepoID)
	}

	if cmdResp.GetConsumesInitialInput() {
		return agentSessionID, nil
	}

	// Step 10: inject the prompt. This is the step
	// that broke for every repair attempt before #350: the broken
	// `bash -c "claude … | tee log"` argv caused the pane to show a
	// "--print needs stdin or prompt" error from claude and the ready
	// marker (❯) never appeared, so SendPlan timed out at 5s. We now
	// preserve the chat row with a start_error rather than deleting it,
	// so the operator can see exactly what was attempted even when the
	// agent never came up.
	if err := l.injectTmuxChatInput(ctx, tmuxName, input, cmdResp); err != nil {
		reason := "send plan failed: " + err.Error()
		if input.Command != "" {
			reason = "send command failed: " + err.Error()
		}
		l.failStartBestEffort(ctx, sessionID, agentSessionID, tmuxName, reason)
		return "", err
	}

	return agentSessionID, nil
}

func (l *Lifecycle) sendInputToLiveTmuxChat(ctx context.Context, sess *models.Session, client agent.AgentRunnerClient, input ChatInput, chat *models.AgentChat, resumeSessionID string, hookOpts HookOpts) (string, error) {
	agentSessionID := chat.AgentSessionID
	tmuxName := ""
	if chat.TmuxSessionName != nil {
		tmuxName = *chat.TmuxSessionName
	}
	mcpConfigPath := l.writeSessionMcpConfig(sess, agentSessionID, sess.ID)
	cmdResp, err := client.BuildInteractiveCommand(ctx, &bossanovav1.BuildInteractiveCommandRequest{
		SessionId:       resumeSessionID,
		Resume:          true,
		WorktreePath:    sess.WorktreePath,
		LogPath:         l.agentLogPathFor(agentSessionID),
		InitialPrompt:   input.Prompt,
		InitialCommand:  input.Command,
		Model:           sess.Model,
		McpConfigPath:   mcpConfigPath,
		StrictMcpConfig: isCronSession(sess),
	})
	if err != nil {
		return "", fmt.Errorf("build interactive command for session %s: %w", sess.ID, err)
	}
	if cmdResp == nil || len(cmdResp.Argv) == 0 {
		return "", grpcstatus.Errorf(codes.FailedPrecondition,
			"agent runner for session %s returned empty argv", sess.ID)
	}

	finalizeHookSupported, err := l.configureFinalizeHookForTmuxChat(ctx, client, sess.WorktreePath, sess.ID, agentSessionID, hookOpts)
	if err != nil {
		return "", err
	}
	if !finalizeHookSupported {
		l.armTmuxCompletionForHooklessTmux(sess.ID, agentSessionID, tmuxName)
	}

	l.logger.Info().
		Str("session", sess.ID).
		Str("agentSessionID", agentSessionID).
		Str("tmuxSession", tmuxName).
		Msg("resuming agent inside existing tmux")

	// Existing panes cannot consume startup input via argv; always inject the
	// rendered input into the live prompt.
	if err := l.injectTmuxChatInput(ctx, tmuxName, input, cmdResp); err != nil {
		return "", err
	}
	return agentSessionID, nil
}

func liveChatMatchesResumeTarget(chat *models.AgentChat, resumeSessionID string) bool {
	if chat == nil || resumeSessionID == "" {
		return false
	}
	if chat.AgentSessionID == resumeSessionID {
		return true
	}
	return chat.ProviderSessionID != nil && *chat.ProviderSessionID == resumeSessionID
}

// injectTmuxChatInput delivers input into the live tmux composer. The Command vs
// Prompt field selects the delivery mechanics (literal send-keys for a boss
// command, bracketed paste for raw text); input.Delivery selects the intent —
// DeliverySubmit types/pastes then presses Enter and verifies the payload left
// the composer, while DeliveryPrefillOnly (the zero value) delivers into the
// composer and stops there so the composer owner submits.
//
// BOS-600 gave the readiness gate two halves and this path carries only one.
// Row-anchored composer resolution applies here (it lives inside
// waitForReadyMarker, which every entry point below funnels through), so a
// marker glyph drawn mid-row no longer reads as ready. The agent-grammar half —
// the injected ModalDetector — is NOT installed here; it is bound per call on
// the SendChatMessage path only (server.modalDetectorFor).
//
// That is a deliberate limit, not an oversight. The grammars the detector routes
// to (hasCodexQuestionPrompt, statusdetect.HasModalPrompt) recognise approval
// menus and question pickers — states a *conversing* agent enters. The panes this
// path meets are freshly started or freshly resumed, where the realistic modal is
// an update interstitial or an auth prompt, which those grammars do not match; a
// detector here would cost a plugin round-trip per poll tick and refuse nothing.
//
// Be precise about what that leaves uncovered, because it includes the incident
// BOS-600 was opened for: a codex pane that *opened* on an "Update available!"
// interstitial, i.e. this path, not SendChatMessage. What this path gained is the
// row-anchoring half, and whether that alone refuses the interstitial depends on
// whether the interstitial draws a row led by "›" — which nobody knows, because
// the rendering was never captured. So this path is improved but does NOT prove
// the motivating case is prevented, and the same is true of a resume branch
// reaching a pane left mid-menu.
//
// Closing it needs a detector grammar for boot interstitials (the parent epic's
// requirement 5), which needs a real capture first; the plan's instruction for
// exactly this outcome was to disclose it rather than commit a guessed fixture.
func (l *Lifecycle) injectTmuxChatInput(ctx context.Context, tmuxName string, input ChatInput, cmdResp *bossanovav1.BuildInteractiveCommandResponse) error {
	prompt := input.render(cmdResp.GetCommandPrefix())
	marker := cmdResp.GetReadyMarker()
	if input.Delivery == DeliverySubmit {
		if input.Command != "" {
			if err := l.tmux.SendLineWithReadyMarker(ctx, tmuxName, prompt, marker); err != nil {
				return fmt.Errorf("send command to tmux session %q: %w", tmuxName, err)
			}
			return nil
		}
		if err := l.tmux.SendPlanWithReadyMarker(ctx, tmuxName, prompt, marker); err != nil {
			return fmt.Errorf("send plan to tmux session %q: %w", tmuxName, err)
		}
		return nil
	}
	if input.Command != "" {
		if err := l.tmux.PrefillLineWithReadyMarker(ctx, tmuxName, prompt, marker); err != nil {
			return fmt.Errorf("prefill command to tmux session %q: %w", tmuxName, err)
		}
		return nil
	}
	if err := l.tmux.PrefillPlanWithReadyMarker(ctx, tmuxName, prompt, marker); err != nil {
		return fmt.Errorf("prefill plan to tmux session %q: %w", tmuxName, err)
	}
	return nil
}

func (l *Lifecycle) configureFinalizeHookForTmuxChat(ctx context.Context, client agent.AgentRunnerClient, workDir, sessionID, agentSessionID string, hookOpts HookOpts) (bool, error) {
	if hookOpts.Token == "" {
		return true, nil
	}
	if l.hookPort == 0 {
		return false, grpcstatus.Errorf(codes.FailedPrecondition,
			"hook port not configured: SetHookPort must be called before StartTmuxChat with a hook token")
	}
	resp, err := client.ConfigureFinalizeHook(ctx, &bossanovav1.ConfigureFinalizeHookRequest{
		WorkDir:        workDir,
		SessionId:      sessionID,
		AgentSessionId: agentSessionID,
		HookToken:      hookOpts.Token,
		HookPort:       clampInt32(l.hookPort),
	})
	if err != nil {
		return false, fmt.Errorf("configure finalize hook for run %s: %w", agentSessionID, err)
	}
	return resp.GetIsSupported(), nil
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

// recordFailedStartChat ensures an agent_chats row exists for a StartTmuxChat
// spawn whose tmux launch failed BEFORE the normal Step-7 row creation, and
// stamps its StartError so the failure surfaces as a "(failed to start)" chat
// entry (boss show / the TUI / external clients) instead of only being logged.
// A resume reuses an existing row (still present at the failure point), so we
// stamp that in place rather than inserting a duplicate; a fresh launch has no
// row yet, so we create one first via the same store method the happy path
// uses. Best-effort: store errors are logged, never returned — the caller
// already carries the primary tmux error.
func (l *Lifecycle) recordFailedStartChat(ctx context.Context, sessionID, agentSessionID, agentName, title, reason string) {
	existing, err := l.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	switch {
	case err == nil && existing != nil:
		// Row already exists (resume path); stamp it in place below.
	case errors.Is(err, db.ErrAgentChatNotFound):
		if _, createErr := l.agentChats.Create(ctx, db.CreateAgentChatParams{
			SessionID:      sessionID,
			AgentSessionID: agentSessionID,
			AgentName:      agentName,
			Title:          title,
		}); createErr != nil {
			l.logger.Warn().Err(createErr).
				Str("session", sessionID).
				Str("agentSessionID", agentSessionID).
				Msg("record failed-start chat: create row failed; tmux launch failure will be invisible")
			return
		}
	default:
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Msg("record failed-start chat: lookup failed; tmux launch failure will be invisible")
		return
	}
	if markErr := l.agentChats.MarkStartFailed(ctx, agentSessionID, reason); markErr != nil {
		l.logger.Warn().Err(markErr).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Str("reason", reason).
			Msg("record failed-start chat: mark start-failed failed; row may not show the failure reason")
	}
}

// findLiveTmuxChat scans the session's existing agent_chats rows for an
// entry whose tmux_session_name is still alive. It returns:
//
//   - (chat, true, nil) when a live tmux chat is found.
//   - (nil, false, nil) when no row matches OR the rows that exist have a
//     stale tmux name (those rows have their tmux_session_name cleared
//     as a side effect so they no longer count toward idempotency, but
//     the rows themselves are preserved as historical chat-list
//     entries — eg. completed repair runs the operator should still be
//     able to see).
//   - (nil, false, err) when ListBySession itself fails.
//
// Best-effort: if clearing a stale row's tmux_session_name fails, we log
// and continue — the caller will create a new agent_chats row anyway,
// and the next sweep can retry the unlink.
func (l *Lifecycle) findLiveTmuxChat(ctx context.Context, sessionID string) (*models.AgentChat, bool, error) {
	chats, err := l.agentChats.ListBySession(ctx, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("list chats for session %s: %w", sessionID, err)
	}
	for _, chat := range chats {
		if chat.TmuxSessionName == nil || *chat.TmuxSessionName == "" {
			continue
		}
		if l.tmux.HasSession(ctx, *chat.TmuxSessionName) {
			return chat, true, nil
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
	return nil, false, nil
}

// ReclaimRepairChat kills the tmux pane backing a repair chat and marks its
// row start-failed so the next repair sweep can respawn. It enforces the
// arg/session/title guards and verifies tmux liveness before killing, but no
// longer decides displaceability itself: whether the chat is idle enough to
// reclaim is evaluated by the host layer from the chat tracker (BOS-153); the
// old pipe-pane log-mtime staleness gate is gone. When tmux reports the pane
// alive the caller has already judged it displaceable, so we kill.
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
			return ReclaimRepairChatResult{}, fmt.Errorf("%w: cannot verify tmux session %s liveness: %w", ErrRepairChatActive, result.TmuxSessionName, tmuxErr)
		}
		if tmuxAlive {
			l.logger.Info().
				Str("session", sessionID).
				Str("agentSessionID", agentSessionID).
				Str("tmuxSession", result.TmuxSessionName).
				Str("reason", reason).
				Msg("reclaiming repair chat: killing live tmux pane")
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

// ReplaceBlockingChatForRepair kills whatever chat pane is blocking a repair
// launch (repair-titled or any other, e.g. a finalize chat) and marks its row
// start-failed. It enforces the arg/session guards and verifies tmux liveness
// before killing, but no longer decides displaceability itself: whether the
// blocking chat is idle enough to displace is evaluated by the host layer from
// the chat tracker (BOS-153); the old pipe-pane log-mtime staleness gate is
// gone. When tmux reports the pane alive the caller has already judged it
// displaceable, so we kill.
func (l *Lifecycle) ReplaceBlockingChatForRepair(ctx context.Context, sessionID, agentSessionID, reason string) (ReclaimRepairChatResult, error) {
	if sessionID == "" {
		return ReclaimRepairChatResult{}, fmt.Errorf("session_id is required")
	}
	if agentSessionID == "" {
		return ReclaimRepairChatResult{}, fmt.Errorf("agent_session_id is required")
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

	result := ReclaimRepairChatResult{Reclaimed: true}
	if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		result.TmuxSessionName = *chat.TmuxSessionName
		if l.tmux == nil {
			return ReclaimRepairChatResult{}, fmt.Errorf("%w: tmux client not configured; cannot verify blocking chat %s is dead", ErrRepairChatActive, result.TmuxSessionName)
		}
		tmuxAlive, tmuxErr := l.tmux.HasSessionStatus(ctx, result.TmuxSessionName)
		if tmuxErr != nil {
			return ReclaimRepairChatResult{}, fmt.Errorf("%w: cannot verify tmux session %s liveness: %w", ErrRepairChatActive, result.TmuxSessionName, tmuxErr)
		}
		if tmuxAlive {
			l.logger.Info().
				Str("session", sessionID).
				Str("agentSessionID", agentSessionID).
				Str("tmuxSession", result.TmuxSessionName).
				Str("reason", reason).
				Msg("replacing idle blocking chat for repair")
			if err := l.tmux.KillSession(ctx, result.TmuxSessionName); err != nil {
				return ReclaimRepairChatResult{}, fmt.Errorf("kill blocking tmux session %s: %w", result.TmuxSessionName, err)
			}
		}
	}
	if err := l.agentChats.MarkStartFailed(ctx, agentSessionID, reason); err != nil {
		return ReclaimRepairChatResult{}, err
	}
	return result, nil
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

// startTmuxChat is a thin wrapper around StartTmuxChat that supplies the
// tmux-hosted prompt (the session plan) and a title. A cron session keeps its
// `Run "<cron name>"` title; a tmux_unattended session (e.g. /boss-epic) uses the
// plain session title. All actual lifecycle work — tmux spawn, agent_chats row,
// idempotency, cleanup — lives in StartTmuxChat.
func (l *Lifecycle) startTmuxChat(
	ctx context.Context,
	sessionID string,
	opts StartSessionOpts,
	session *models.Session,
	_ *gitpkg.CreateResult,
) (string, error) {
	title := session.Title
	if opts.CronJobID != "" {
		title = `Run "` + session.Title + `"`
	}
	// Select delivery mechanics (command vs paste) from the plan, then set the
	// delivery intent from provenance: an unattended session has no human to
	// press Enter, so its payload must be submitted and verified. The intent is
	// derived from provenance, never guessed from the plan's content.
	//
	// opts is the authoritative provenance signal here: this is the same
	// cron/tmux_unattended routing that steered StartSession into startTmuxChat,
	// and it is reliable even though the caller's `session` snapshot is read
	// before StartSession stamps CronJobID/IsTmuxUnattended onto the row (so
	// isUnattendedSession(session) can still be false on this path). The session
	// predicate is kept as a fallback for any future caller that funnels a
	// fully-populated session through with empty opts.
	input := chatInputMechanicsFromPrompt(session.Plan)
	if opts.CronJobID != "" || opts.IsTmuxUnattended || opts.Detach || isUnattendedSession(session) {
		input.Delivery = DeliverySubmit
	}
	// Unattended/cron sessions wire their session-keyed Stop hook earlier in
	// StartSession (opts.HookToken); pass an empty HookOpts so StartTmuxChat
	// doesn't install a duplicate run-keyed entry.
	return l.StartTmuxChat(ctx, sessionID, input, title, HookOpts{
		HeadlessCapabilityProfile: opts.HeadlessCapabilityProfile,
	})
}

// cronAutonomyDirective is appended to the agent's system prompt for every
// chat in a cron-spawned session. It tells the agent it is running unattended
// so it stops asking questions a human will never answer — the failure mode
// that previously stranded cron runs at a "? question" prompt. bossd still
// opens and tags the PR after the run stops, so the agent only needs to
// complete its task and commit.
const cronAutonomyDirective = "You are running as an autonomous, scheduled (cron) job. " +
	"The environment variable BOSS_CRON=true is set. No human is watching this run and " +
	"no one will ever answer a question, approve a plan, or confirm an action. " +
	"Make every decision yourself: if you would normally pause to ask, pick the most " +
	"reasonable option and proceed. Never block waiting for input. " +
	"Complete your task and commit your work — the system opens and tags the pull request " +
	"for you after you stop."

const zeroOutputCronAutonomyDirective = "You are running as an autonomous, scheduled zero-output cron job. " +
	"There is no worktree and no pull request for this run. Do not commit or modify repository files. " +
	"No human is watching, so make decisions yourself and never wait for input."

// isCronSession reports whether the session was spawned by the cron scheduler.
func isCronSession(sess *models.Session) bool {
	return sess != nil && sess.CronJobID != nil && *sess.CronJobID != ""
}

// isUnattendedSession reports whether a session runs autonomously with no human
// in the loop — a scheduled cron job, a tmux_unattended session (e.g. /boss-epic),
// OR a durable tmux-hosted `boss new --detach` run (Detach). Cron-job scheduler
// bookkeeping stays keyed on CronJobID; this predicate governs autonomy: env
// (BOSS_CRON), the autonomy directive, headless-finalize exclusion, completion-gate
// eligibility, and the restart orphan-sweep exclusion. Detach joins the class so a
// tmux-hosted detach run's finalize Stop hook is admitted by the completion gate
// and it is never swept to Orphaned; a fallback (paneless) detach run leaves Detach
// false and stays in the headless class.
func isUnattendedSession(sess *models.Session) bool {
	return isCronSession(sess) || (sess != nil && (sess.IsTmuxUnattended || sess.Detach))
}

// ManagedSessionEnv returns the canonical BOSS_* environment set on every
// managed chat's tmux session. The values let the agent (and any skill or
// future agent runner — including Codex, which never sees the system prompt)
// discover its boss context. Unattended sessions (cron OR tmux_unattended)
// additionally get BOSS_CRON=true so shell-mode detection stays consistent with
// the autonomy directive that AppendSystemPromptFor appends; only real cron jobs
// also get BOSS_CRON_JOB_ID/BOSS_CRON_NAME. Binary paths are omitted when not
// resolved.
//
// agentName is the running agent for this chat. It takes precedence over
// sess.AgentName, so a codex chat spawned from a claude session correctly
// reports BOSS_AGENT=codex. Pass "" to fall back to sess.AgentName.
func ManagedSessionEnv(sess *models.Session, agentSessionID, agentName string) map[string]string {
	return managedSessionEnv(ResolveSessionFacts(sess, agentSessionID, agentName))
}

func managedSessionEnv(f SessionFacts) map[string]string {
	env := map[string]string{
		"BOSS_SESSION_ID":       f.SessionID,
		"BOSS_AGENT_SESSION_ID": f.AgentSessionID,
		"BOSS_REPO_ID":          f.RepoID,
		"BOSS_AGENT":            f.Agent,
		"BOSS_WORKTREE":         f.Worktree,
		"BOSS_SETTINGS_PATH":    f.SettingsPath,
		"BOSS_SOCKET":           f.Socket,
	}
	if f.BossBin != "" {
		env["BOSS_BIN"] = f.BossBin
	}
	if f.McpBin != "" {
		env["BOSS_MCP_BIN"] = f.McpBin
	}
	if f.IsUnattended {
		env["BOSS_CRON"] = "true"
	}
	if f.IsCron {
		env["BOSS_CRON_JOB_ID"] = f.CronJobID
		env["BOSS_CRON_NAME"] = f.CronName
	}
	return env
}

// SessionFacts is the single resolved view of a managed chat's environment,
// shared by ManagedSessionEnv (env vars) and bossSessionContext (system
// prompt) so the two can never disagree. All filesystem/config resolution
// happens here; the builders that consume it are pure.
type SessionFacts struct {
	SessionID      string
	AgentSessionID string
	RepoID         string
	Agent          string
	Worktree       string
	SettingsPath   string
	Socket         string
	BossBin        string // "" when neither a trusted boss binary nor a repo-local <worktree>/bin/boss resolved
	McpBin         string // "" when no trusted mcp binary was resolved
	IsCron         bool
	IsQuickChat    bool
	IsUnattended   bool
	CronJobID      string
	CronName       string
}

// ResolveSessionFacts gathers identifiers, config paths, and binary paths for a
// managed chat exactly once per spawn. McpBin is resolved trusted-only; BossBin
// prefers a trusted binary and, only when none resolves, falls back to the
// session's own repo-local <worktree>/bin/boss (BOS-230 — see resolveRepoLocalBoss
// for why that fallback is safe). Missing/unresolvable values are left as the
// zero string; callers degrade gracefully rather than emitting blanks.
//
// agentName is the running agent for this specific chat. It wins over
// sess.AgentName when non-empty, so a codex chat inside a claude session
// correctly reflects BOSS_AGENT=codex in both the env and system prompt.
// Pass "" to fall back to sess.AgentName (never emits a worse value than before).
func ResolveSessionFacts(sess *models.Session, agentSessionID, agentName string) SessionFacts {
	f := SessionFacts{AgentSessionID: agentSessionID}
	if sess != nil {
		f.SessionID = sess.ID
		f.RepoID = sess.RepoID
		f.Worktree = sess.WorktreePath
		f.IsQuickChat = sess.IsQuickChat
		f.IsUnattended = isUnattendedSession(sess)
		if isCronSession(sess) {
			f.IsCron = true
			f.CronJobID = *sess.CronJobID
			f.CronName = sess.Title
		}
	}
	f.Agent = agentName
	if f.Agent == "" && sess != nil {
		f.Agent = sess.AgentName
	}
	if p, err := config.Path(); err == nil {
		f.SettingsPath = p
	}
	if s, err := config.Load(); err == nil {
		if sock, ok, err := config.ConfiguredSocketPath(s); err == nil && ok {
			f.Socket = sock
		}
	}
	if f.Socket == "" {
		if dir, err := config.DefaultAppDataDir(); err == nil {
			f.Socket = filepath.Join(dir, "bossd.sock")
		}
	}
	f.BossBin = config.ResolveTrustedExecutable("boss")
	if f.BossBin == "" {
		// BOS-230: last-resort fallback to the session's own repo build. When no
		// trusted `boss` resolves (not in bossd's dir, not on PATH), the session
		// still has a usable CLI if its worktree carries a built bin/boss.
		f.BossBin = resolveRepoLocalBoss(f.Worktree)
	}
	f.McpBin = config.ResolveMcpBinary()
	return f
}

// resolveRepoLocalBoss returns the session's own build of the boss CLI at
// <worktree>/bin/boss when it exists and is an executable regular file, else "".
// It is the last-resort fallback after config.ResolveTrustedExecutable("boss")
// fails (BOS-230): a binary built from the very tree the session works in is at
// least as trustworthy as a PATH lookup, and it talks to the daemon over the
// already-authenticated socket with the session's own permissions.
//
// A session worktree is agent-writable, so this deliberately does NOT apply the
// trusted-path check ResolveTrustedExecutable uses — that check is designed to
// reject worktree binaries, which is exactly what we want to allow here. The
// daemon socket enforces the session's own permissions regardless of which
// binary invokes it. The path comes from the session's worktree record, never
// the current working directory.
func resolveRepoLocalBoss(worktree string) string {
	if worktree == "" {
		return ""
	}
	candidate := filepath.Join(worktree, "bin", "boss")
	info, err := os.Stat(candidate)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return ""
	}
	return candidate
}

// bossSessionContext describes the chat's own bossanova identifiers and how to
// act on them, built purely from resolved facts. It enumerates NO tools (the
// previous hand-listed subset caused agents to wrongly conclude a capability
// was absent). It points the agent at `boss env` — the self-describing context
// + CLI/MCP capability inventory command — with `boss --help` as a fallback,
// and hard-forbids working around a missing capability by editing boss state
// directly.
func bossSessionContext(f SessionFacts) string {
	var bossRef, helpRef, renameRef string
	if f.BossBin != "" {
		bossRef = "the boss CLI at " + f.BossBin
		helpRef = "`" + f.BossBin + " env` (or `" + f.BossBin + " --help`)"
		renameRef = "`" + f.BossBin + " rename " + f.SessionID + " <new title>`"
	} else {
		// BOS-230: no boss CLI resolved (not trusted, no repo build). Tell the
		// truth and point at the build recipe instead of advertising `boss` as
		// if it were on PATH — the honest half of the fix.
		bossRef = "the boss CLI (not available in this environment — build it with `make build`, then use `./bin/boss`)"
		helpRef = "`./bin/boss env` (or `./bin/boss --help`)"
		renameRef = "`./bin/boss rename " + f.SessionID + " <new title>`"
	}
	// Advertise exactly the identifier vars managedSessionEnv exports, so the
	// prompt never names an unset var (BOS-230). BOSS_BIN and BOSS_MCP_BIN are
	// listed only when their binary resolved — the same condition managedSessionEnv
	// gates their export on. This list is a parallel copy of that gating, kept
	// honest by TestBossSessionContext_AdvertisesExactlyExportedIdentifiers, which
	// fails if a future export drifts from the advertised set.
	idVars := []string{
		"BOSS_SESSION_ID", "BOSS_AGENT_SESSION_ID", "BOSS_REPO_ID", "BOSS_AGENT",
		"BOSS_WORKTREE", "BOSS_SETTINGS_PATH", "BOSS_SOCKET",
	}
	if f.BossBin != "" {
		idVars = append(idVars, "BOSS_BIN")
	}
	if f.McpBin != "" {
		idVars = append(idVars, "BOSS_MCP_BIN")
	}
	prompt := "You are running inside a bossanova-managed chat. Your boss identifiers are: " +
		"session ID " + f.SessionID + ", agent-session (chat) ID " + f.AgentSessionID + ", " +
		"repository ID " + f.RepoID + ", agent " + f.Agent + ", worktree " + f.Worktree + ". " +
		"These, plus the daemon socket and binary paths, are also in your environment as " +
		strings.Join(idVars, ", ") + " (context for you, not an " +
		"action surface). To act on this session or chat, use " + bossRef + " — run " + helpRef +
		" for your live session context plus the full, current CLI command and MCP tool inventory " +
		"(for example, " + renameRef + " retitles a session). " +
		"Boss operations are keyed on the session ID and agent-session (chat) ID above. " +
		"Do not assume a capability is missing without checking " + helpRef + " first. " +
		"Never edit the boss database or session files directly, and never invent a workaround " +
		"for a missing capability. If no documented boss command covers what you need, stop and " +
		"report that you are blocked."
	if f.McpBin != "" {
		// The boss MCP server is wired into this chat (see BOS-95). Announce the
		// namespace, not a tool subset: hand-listing individual tools is the
		// drift bug this prompt deliberately avoids, so point at the live tool
		// list instead of enumerating it.
		prompt += " The boss MCP server is also wired into this chat: its mcp__boss__* tools " +
			"are available to you in-session as another way to run boss operations. Discover " +
			"the current set from your own tool list rather than assuming which mcp__boss__* " +
			"tools exist."
	}
	if warning := installedSkillDriftWarning(f); warning != "" {
		prompt += " " + warning
	}
	return prompt
}

// installedSkillDriftWarning returns the source-drift warning for a session's
// worktree. Every probe is best-effort: skill freshness must not block chat
// launch in consuming repositories or on a broken local filesystem.
func installedSkillDriftWarning(f SessionFacts) string {
	if f.Worktree == "" {
		return ""
	}
	srcRoot, found := libskillinstall.FindSourceRoot(f.Worktree)
	if !found {
		return ""
	}
	agent := libskillinstall.AgentClaude
	if f.Agent == string(libskillinstall.AgentCodex) {
		agent = libskillinstall.AgentCodex
	}
	dir, err := libskillinstall.DirForAgent(agent)
	if err != nil {
		return ""
	}
	drift, err := libskillinstall.SourceDrift(dir, srcRoot)
	if err != nil || !drift {
		return ""
	}
	return "Warning: the boss skill files installed at " + dir + " do not match this repository's skill sources at " + filepath.Join(srcRoot, "skills") + " — the installed skill bodies may be out of date. Prefer the repo copies under services/boss/internal/skillinstall/skills/ when a skill body and the repo disagree, and report the drift."
}

// AppendSystemPromptFor builds the per-chat system-prompt suffix bossd injects
// into every agent launch: the boss session context for every chat, plus the
// autonomy directive for unattended sessions (cron OR tmux_unattended).
// Resolution of facts is shared with ManagedSessionEnv via ResolveSessionFacts.
//
// agentName is the running agent for this chat (see ResolveSessionFacts).
//
// mcpConfigPath is the per-spawn boss MCP config path actually written for this
// launch ("" when none). The mcp__boss__* mention is gated on it — not merely on
// a resolvable mcp binary — so the prompt never claims tools the agent will not
// receive: claude only gets --mcp-config when this path is non-empty, and a
// failed config write yields "" even when McpBin resolves.
func AppendSystemPromptFor(sess *models.Session, agentSessionID, agentName, mcpConfigPath string) string {
	if sess == nil {
		return ""
	}
	f := ResolveSessionFacts(sess, agentSessionID, agentName)
	if mcpConfigPath == "" {
		// No config was written for this spawn; do not advertise mcp__boss__*.
		f.McpBin = ""
	}
	prompt := bossSessionContext(f)
	if f.IsUnattended {
		if f.IsCron && f.IsQuickChat {
			prompt += "\n\n" + zeroOutputCronAutonomyDirective
		} else {
			prompt += "\n\n" + cronAutonomyDirective
		}
	}
	return prompt
}

// writeSessionMcpConfig generates the per-spawn boss MCP config for this chat
// and returns its absolute path (in the app-data dir, never the worktree), or
// "" when no trusted mcp binary is resolved. MCP wiring is best-effort: a write
// failure must never block a chat launch, so it is logged and the chat
// continues without mcp__boss__* rather than failing.
func (l *Lifecycle) writeSessionMcpConfig(sess *models.Session, agentSessionID, sessionID string) string {
	path, err := WriteSessionMcpConfig(ResolveSessionFacts(sess, agentSessionID, sess.AgentName))
	if err != nil {
		l.logger.Warn().Err(err).
			Str("session", sessionID).
			Str("agentSessionID", agentSessionID).
			Msg("write session mcp config failed; chat continues without mcp__boss__*")
		return ""
	}
	return path
}

// SessionMcpConfigPath writes the per-spawn boss MCP config for this chat and
// returns its absolute path (in the app-data dir, never the worktree), or ""
// when no trusted mcp binary is resolved OR the write failed. MCP wiring is
// best-effort and must never block a launch, so a write error degrades to "".
// This mirrors AppendSystemPromptFor as a pure helper for spawn call sites that
// have no Lifecycle logger (RecordChat/WakeChat); the Lifecycle path uses
// writeSessionMcpConfig instead so it can log the failure.
func SessionMcpConfigPath(sess *models.Session, agentSessionID, agentName string) string {
	if sess == nil {
		return ""
	}
	path, _ := WriteSessionMcpConfig(ResolveSessionFacts(sess, agentSessionID, agentName))
	return path
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
