package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/chatdelivery"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossd/internal/adapter/tmux"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
)

// unconfirmedResendGuidance is the one action an UNCONFIRMED delivery calls for.
// It is appended to notice_text rather than left to the surfaces that render
// delivery_state, because notice_text is the only field that survives every
// converter between here and a caller.
//
// It comes from bossalib rather than being spelled out here because the CLI
// MATCHES on this sentence to recognise an unconfirmed delivery whose
// delivery_state the proxy dropped: reworded here alone, that recognition fails
// silently. Shared, the two cannot drift.
const unconfirmedResendGuidance = chatdelivery.ResendGuidance

// queuedGuidance is the counterpart for a QUEUED delivery, and it says the
// opposite: the agent has the message, so resending queues a second copy that
// runs twice. It comes from the same package for the same reason — surfaces
// match on the sentence to recover a delivery_state the proxy dropped.
const queuedGuidance = chatdelivery.QueuedGuidance

const turnStartNoResendGuidance = "turn start observation does not change delivery; do not resend based on this verdict"

// promptReceiptBook is the part of session.Lifecycle that confirms a send by
// the agent's own prompt-submitted receipt: Claude's UserPromptSubmit hook,
// which the claude plugin installs for every tmux-hosted chat and which posts
// to /hooks/prompt-submitted/{id} (BOS-1505). A receipt is recorded only while
// a slot for that id is open.
type promptReceiptBook interface {
	ExpectPromptSubmitted(key string) (release func())
	PromptSubmittedSince(key string, since time.Time) bool
}

// expectPromptReceipt opens a receipt slot for one submitted Claude send and
// returns the check the verifier consults (nil when the send has no receipt to
// wait for) and the slot's release.
//
// Only Claude chats get one: Codex has no prompt hook, so its sends are
// confirmed from its rollout instead (expectRolloutReceipt). The slot is keyed
// by the id the chat's
// runner was launched with — its provider session id when one is bound, else
// bossd's agent_session_id — because that is the id the hook URL carries.
//
// A receipt says that A prompt was submitted in that chat at or after the send
// began, not that it was THIS payload. Within a send's few seconds the only
// other source is a human typing into the same pane, which is accepted.
func (s *Server) expectPromptReceipt(chat *models.AgentChat, willSubmit bool) (submitted func() bool, release func()) {
	if s.promptReceipts == nil || !willSubmit || !isClaudeChat(chat.AgentName) {
		return nil, func() {}
	}
	key := chat.AgentSessionID
	if chat.ProviderSessionID != nil && *chat.ProviderSessionID != "" {
		key = *chat.ProviderSessionID
	}
	book := s.promptReceipts
	release = book.ExpectPromptSubmitted(key)
	since := time.Now()
	return func() bool { return book.PromptSubmittedSince(key, since) }, release
}

// rolloutReceiptReadTimeout bounds each rollout read, the probe before the send
// and every read inside the submit verifier, so a stalled plugin cannot hold the
// send past its budget. The read parses the whole rollout, which a long chat
// makes large.
const rolloutReceiptReadTimeout = time.Second

// expectRolloutReceipt returns the check that confirms a submitted Codex send
// from the agent's own rollout: a user turn recorded at or after the send began,
// the same receipt that confirms a launch's first input. The rollout records
// every turn Codex takes, so the verifier treats its absence as evidence too
// (tmux.SendHooks.ReceiptRequired).
//
// The rollout is looked up by the chat's provider session id when one is bound
// and by its agent_session_id otherwise. A codex chat whose rollout is keyed by
// its agent_session_id never gets a provider id bound — the launch receipt binds
// one only when the ids differ (session.awaitDeliveryReceipt) — so reading the
// provider id alone left such a chat on the pane verifier for its whole life.
//
// It returns nil, leaving the pane verifier as the only signal, when there is no
// rollout to read: no codex runner, or no candidate id whose rollout can be read
// within rolloutReceiptReadTimeout. That fallback is logged at warn, since the
// pane alone cannot see a burst Codex holds undrawn. A slash command (a Codex
// built-in such as /status) starts no turn, so it gets no receipt and no warning.
func (s *Server) expectRolloutReceipt(ctx context.Context, chat *models.AgentChat, workDir, message string, willSubmit bool) func() bool {
	if !willSubmit || chat.AgentName != "codex" {
		return nil
	}
	if strings.HasPrefix(strings.TrimSpace(message), "/") {
		return nil
	}
	client, ok := s.agentClients[chat.AgentName]
	if !ok || client == nil {
		s.warnPaneOnlyCodexSubmit(chat, "no codex agent runner is loaded", nil)
		return nil
	}
	since := time.Now()
	rolloutID, err := findCodexRollout(ctx, client, workDir, chat)
	if rolloutID == "" {
		s.warnPaneOnlyCodexSubmit(chat, "no readable rollout for the chat's provider session id or agent_session_id", err)
		return nil
	}
	return func() bool {
		readCtx, cancel := context.WithTimeout(ctx, rolloutReceiptReadTimeout)
		defer cancel()
		return session.TranscriptHasUserMessageSince(readCtx, client, workDir, rolloutID, since)
	}
}

// findCodexRollout returns the first of the chat's candidate rollout ids
// (provider session id, then agent_session_id) whose rollout the runner can
// read, or "" with the last read error. All probes share one
// rolloutReceiptReadTimeout, so a second candidate cannot lengthen the send.
func findCodexRollout(ctx context.Context, client agent.RunnerClient, workDir string, chat *models.AgentChat) (string, error) {
	var candidates []string
	if chat.ProviderSessionID != nil && *chat.ProviderSessionID != "" {
		candidates = append(candidates, *chat.ProviderSessionID)
	}
	if chat.AgentSessionID != "" && !slices.Contains(candidates, chat.AgentSessionID) {
		candidates = append(candidates, chat.AgentSessionID)
	}
	probeCtx, cancel := context.WithTimeout(ctx, rolloutReceiptReadTimeout)
	defer cancel()
	var lastErr error
	for _, id := range candidates {
		probe, err := client.ReadTranscript(probeCtx, &pb.ReadTranscriptRequest{WorkDir: workDir, AgentSessionId: id, MaxMessages: 1})
		if err != nil {
			lastErr = err
			continue
		}
		if probe.GetExists() {
			return id, nil
		}
	}
	return "", lastErr
}

// warnPaneOnlyCodexSubmit records a codex submit that falls back to the pane
// verifier with no rollout receipt. The pane cannot see a burst Codex holds
// undrawn, so a send settled this way can read submitted without having run.
func (s *Server) warnPaneOnlyCodexSubmit(chat *models.AgentChat, reason string, err error) {
	event := s.logger.Warn().
		Str("chat_id", chat.ID).
		Str("agent_session_id", chat.AgentSessionID).
		Str("session_id", chat.SessionID).
		Bool("has_provider_session_id", chat.ProviderSessionID != nil && *chat.ProviderSessionID != "").
		Str("reason", reason)
	if err != nil {
		event = event.Err(err)
	}
	event.Msg("codex submit has no rollout receipt; verifying from the pane alone")
}

// deliveryConfirmation names the signal that settled a send, for the delivery
// log line.
func deliveryConfirmation(sendErr error, willSubmit, receiptSeen bool) string {
	if receiptSeen {
		return "receipt"
	}
	if sendErr == nil {
		if willSubmit {
			return "pane"
		}
		return "none"
	}
	switch tmux.OutcomeOf(sendErr) {
	case tmux.OutcomeQueued, tmux.OutcomeNotSubmitted, tmux.OutcomeBlockedByModal, tmux.OutcomeComposerOccupied:
		return "pane"
	default:
		return "none"
	}
}

// bindCodexRolloutAfterSend starts provider session discovery for a codex chat
// that is still unbound once a message reaches its pane. Codex creates its
// rollout on a thread's first turn, not at launch, so a chat opened with
// `boss chat new` and prompted later outlives the launch-time discovery window
// and stays unbound until its next wake. sentAt as the launch floor confines
// binding to the pane's own open rollout: the time-window fallback rejects every
// thread started before the send, this one included, so it can never bind a
// sibling's older rollout in the same worktree (BOS-290).
func (s *Server) bindCodexRolloutAfterSend(chat *models.AgentChat, worktreePath, tmuxName string, sentAt time.Time, tmuxer tmuxSpawner) {
	if chat.AgentName != "codex" || (chat.ProviderSessionID != nil && *chat.ProviderSessionID != "") {
		return
	}
	s.discoverProviderSessionIDInBackground(chat, worktreePath, tmuxName, sentAt, s.chatSpawnDeps().Resolver, tmuxer)
}

// isClaudeChat reports whether a chat runs Claude; an empty agent name is the
// legacy Claude default, as chatReadyMarker resolves it.
func isClaudeChat(agentName string) bool {
	return agentName == "" || agentName == "claude"
}

// receiptOverridesOutcome reports whether a receipt turns a failed send into a
// delivered one. Only the two verifier outcomes that leave the payload in the
// pane qualify: a modal refusal, an occupied-composer refusal or a readiness
// timeout typed nothing, so a receipt then cannot be this send's, and a queued
// send is already delivered.
func receiptOverridesOutcome(err error, submitted func() bool) bool {
	switch tmux.OutcomeOf(err) {
	case tmux.OutcomeNotSubmitted, tmux.OutcomeUnconfirmed:
		return submitted != nil && submitted()
	default:
		return false
	}
}

// modalDetectorFor returns the "is this pane showing a menu rather than a
// composer?" check for one agent, or nil when it cannot be answered.
//
// The check itself lives in internal/agent (agent.NewModalPaneChecker), which
// is where its contract is documented: why it reads blocks_input rather than
// has_prompt, why an unreachable plugin fails open, and why the grammar stays
// in the plugins. Session start needs the same check over the same panes
// (BOS-894), so keeping the body here would have meant two copies of it.
//
// What is left here is the only part that is this server's business: which
// client answers for this agent. A missing entry returns nil, which disables
// the check — deliberate fail-open, mirroring the poller.
//
// It costs at most one plugin round-trip per readiness wait — the gate probes
// only the capture that resolved a composer row, or the last capture on the
// deadline path — not one per poll tick.
func (s *Server) modalDetectorFor(agentName string) tmux.ModalDetector {
	client, ok := s.agentClients[agentName]
	if !ok {
		return nil
	}
	return agent.NewModalPaneChecker(client, agentName, s.logger)
}

// SendChatMessage delivers a user message into a chat's live agent, optionally
// waking an asleep session first.
//
// BOS-598 moved one failure off the error channel: an UNCONFIRMED submit (the
// verifier could not read the pane, so the payload may or may not be running)
// used to be a CodeInternal error and is now a SUCCESSFUL response carrying
// delivered=false, delivery_state=UNCONFIRMED and a human-readable notice_text.
// Every caller was audited for that move, because a caller that reads only err
// would silently turn a failure into a success:
//
//   - services/boss/internal/client/local.go, services/mcp/internal/
//     socketbackend, services/bossd/internal/upstream/adapters.go — pass-through
//     wrappers returning the whole resp.Msg, so both new signals survive intact.
//   - services/boss/internal/client/remote.go — the ONE caller that rebuilds the
//     response field by field (from ProxySendChatMessageResponse), so it is where
//     a new signal can be dropped silently. It carries notice_text; it cannot
//     carry delivery_state, because the proxy response has no mirror of it (see
//     the deferral below). A cloud caller therefore sees delivered=false plus the
//     notice rather than a distinct UNCONFIRMED state.
//   - services/bossd/internal/upstream/command_dispatcher.go — forwards the whole
//     message as CommandResult{send_chat_message} with ok=true; bosso's
//     ProxySendChatMessage maps it onto ProxySendChatMessageResponse, which
//     carries notice_text (the delivery_state mirror is deliberately deferred:
//     orchestrator.proto is an observable surface and would force an apiversion
//     bump).
//   - services/mcp-gateway/internal/adapter/proxybackend — remote MCP; carries
//     notice_text, which on that path is the only reason a caller can see.
//   - lib/bossalib/bossmcp/tools_mutating.go (send_chat_message) — renders the
//     whole response as JSON, so a driver sees delivered=false plus the notice.
//   - services/boss/cmd/chat.go — reportChatSendOutcome renders UNCONFIRMED
//     distinctly and tells the operator to check the pane before resending;
//     exit status stays 0 because the message may in fact be running.
//   - services/bossd/cmd/main.go deliverChatMessage — the shared primitive for
//     callback delivery, transient resume, and broadcast delivery. Undelivered
//     stays an error there, so all three keep their capped-backoff retry, and
//     the delivery state plus notice are folded into what they log.
//
// The move is visible one hop further out, on bossanova.v1: an unconfirmed
// submit used to reach ProxySendChatMessage as a failed CommandResult (a Connect
// error) and now arrives as ok=true with delivered=false. No apiversion bump
// accompanies it, deliberately. The versioning mechanism down-converts response
// VALUES (VersionChange.TransformResponse mutates a response message in place),
// and this produces no value a Baseline client was not already built for:
// delivered=false plus notice_text is the pre-existing, proto-documented shape
// for "bossd handled this and did not deliver it; read notice_text" (the
// intercepted "/boss switch" path). What widened is the set of conditions that
// produce that shape. A transform therefore has nothing to rewrite — the only
// down-convert would be to synthesize the old error for older clients, which the
// interface cannot express and which would be actively harmful: it would restore
// exactly the "it failed, retry it" reading whose retry double-types into the
// agent's composer, i.e. the bug this change exists to remove. Adding
// delivery_state to ProxySendChatMessageResponse WOULD put a new value on that
// wire and would need a bump; that is the deferral recorded above.
func (s *Server) SendChatMessage(ctx context.Context, req *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error) {
	chat, err := s.agentChats.GetByAgentSessionID(ctx, req.Msg.GetAgentSessionId())
	if err != nil || chat == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat not found: %s", req.Msg.GetAgentSessionId()))
	}

	sess, err := s.sessions.Get(ctx, chat.SessionID)
	if err != nil || sess == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session not found for chat %s", req.Msg.GetAgentSessionId()))
	}

	// Intercept a SUBMITTED single-line "/boss switch" (or "/switch") control
	// command before any tmux delivery: parse it mechanically, run the existing
	// account-switch primitive, and return the outcome in notice_text WITHOUT ever
	// waking, pasting into, or submitting to the pane — so a credit-exhausted chat
	// can switch accounts with zero LLM calls. Everything that is not a bare
	// single-line "/boss switch" (prose, multi-line input, any other command) falls
	// through to normal delivery below, unchanged.
	//
	// The interception is gated on submit: a submit=false request explicitly asks
	// to "only prefill the composer" (see BOS-242), so honoring that intent, a
	// prefilled "/boss switch" is staged into the composer verbatim rather than
	// executed — the switch fires only when the caller actually submits the command
	// (the CLI's default; an explicit submit=true for MCP/web). This keeps submit's
	// meaning consistent: it governs whether the user's directive is acted on now.
	//
	// Scope boundary: this guards only the RPC send path. Raw SSH keystrokes typed
	// directly into the tmux pane bypass this RPC entirely and still reach the agent
	// (→ 401 on an exhausted account); guarding the raw pane is deliberately out of
	// scope here and is covered by S3 (auto-rotate on 401).
	if req.Msg.GetSubmit() {
		if cmd, ok := session.ParseBossControlCommand(req.Msg.GetMessage()); ok {
			resp, err := s.handleBossSwitchInterception(ctx, chat, sess, cmd)
			if err != nil {
				return nil, err
			}
			s.applyTurnStartVerdict(req.Msg, resp.Msg, status.TurnStartUnobservable)
			return resp, nil
		}
	}

	// Prefer the tmux session name persisted on the chat row: a legacy or
	// relocated chat may be live under a name that differs from the deterministic
	// one, and checking/sending to the recomputed name would miss the running
	// session (and, on the wake path, spawn a duplicate). Fall back to the
	// deterministic name when none has been persisted yet. This mirrors the kill
	// and liveness paths (killChatTmuxSession, tmux_poller, liveness).
	tmuxName := tmux.ChatSessionName(sess.RepoID, chat.AgentSessionID)
	if chat.TmuxSessionName != nil && *chat.TmuxSessionName != "" {
		tmuxName = *chat.TmuxSessionName
	}

	// Resolve the tmux interface: prefer the test hook, fall back to the live client.
	var spawner tmuxSpawner
	if s.wakeHook.spawner != nil {
		spawner = s.wakeHook.spawner
	} else if s.tmux != nil {
		spawner = liveTmuxSpawner{c: s.tmux}
	}

	isLive := spawner != nil && spawner.HasSession(ctx, tmuxName)

	if !isLive {
		if !req.Msg.GetWakeIfAsleep() {
			return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("chat %s is not live and wake_if_asleep is false", req.Msg.GetAgentSessionId()))
		}
		_, wokenName, _, wakeErr := s.WakeChatInternal(ctx, req.Msg.GetAgentSessionId(), false)
		if wakeErr != nil {
			if errors.Is(wakeErr, ErrWorktreeMissing) || errors.Is(wakeErr, ErrHeadlessRunActive) {
				return nil, connect.NewError(connect.CodeFailedPrecondition, wakeErr)
			}
			if refusal, ok := injectionRefusalConnectError(wakeErr, "wake chat"); ok {
				return nil, refusal
			}
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("wake chat: %w", wakeErr))
		}
		// Wake spawns under (and persists) the canonical name; send there rather
		// than to a stale persisted name we may have just superseded.
		if wokenName != "" {
			tmuxName = wokenName
		}
	}

	if spawner == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("tmux not available"))
	}

	// Rewrite an installed custom skill command ("/boss-repair watch",
	// "/api-review") to the chat agent's command prefix before delivery,
	// mirroring the plan-launch render path: a raw "/boss-repair" reaches codex
	// verbatim and its CLI rejects it as unrecognized (codex custom commands use
	// "$"). The rewrite is scoped to installed skill names on the first line, so
	// a multi-line message's leading "/boss-repair N" is rewritten while a codex user's
	// native built-in ("/model", "/status") and free text pass through unchanged.
	message := session.RenderBossCommandPrefix(req.Msg.GetMessage(), chatCommandPrefix(chat.AgentName), sess.WorktreePath)

	// submit routes the delivery: true submits the message (Enter + BOS-228
	// verifier) for both single- and multi-line payloads (BOS-488); false
	// (default) prefills the composer. The verifier fails toward "still
	// pending", so a swallowed Enter surfaces as an error here rather than a
	// silent false "submitted". The ready marker is resolved from the chat's
	// agent so the submit path waits for the correct composer glyph (claude
	// "❯", codex "›").
	//
	// The modal detector is resolved from the same agent, so the readiness gate
	// can tell that agent's approval menu / question picker from its composer and
	// refuse rather than fire an Enter into it (BOS-600).
	shouldObserveTurnStart := req.Msg.GetShouldObserveTurnStart()
	willSubmit := tmux.WillSubmit(req.Msg.GetSubmit(), message)
	baseline := status.TurnStartBaseline{}
	var beforeSubmit func()
	if shouldObserveTurnStart && willSubmit {
		beforeSubmit = func() {
			baseline = status.CaptureTurnStartBaseline(s.chatStatus, chat.AgentSessionID)
		}
	}
	// A Claude send is confirmed by the agent's UserPromptSubmit receipt
	// first; the pane verifier is the fallback for Codex and for a Claude pane
	// whose hook never fires (BOS-1506). The verifier consults the receipt
	// before each pane read and before its clear-and-redeliver retry, and a
	// receipt that lands after the verifier gave up still settles the outcome
	// here.
	//
	// A Codex send is confirmed by the user turn its rollout records. That
	// receipt is required, not just preferred: Codex can hold a typed burst
	// undrawn, so an empty composer alone does not prove the send was taken.
	submitted, releaseReceipt := s.expectPromptReceipt(chat, willSubmit)
	defer releaseReceipt()
	receiptRequired := false
	if rollout := s.expectRolloutReceipt(ctx, chat, sess.WorktreePath, message, willSubmit); rollout != nil {
		submitted, receiptRequired = rollout, true
	}
	var receiptSeen atomic.Bool
	if check := submitted; check != nil {
		submitted = func() bool {
			if check() {
				receiptSeen.Store(true)
				return true
			}
			return false
		}
	}
	sentAt := time.Now()
	sendErr := spawner.SendMessage(ctx, tmuxName, message, req.Msg.GetSubmit(), chatReadyMarker(chat.AgentName), s.modalDetectorFor(chat.AgentName),
		tmux.SendHooks{BeforeSubmit: beforeSubmit, Submitted: submitted, ReceiptRequired: receiptRequired})
	if receiptOverridesOutcome(sendErr, submitted) {
		sendErr = nil
	}
	deliveryOutcome := tmux.OutcomeOf(sendErr).String()
	if sendErr == nil && !willSubmit {
		deliveryOutcome = "prefilled"
	}
	s.logger.Info().
		Str("chat_id", chat.ID).
		Str("agent_session_id", chat.AgentSessionID).
		Str("session_id", chat.SessionID).
		Str("agent", chat.AgentName).
		Str("delivery_state", deliveryOutcome).
		Str("confirmed_by", deliveryConfirmation(sendErr, willSubmit, receiptSeen.Load())).
		Bool("receipt_required", receiptRequired).
		Msg("chat message delivery")
	if willSubmit {
		switch tmux.OutcomeOf(sendErr) {
		case tmux.OutcomeSubmitted, tmux.OutcomeQueued, tmux.OutcomeUnconfirmed:
			s.bindCodexRolloutAfterSend(chat, sess.WorktreePath, tmuxName, sentAt, spawner)
		case tmux.OutcomeNotSubmitted, tmux.OutcomeBlockedByModal, tmux.OutcomeComposerOccupied, tmux.OutcomeUnclassified:
			// Nothing reached the agent, so it started no turn to write a rollout for.
		}
	}
	if err := sendErr; err != nil {
		// Three outcomes, three answers (BOS-598). Only the verifier classifies an
		// error; everything else (ready-marker timeout, a tmux command that failed
		// to run) stays OutcomeUnclassified and keeps the pre-existing CodeInternal.
		switch tmux.OutcomeOf(err) {
		case tmux.OutcomeUnconfirmed:
			// Verification could not resolve, so the payload MAY have been submitted.
			// Returning an error here reads to a caller as "it failed" and invites a
			// retry that double-types into the agent's composer. Report it as a
			// response instead, with delivered=false so every existing caller keying
			// on delivered still fails closed, and delivery_state carrying the reason
			// a caller needs to decide whether retrying is safe.
			//
			// The retry guidance lives in notice_text, not only in the surfaces that
			// read delivery_state: notice_text is the ONE field every hand-rolled
			// converter forwards (services/boss/internal/client/remote.go and
			// services/mcp-gateway/internal/adapter/proxybackend/chats.go both drop
			// delivery_state), so a caller reached over the proxy would otherwise be
			// told delivery is unconfirmed without being told that resending may
			// double-type into the composer.
			resp := &pb.SendChatMessageResponse{
				TmuxSessionName: tmuxName,
				Delivered:       false,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_UNCONFIRMED,
				// No verdict lead-in: delivery_state above already says the submit is
				// unconfirmed, and the verifier's error states the observation that
				// makes it so ("no live composer was drawn on tmux session <name>
				// …"). Prefixing a second "could not be confirmed" here would make
				// every surface state the same fact twice — the CLI, which labels the
				// line "delivery unconfirmed" itself, twice over. Carry the
				// observation and the guidance, once each.
				NoticeText: fmt.Sprintf("%v; %s", err, unconfirmedResendGuidance),
			}
			s.applyTurnStartVerdict(req.Msg, resp, status.TurnStartUnobservable)
			return connect.NewResponse(resp), nil
		case tmux.OutcomeQueued:
			// The payload left the composer into the agent's own visible message
			// queue, so it will run when the current turn ends. That IS delivery —
			// a more precise SUBMITTED rather than a softened failure — and it is
			// the outcome where a retry is actively harmful, since the agent
			// already holds the message and resending queues a second copy.
			//
			// It is a response, not an error, for the same reason UNCONFIRMED is:
			// an error reads to every caller as "it failed" and invites exactly
			// that retry. notice_text carries the guidance because it is the ONE
			// field the hand-rolled converters forward (remote.go and
			// proxybackend.go both drop delivery_state), so a proxied caller is
			// still told not to resend.
			resp := &pb.SendChatMessageResponse{
				TmuxSessionName: tmuxName,
				Delivered:       true,
				DeliveryState:   pb.SendChatMessageResponse_DELIVERY_STATE_QUEUED,
				NoticeText:      fmt.Sprintf("%v; %s", err, queuedGuidance),
			}
			s.applyTurnStartVerdict(req.Msg, resp, status.TurnStartUnobservable)
			return connect.NewResponse(resp), nil
		case tmux.OutcomeBlockedByModal:
			// The pane was showing a modal, so nothing was typed and no Enter was
			// sent. FailedPrecondition, not Internal: the daemon is healthy and the
			// caller's request was well-formed — the *pane* is in the wrong state,
			// and it takes a human (or the agent finishing its prompt) to leave that
			// state. Resending changes nothing until it does, which is exactly what
			// separates this from OutcomeNotSubmitted's safe retry. The state is
			// named in the message so the distinction survives the error-only
			// proxied surface that drops delivery_state (BOS-600).
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("send message (delivery state: %s): %w", tmux.OutcomeBlockedByModal, err))
		case tmux.OutcomeComposerOccupied:
			// Refused before typing, for the modal's reason: the pane holds a draft
			// only a human may clear, and a resend is refused again until then.
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("send message (delivery state: %s): %w", tmux.OutcomeComposerOccupied, err))
		case tmux.OutcomeNotSubmitted:
			// The payload was confirmed unsubmitted: a genuine failure, and one a
			// caller can safely retry. The send withdrew its payload from the
			// composer before returning, so the retry types into an empty box; when
			// the withdrawal could not run, the error says so and a retry is refused
			// as composer occupied until the box is cleared. Keep it loud, and name the
			// state in the message so the distinction survives an error-only surface
			// (the proxied bossanova.v1 path, which does not carry delivery_state).
			return nil, connect.NewError(connect.CodeInternal,
				fmt.Errorf("send message (delivery state: %s): %w", tmux.OutcomeNotSubmitted, err))
		default:
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("send message: %w", err))
		}
	}

	// delivery_state stays UNSPECIFIED for a prefill: nothing was submitted and
	// nothing was verified, so claiming SUBMITTED would be exactly the lie this
	// field exists to remove. It asks tmux.WillSubmit — the same predicate
	// tmux.Client.SendMessage routes on — rather than submit alone, because a
	// whitespace-only or empty body has nothing to submit and is routed to the
	// PREFILL path there (no Enter, no verification) even with submit=true. It
	// reads the rendered `message`, which is what is actually delivered.
	deliveryState := pb.SendChatMessageResponse_DELIVERY_STATE_UNSPECIFIED
	if willSubmit {
		deliveryState = pb.SendChatMessageResponse_DELIVERY_STATE_SUBMITTED
	}
	resp := &pb.SendChatMessageResponse{
		TmuxSessionName: tmuxName,
		Delivered:       true,
		DeliveryState:   deliveryState,
	}
	if shouldObserveTurnStart {
		verdict := status.TurnStartUnobservable
		if willSubmit {
			verdict = status.AwaitTurnStart(ctx, s.chatStatus, chat.AgentSessionID, baseline)
		}
		s.applyTurnStartVerdict(req.Msg, resp, verdict)
	}
	return connect.NewResponse(resp), nil
}

func (s *Server) applyTurnStartVerdict(req *pb.SendChatMessageRequest, resp *pb.SendChatMessageResponse, verdict status.TurnStartVerdict) {
	if !req.GetShouldObserveTurnStart() {
		return
	}
	switch verdict {
	case status.TurnStartObserved:
		resp.TurnStartState = pb.SendChatMessageResponse_TURN_START_STATE_OBSERVED
	case status.TurnStartNotObserved:
		resp.TurnStartState = pb.SendChatMessageResponse_TURN_START_STATE_NOT_OBSERVED
		resp.NoticeText = appendNotice(resp.NoticeText, fmt.Sprintf("turn start not observed; %s", turnStartNoResendGuidance))
	case status.TurnStartUnobservable:
		resp.TurnStartState = pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE
		resp.NoticeText = appendNotice(resp.NoticeText, fmt.Sprintf("turn start unobservable; %s", turnStartNoResendGuidance))
	default:
		resp.TurnStartState = pb.SendChatMessageResponse_TURN_START_STATE_UNOBSERVABLE
		resp.NoticeText = appendNotice(resp.NoticeText, fmt.Sprintf("turn start unobservable; %s", turnStartNoResendGuidance))
	}
}

func appendNotice(existing, extra string) string {
	if existing == "" {
		return extra
	}
	return fmt.Sprintf("%s; %s", existing, extra)
}

// handleBossSwitchInterception executes a mechanically-parsed "/boss switch"
// control command and returns the outcome as a SendChatMessageResponse whose
// Delivered is false and whose NoticeText carries the human-readable result. The
// message is never delivered to the agent, so the switch costs zero LLM calls.
//
// Account resolution mirrors the SwitchSessionAgentAccount RPC so the two paths behave
// identically:
//   - Named target (cmd.Account != ""): resolveSessionAccount does id-or-label,
//     provider-scoped, eligibility-checked resolution — an ambiguous label is
//     InvalidArgument and an ineligible (disabled/failed/cooling) named target is
//     InvalidArgument, both surfaced verbatim (identical to SwitchSessionAgentAccount).
//   - Unnamed target: the S1 util-aware resolver picks the next eligible account
//     EXCLUDING the current one. When no other account is available it returns a
//     friendly notice rather than an error or a silent no-op.
//
// The switch itself runs through executeAccountSwitch (Auto:false), so mid-turn
// refusal (→ FailedPrecondition naming --force) and the stream publish are shared.
func (s *Server) handleBossSwitchInterception(ctx context.Context, chat *models.AgentChat, sess *models.Session, cmd *session.BossControlCommand) (*connect.Response[pb.SendChatMessageResponse], error) {
	// The switch's account picker is scoped to the CHAT's provider (a claude
	// account must not bind a codex chat), falling back to the session's agent for
	// a legacy chat with no AgentName — the same resolution switchTargetAgentName
	// performs, but chat and sess are already in scope so no extra fetch is needed.
	agentName := chat.AgentName
	if agentName == "" {
		agentName = sess.AgentName
	}

	var targetAccountID string
	var err error
	if cmd.Account != "" {
		requested := cmd.Account
		targetAccountID, err = s.resolveSessionAccount(ctx, &requested, agentName)
		if err != nil {
			return nil, err
		}
	} else {
		if s.resolver == nil {
			return nil, connect.NewError(connect.CodeFailedPrecondition,
				fmt.Errorf("no account registry configured; cannot pick a switch target"))
		}
		current := derefAccountID(sess.AccountID)
		targetAccountID, err = s.resolver.DefaultAgentAccountIDExcluding(ctx, accountAgentName(agentName), current, time.Now())
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("select switch target: %w", err))
		}
		// An empty pick (or the current account echoed back) means there is no
		// other eligible account to move to. Report it as a benign notice on the
		// undelivered response, not an error — the user asked a reasonable question
		// and there is simply nowhere to go.
		if targetAccountID == "" || targetAccountID == current {
			return connect.NewResponse(&pb.SendChatMessageResponse{
				Delivered:  false,
				NoticeText: "no other account available to switch to",
			}), nil
		}
	}

	res, err := s.executeAccountSwitch(ctx, chat.SessionID, chat.AgentSessionID, targetAccountID, cmd.Force)
	if err != nil {
		return nil, err
	}

	return connect.NewResponse(&pb.SendChatMessageResponse{
		Delivered:  false,
		NoticeText: res.NoticeText,
	}), nil
}
