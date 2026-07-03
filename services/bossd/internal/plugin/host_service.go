package plugin

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/machine"
	"github.com/recurser/bossalib/vcs"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/session"
	"github.com/recurser/bossd/internal/status"
	"github.com/rs/zerolog/log"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ErrAuthMismatch is returned by CompleteAgentRun when the supplied bearer
// token doesn't match the recorded per-run hook token. The hook server
// translates this to HTTP 401.
var ErrAuthMismatch = errors.New("agent run auth mismatch")

// ErrAgentRunNotFound is returned by CompleteAgentRun when the supplied
// agent_session_id has never been registered (or has already been torn
// down by WaitChatRun). The hook server translates this to HTTP 404.
var ErrAgentRunNotFound = errors.New("agent run not found")

// waitAgentRunPollInterval is how often WaitAgentRun polls the loaded
// AgentRunner plugin's ExitStatus. Picked to balance responsiveness with
// idle CPU cost; the repair plugin's only consumer is single-shot per repair.
const waitAgentRunPollInterval = 500 * time.Millisecond

// defaultWaitChatRunDeadline caps how long WaitChatRun blocks for the Stop
// hook to fire before returning a synthetic exit_error and tearing down
// the run state. Spec §"Failure modes" sets this at 30 minutes.
const defaultWaitChatRunDeadline = 30 * time.Minute

const waitChatRunProviderIDDiscoveryTimeout = 2 * time.Second

// watchdogPollInterval is how often the WaitChatRun idle watchdog samples the
// chat tracker while a run is armed (idle_fail_after_seconds > 0). Package-level
// var (not const) so tests can shrink it to milliseconds.
var watchdogPollInterval = 15 * time.Second

// watchdogReclaimTimeout bounds the best-effort pane teardown when the idle
// watchdog fires. Independent of the caller's context so a fire late in the
// wait still gets a chance to kill the dead pane.
const watchdogReclaimTimeout = 10 * time.Second

// ChatLifecycle is the narrow surface HostServiceServer needs from
// *session.Lifecycle for StartChatRun. Defining it as an interface
// (rather than holding a *session.Lifecycle directly) keeps tests
// lightweight — they can pass a small fake instead of constructing the
// full Lifecycle struct.
//
// *session.Lifecycle satisfies this interface as-is.
type ChatLifecycle interface {
	// StartTmuxChat spawns a tmux-hosted agent run for sessionID with the
	// supplied input and chat-list title. When hookOpts.Token is
	// non-empty the lifecycle installs a run-keyed Stop-hook entry
	// pointing at the daemon hook server. Returns the resolved
	// agent_session_id on success. On AlreadyExists the returned string
	// is the existing run's agent_session_id alongside the typed gRPC
	// error.
	StartTmuxChat(ctx context.Context, sessionID string, input session.ChatInput, title string, hookOpts session.HookOpts) (string, error)
	ReclaimRepairChat(ctx context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error)
	ReplaceBlockingChatForRepair(ctx context.Context, sessionID, agentSessionID, reason string) (session.ReclaimRepairChatResult, error)
	// GetAgentChatTitle returns the chat-list title of the chat identified
	// by agentSessionID (empty string + error when the row is missing). The
	// host reads it to decide whether a blocking chat is repair-owned, which
	// selects the QUESTION displacement policy in chatDisplaceable.
	GetAgentChatTitle(ctx context.Context, agentSessionID string) (string, error)
}

// HostServiceServer implements the HostService gRPC server on the daemon
// side. Plugins call back to this server via go-plugin's GRPCBroker to
// query VCS data and session state.
type HostServiceServer struct {
	provider       vcs.Provider
	sessionStore   db.SessionStore
	agentChats     db.AgentChatStore
	repoStore      db.RepoStore
	displayTracker *status.DisplayTracker
	chatTracker    *status.Tracker

	// agentClients is the per-name registry of AgentRunnerClient gRPC
	// clients (one per loaded AgentRunner plugin, keyed by the plugin's
	// configured Name — matching session.AgentName). StartAgentRun /
	// WaitAgentRun look up the right client based on the session record's
	// AgentName so non-agent plugins (like bossd-plugin-repair) don't need
	// a direct broker channel into each agent plugin's process. An empty
	// map means no AgentRunner plugins are loaded; per-handler nil guards
	// surface that as FailedPrecondition.
	agentClients map[string]agent.AgentRunnerClient
	// agentLogsDir is the bossd-owned directory where the agent plugin
	// writes per-session NDJSON log files. Forwarded into StartRun.LogPath.
	agentLogsDir string

	// activeRuns maps session_id → activeRun for the currently-active
	// repair run on each session. Guarded by runMu. The repair plugin's
	// in-memory dedup map provides cross-call deduplication; this map is
	// the daemon's last-line defense against two concurrent StartAgentRun
	// calls landing on the same session.
	//
	// agentSessionByID is the reverse index from agent_session_id back to
	// the agent plugin name that started it — needed by WaitAgentRun, which
	// only receives the agent_session_id and must route the ExitStatus poll
	// to the right plugin client. Populated atomically with activeRuns.
	//
	// runCompletion is keyed by agent_session_id. Each value records a
	// buffered (cap 1) completion channel plus the mode WaitChatRun should
	// use if no completion signal arrives. Guarded by runMu.
	//
	// runHookTokens is keyed by agent_session_id. Maps to the per-run hook
	// token registered when StartChatRun installed the run-scoped Stop hook.
	// The hook endpoint validates the inbound Bearer token against this map
	// before signaling the channel. Guarded by runMu.
	//
	// runSessionByID is keyed by agent_session_id. Reverse index back to the
	// boss session_id, so the hook handler can clear IsRepairing on the
	// right session without re-querying the DB. Guarded by runMu.
	//
	// Lifetime note: runHookTokens and runSessionByID are deliberately NOT
	// cleared by CompleteAgentRun on success — they survive until WaitChatRun
	// (Task 4) tears them down when the wait completes or its deadline
	// elapses. Leaving them in place lets duplicate Stop-hook POSTs for the
	// same agent_session_id authenticate against the live token entry and
	// short-circuit to 200 (idempotent, per spec). runCompletion and
	// activeRuns are the per-run state that does get cleared on first
	// signal — the surviving tombstone is just enough to recognise "still
	// known, already signalled" without leaking the channel.
	runMu            sync.Mutex
	activeRuns       map[string]activeRun
	agentSessionByID map[string]string
	runCompletion    map[string]activeChatRun
	runHookTokens    map[string]string
	runSessionByID   map[string]string

	// pendingChatRuns marks sessions with an in-flight StartChatRun that has
	// not yet registered its runCompletion entry. StartChatRun sets the flag
	// before it asks the lifecycle to spawn the tmux agent (which is also
	// where the hookless completion poll is armed) and clears it once the run
	// is registered or the call fails. It bounds earlyChatCompletions so a
	// poll completion that races ahead of registration is only retained for
	// runs that will actually register a waiter — cron, finalize, and
	// bootstrap re-arm runs never set the flag, so their SignalRunComplete
	// no-ops as before and leave nothing behind. Keyed by session_id, guarded
	// by runMu.
	pendingChatRuns map[string]bool
	// earlyChatCompletions holds completion results that arrived (via
	// SignalRunComplete) after the hookless poll observed the agent exit but
	// before StartChatRun registered the run. Without this, the signal would
	// no-op against an empty runCompletion map and the matching WaitChatRun
	// would block until its deadline. Keyed by session_id; drained by
	// registerRunWithCompletionMode and discarded by clearPendingChatRun.
	// Guarded by runMu.
	earlyChatCompletions map[string]completionResult

	// lifecycle owns the tmux-hosted chat lifecycle (StartTmuxChat /
	// FinalizeSession). Required by StartChatRun. Wired post-construction
	// via SetLifecycle so cmd/main.go can still build either object first.
	// Tests that exercise StartChatRun directly install a fake here; tests
	// that don't use StartChatRun leave it nil and the per-handler nil
	// guard surfaces FailedPrecondition.
	lifecycle ChatLifecycle

	// waitChatRunDeadline overrides the default 30m deadline for
	// WaitChatRun. Tests with synthetic deadlines flip this via the
	// (test-only) setWaitChatRunDeadline helper in host_service_test.go
	// so the deadline path is observable in milliseconds rather than
	// minutes; production paths leave it at the default.
	waitChatRunDeadline time.Duration
}

// completionResult is the value pushed onto runCompletion when a run
// finishes (either via the Stop-hook POST or the WaitChatRun deadline).
// exitError is the message from the hook payload; empty on clean exit.
type completionResult struct {
	exitError string
}

type chatRunCompletionMode string

const (
	chatRunCompletionHook     chatRunCompletionMode = "hook"
	chatRunCompletionExplicit chatRunCompletionMode = "explicit"
)

type activeChatRun struct {
	done           chan completionResult
	completionMode chatRunCompletionMode
}

// activeRun records the agent plugin name and agent session ID for an
// in-flight StartAgentRun on a given boss session. agentName lets
// WaitAgentRun and isAgentRunning route the follow-up RPCs to the right
// plugin client when multiple agent plugins are loaded.
type activeRun struct {
	agentName      string
	agentSessionID string
}

// NewHostServiceServer creates a HostServiceServer that proxies to the
// given VCS provider. Session-related functionality requires SetSessionDeps
// to be called before use; agent execution (StartAgentRun/WaitAgentRun)
// requires SetAgentClients + SetAgentLogsDir.
func NewHostServiceServer(provider vcs.Provider) *HostServiceServer {
	return &HostServiceServer{
		provider:             provider,
		activeRuns:           make(map[string]activeRun),
		agentSessionByID:     make(map[string]string),
		runCompletion:        make(map[string]activeChatRun),
		runHookTokens:        make(map[string]string),
		runSessionByID:       make(map[string]string),
		pendingChatRuns:      make(map[string]bool),
		earlyChatCompletions: make(map[string]completionResult),
		agentClients:         map[string]agent.AgentRunnerClient{},
		waitChatRunDeadline:  defaultWaitChatRunDeadline,
	}
}

// registerRun installs the run-scoped state needed by WaitChatRun and the
// /hooks/agent-run-complete/{agent_session_id} handler. Returns the
// buffered (cap 1) completion channel the caller (Task 4's
// StartChatRun) should select on.
//
// Package-private — Task 4's StartChatRun calls this from the same
// package. The hook server only consumes via CompleteAgentRun, which
// goes through the AgentRunCompleter interface.
//
// Idempotency: if an entry for agentSessionID already exists, it is
// overwritten — the previous channel's reader is stranded (will block
// until its WaitChatRun deadline elapses). The caller is responsible
// for refusing concurrent StartChatRun calls (via activeRuns) before
// reaching this point; in production the activeRuns mutex prevents two
// live registrations for the same agent_session_id.
func (s *HostServiceServer) registerRun(sessionID, agentSessionID, token string) chan completionResult {
	return s.registerRunWithCompletionMode(sessionID, agentSessionID, token, chatRunCompletionHook)
}

func (s *HostServiceServer) registerRunWithCompletionMode(sessionID, agentSessionID, token string, mode chatRunCompletionMode) chan completionResult {
	ch := make(chan completionResult, 1)
	s.runMu.Lock()
	s.runCompletion[agentSessionID] = activeChatRun{done: ch, completionMode: mode}
	s.runHookTokens[agentSessionID] = token
	s.runSessionByID[agentSessionID] = sessionID
	// Drain any completion that the hookless poll signalled before this
	// registration landed (see earlyChatCompletions). Deliver it onto the
	// buffered channel so the matching WaitChatRun returns immediately
	// instead of blocking until its deadline. The session is no longer
	// pending once registered.
	if res, ok := s.earlyChatCompletions[sessionID]; ok {
		delete(s.earlyChatCompletions, sessionID)
		select {
		case ch <- res:
		default:
		}
	}
	delete(s.pendingChatRuns, sessionID)
	s.runMu.Unlock()
	return ch
}

// markPendingChatRun records that StartChatRun is about to spawn a run for
// sessionID whose runCompletion entry has not been registered yet. While the
// flag is set, a SignalRunComplete that races ahead of registration is
// retained in earlyChatCompletions rather than discarded. Guarded by runMu.
func (s *HostServiceServer) markPendingChatRun(sessionID string) {
	s.runMu.Lock()
	s.pendingChatRuns[sessionID] = true
	s.runMu.Unlock()
}

// clearPendingChatRun drops the pending marker for sessionID and discards any
// early completion that was never drained by a registration (e.g. StartTmuxChat
// failed, or returned an AlreadyExists id that took a different code path).
// StartChatRun defers this so a racing poll completion can't outlive the call
// that armed it. Guarded by runMu.
func (s *HostServiceServer) clearPendingChatRun(sessionID string) {
	s.runMu.Lock()
	delete(s.pendingChatRuns, sessionID)
	delete(s.earlyChatCompletions, sessionID)
	s.runMu.Unlock()
}

// SetSessionDeps injects the dependencies needed for session-related RPCs
// (ListSessions, GetReviewComments, FireSessionEvent). The chats store is
// needed to surface per-session HasActiveChat in ListSessions.
func (s *HostServiceServer) SetSessionDeps(repos db.RepoStore, sessions db.SessionStore, chats db.AgentChatStore, tracker *status.DisplayTracker, chatTracker *status.Tracker) {
	s.repoStore = repos
	s.sessionStore = sessions
	s.agentChats = chats
	s.displayTracker = tracker
	s.chatTracker = chatTracker
}

// SetAgentClients injects the per-name registry of AgentRunnerClient gRPC
// clients (one per loaded AgentRunner plugin, keyed by plugin Name —
// matching session.AgentName). The repair plugin's StartAgentRun /
// WaitAgentRun host RPCs look up the right client based on the session
// record's AgentName so non-agent plugins don't need a direct broker
// channel into each agent plugin. A nil or empty map disables the agent
// path entirely; per-handler nil guards surface that as FailedPrecondition
// when a plugin actually tries to use it.
//
// Concurrency: called exactly once during daemon startup, before plugins
// begin issuing host RPCs. Not safe for concurrent re-injection.
func (s *HostServiceServer) SetAgentClients(m map[string]agent.AgentRunnerClient) {
	if m == nil {
		s.agentClients = map[string]agent.AgentRunnerClient{}
		return
	}
	s.agentClients = m
}

// SetAgentLogsDir sets the bossd-owned directory where the agent plugin
// writes per-session NDJSON log files. Required alongside SetAgentClients.
func (s *HostServiceServer) SetAgentLogsDir(dir string) {
	s.agentLogsDir = dir
}

// SetLifecycle wires the lifecycle that StartChatRun delegates to. The
// daemon constructs HostServiceServer and *session.Lifecycle independently
// then calls this once during startup; tests substitute a fake. Passing
// nil is harmless — StartChatRun's per-handler nil guard surfaces it as
// FailedPrecondition.
//
// Concurrency: called exactly once during daemon startup, before plugins
// begin issuing host RPCs. Not safe for concurrent re-injection.
func (s *HostServiceServer) SetLifecycle(lc ChatLifecycle) {
	s.lifecycle = lc
}

// AgentClientNames returns the registered agent plugin names (the keys of
// agentClients). Read-only; used by RepairDoctor to verify a "claude" entry
// is wired without exposing the underlying clients.
func (s *HostServiceServer) AgentClientNames() []string {
	out := make([]string, 0, len(s.agentClients))
	for name := range s.agentClients {
		out = append(out, name)
	}
	return out
}

// AgentLogsDir returns the directory where per-session NDJSON log files
// land. Empty string if SetAgentLogsDir hasn't been called.
func (s *HostServiceServer) AgentLogsDir() string {
	return s.agentLogsDir
}

// Validate reports any dependencies that SetSessionDeps was expected to
// install but that are still nil. Host.Start calls this before launching
// plugins so a missing dep fails the daemon loudly at startup instead of
// surfacing later as an "Unavailable" RPC the first time a plugin tries to
// call back. The per-handler nil guards are kept as defense-in-depth for
// anyone using HostServiceServer directly (e.g. tests).
func (s *HostServiceServer) Validate() error {
	var missing []string
	if s.provider == nil {
		missing = append(missing, "vcs provider")
	}
	if s.sessionStore == nil {
		missing = append(missing, "session store")
	}
	if s.agentChats == nil {
		missing = append(missing, "agent chat store")
	}
	if s.repoStore == nil {
		missing = append(missing, "repo store")
	}
	if s.displayTracker == nil {
		missing = append(missing, "PR tracker")
	}
	if s.chatTracker == nil {
		missing = append(missing, "chat tracker")
	}
	// agentClients / agentLogsDir are intentionally not validated here:
	// they're injected post-Start (main.go calls SetAgentClients after the
	// AgentRunner plugins have been dispensed). Per-handler nil guards in
	// StartAgentRun / WaitAgentRun surface the missing dep at call time.
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("host service missing dependencies: %s", strings.Join(missing, ", "))
}

// hostServiceDesc is a manually-built gRPC service descriptor for
// HostService. We build this manually because the project uses
// connect-go (not protoc-gen-go-grpc) for code generation, so there
// is no generated _ServiceDesc. go-plugin requires raw gRPC registration.
var hostServiceDesc = grpc.ServiceDesc{
	ServiceName: "bossanova.v1.HostService",
	HandlerType: (*hostServiceHandler)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "ListOpenPRs",
			Handler:    hostServiceListOpenPRsHandler,
		},
		{
			MethodName: "GetCheckResults",
			Handler:    hostServiceGetCheckResultsHandler,
		},
		{
			MethodName: "GetPRStatus",
			Handler:    hostServiceGetPRStatusHandler,
		},
		{
			MethodName: "ListClosedPRs",
			Handler:    hostServiceListClosedPRsHandler,
		},
		{
			MethodName: "ListSessions",
			Handler:    hostServiceListSessionsHandler,
		},
		{
			MethodName: "GetReviewComments",
			Handler:    hostServiceGetReviewCommentsHandler,
		},
		{
			MethodName: "FireSessionEvent",
			Handler:    hostServiceFireSessionEventHandler,
		},
		{
			MethodName: "SetRepairStatus",
			Handler:    hostServiceSetRepairStatusHandler,
		},
		{
			MethodName: "StartAgentRun",
			Handler:    hostServiceStartAgentRunHandler,
		},
		{
			MethodName: "WaitAgentRun",
			Handler:    hostServiceWaitAgentRunHandler,
		},
		{
			MethodName: "StartChatRun",
			Handler:    hostServiceStartChatRunHandler,
		},
		{
			MethodName: "WaitChatRun",
			Handler:    hostServiceWaitChatRunHandler,
		},
		{
			MethodName: "ReclaimRepairChat",
			Handler:    hostServiceReclaimRepairChatHandler,
		},
		{
			MethodName: "RecordRepairOutcome",
			Handler:    hostServiceRecordRepairOutcomeHandler,
		},
	},
	Metadata: "bossanova/v1/host_service.proto",
}

// Compile-time verification that *HostServiceServer satisfies the
// gRPC handler interface. Catches signature drift at compile time
// instead of as a runtime panic during the first dispatch.
var _ hostServiceHandler = (*HostServiceServer)(nil)

// Compile-time verification that *session.Lifecycle satisfies the
// ChatLifecycle contract StartChatRun depends on. If session.Lifecycle's
// StartTmuxChat ever drifts (signature change, removal), this fails to
// compile rather than blowing up at SetLifecycle-time in production.
var _ ChatLifecycle = (*session.Lifecycle)(nil)

// hostServiceHandler is the interface that the gRPC service descriptor
// expects. HostServiceServer implements it.
type hostServiceHandler interface {
	ListOpenPRs(context.Context, *bossanovav1.ListOpenPRsRequest) (*bossanovav1.ListOpenPRsResponse, error)
	GetCheckResults(context.Context, *bossanovav1.GetCheckResultsRequest) (*bossanovav1.GetCheckResultsResponse, error)
	GetPRStatus(context.Context, *bossanovav1.GetPRStatusRequest) (*bossanovav1.GetPRStatusResponse, error)
	ListClosedPRs(context.Context, *bossanovav1.ListClosedPRsRequest) (*bossanovav1.ListClosedPRsResponse, error)
	ListSessions(context.Context, *bossanovav1.HostServiceListSessionsRequest) (*bossanovav1.HostServiceListSessionsResponse, error)
	GetReviewComments(context.Context, *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error)
	FireSessionEvent(context.Context, *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error)
	SetRepairStatus(context.Context, *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error)
	StartAgentRun(context.Context, *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error)
	WaitAgentRun(context.Context, *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error)
	StartChatRun(context.Context, *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error)
	WaitChatRun(context.Context, *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error)
	ReclaimRepairChat(context.Context, *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error)
	RecordRepairOutcome(context.Context, *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error)
}

// Register registers the HostService on a gRPC server (used by the
// go-plugin broker).
func (s *HostServiceServer) Register(srv *grpc.Server) {
	srv.RegisterService(&hostServiceDesc, s)
}

func hostServiceListOpenPRsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListOpenPRsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).ListOpenPRs(ctx, req)
}

func hostServiceGetCheckResultsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetCheckResultsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).GetCheckResults(ctx, req)
}

func hostServiceGetPRStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.GetPRStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).GetPRStatus(ctx, req)
}

func hostServiceListClosedPRsHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ListClosedPRsRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).ListClosedPRs(ctx, req)
}

// --- VCS RPC implementations ---

func (s *HostServiceServer) ListOpenPRs(ctx context.Context, req *bossanovav1.ListOpenPRsRequest) (*bossanovav1.ListOpenPRsResponse, error) {
	prs, err := s.provider.ListOpenPRs(ctx, req.GetRepoOriginUrl())
	if err != nil {
		return nil, err
	}

	pbPRs := make([]*bossanovav1.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &bossanovav1.PRSummary{
			Number:     int32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      vcsPRStateToProto(pr.State),
			Author:     pr.Author,
		}
	}

	return &bossanovav1.ListOpenPRsResponse{Prs: pbPRs}, nil
}

func (s *HostServiceServer) GetCheckResults(ctx context.Context, req *bossanovav1.GetCheckResultsRequest) (*bossanovav1.GetCheckResultsResponse, error) {
	checks, err := s.provider.GetCheckResults(ctx, req.GetRepoOriginUrl(), int(req.GetPrNumber()))
	if err != nil {
		return nil, err
	}

	pbChecks := make([]*bossanovav1.CheckResult, len(checks))
	for i, c := range checks {
		pbCheck := &bossanovav1.CheckResult{
			Id:     c.ID,
			Name:   c.Name,
			Status: vcsCheckStatusToProto(c.Status),
		}
		if c.Conclusion != nil {
			cc := vcsCheckConclusionToProto(*c.Conclusion)
			pbCheck.Conclusion = &cc
		}
		pbChecks[i] = pbCheck
	}

	return &bossanovav1.GetCheckResultsResponse{Checks: pbChecks}, nil
}

func (s *HostServiceServer) GetPRStatus(ctx context.Context, req *bossanovav1.GetPRStatusRequest) (*bossanovav1.GetPRStatusResponse, error) {
	status, err := s.provider.GetPRStatus(ctx, req.GetRepoOriginUrl(), int(req.GetPrNumber()))
	if err != nil {
		return nil, err
	}

	pbStatus := &bossanovav1.PRStatus{
		State:      vcsPRStateToProto(status.State),
		Title:      status.Title,
		HeadBranch: status.HeadBranch,
		BaseBranch: status.BaseBranch,
	}
	if status.Mergeable != nil {
		pbStatus.Mergeable = status.Mergeable
	}

	return &bossanovav1.GetPRStatusResponse{Status: pbStatus}, nil
}

func (s *HostServiceServer) ListClosedPRs(ctx context.Context, req *bossanovav1.ListClosedPRsRequest) (*bossanovav1.ListClosedPRsResponse, error) {
	prs, err := s.provider.ListClosedPRs(ctx, req.GetRepoOriginUrl())
	if err != nil {
		return nil, err
	}

	pbPRs := make([]*bossanovav1.PRSummary, len(prs))
	for i, pr := range prs {
		pbPRs[i] = &bossanovav1.PRSummary{
			Number:     int32(pr.Number),
			Title:      pr.Title,
			HeadBranch: pr.HeadBranch,
			State:      vcsPRStateToProto(pr.State),
			Author:     pr.Author,
		}
	}

	return &bossanovav1.ListClosedPRsResponse{Prs: pbPRs}, nil
}

func (s *HostServiceServer) ListSessions(ctx context.Context, req *bossanovav1.HostServiceListSessionsRequest) (*bossanovav1.HostServiceListSessionsResponse, error) {
	if s.repoStore == nil || s.sessionStore == nil || s.displayTracker == nil {
		return nil, grpcstatus.Error(codes.Internal, "session dependencies not set")
	}

	// Iterate all repos and collect their active sessions
	repos, err := s.repoStore.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list repos: %w", err)
	}

	var pbSessions []*bossanovav1.Session
	for _, repo := range repos {
		sessions, err := s.sessionStore.ListActive(ctx, repo.ID)
		if err != nil {
			log.Warn().Err(err).Str("repo_id", repo.ID).Msg("Failed to list sessions for repo")
			continue
		}

		for _, sess := range sessions {
			// Hydrate with DisplayTracker status
			entry := s.displayTracker.Get(sess.ID)
			var displayStatus vcs.DisplayStatus
			var hasFailures bool
			var isRepairing bool
			var headSHA string
			if entry != nil {
				displayStatus = entry.Status
				hasFailures = entry.HasFailures
				isRepairing = entry.IsRepairing
				headSHA = entry.HeadSHA
			}

			// Check heartbeat tracker for active Claude Code chat processes.
			// On DB error, assume active (fail closed) to prevent advancing
			// sessions that may have a live Claude Code process.
			hasActiveChat := false
			var latestChatActivity time.Time
			if s.agentChats != nil && s.chatTracker != nil {
				chats, err := s.agentChats.ListBySession(ctx, sess.ID)
				if err != nil {
					log.Warn().Err(err).Str("session_id", sess.ID).Msg("failed to list chats for active chat check, assuming active")
					hasActiveChat = true
				} else {
					for _, chat := range chats {
						if session.IsRepairChatTitle(chat.Title) {
							// BOS-153: a repair-titled chat the tracker deems
							// displaceable (idle past the repair window, at a
							// long-stuck question, or stopped) is a dead/dying
							// run — do not count it as an active chat, which
							// would defer auto-repair forever. Evaluated from
							// the sampled chat tracker, never the pipe-pane log
							// mtime (which an idle TUI refreshes forever). A
							// cold/absent tracker entry is NOT displaceable
							// here; the later Get(...) nil check below already
							// drops such chats from the active tally.
							if s.chatDisplaceable(chat.AgentSessionID, time.Time{}, displacePolicy{MinIdle: repairDisplaceMinIdle, QuestionIdle: repairDisplaceQuestionIdle}, time.Now()) == nil {
								continue
							}
						}
						// Skip chats whose start failed: the agent_chats row is
						// preserved (so the operator sees the × failed badge) but
						// the agent never came up, so any tracker entry on its
						// AgentSessionID is an orphan tmux pane keeping the
						// heartbeat warm. Counting it as an active chat makes the
						// repair plugin's idle gate defer forever — the
						// failed-start row would permanently block its own session
						// from being auto-repaired.
						if chat.StartError != nil && *chat.StartError != "" {
							continue
						}
						chatEntry := s.chatTracker.Get(chat.AgentSessionID)
						if chatEntry == nil {
							continue
						}
						hasActiveChat = true
						if chatEntry.LastOutputAt.After(latestChatActivity) {
							latestChatActivity = chatEntry.LastOutputAt
						}
					}
				}
			}

			pbSess := &bossanovav1.Session{
				Id:                          sess.ID,
				RepoId:                      sess.RepoID,
				RepoDisplayName:             repo.DisplayName,
				RepoOriginUrl:               repo.OriginURL,
				RepoCanAutoRepair:           repo.CanAutoRepair,
				Title:                       sess.Title,
				BranchName:                  sess.BranchName,
				State:                       sessionStateToProto(sess.State),
				DisplayStatus:               vcsDisplayStatusToProto(displayStatus),
				DisplayHasFailures:          hasFailures,
				DisplayIsRepairing:          isRepairing,
				HasActiveChat:               hasActiveChat,
				PrDisplayHeadSha:            headSHA,
				LastRepairRunnerError:       sess.LastRepairRunnerError,
				LastRepairExitError:         sess.LastRepairExitError,
				LastRepairAttemptCount:      int32(sess.LastRepairAttemptCount),
				LastRepairHeadSha:           sess.LastRepairHeadSHA,
				LastRepairDisplayStatus:     bossanovav1.DisplayStatus(sess.LastRepairDisplayStatus),
				LastRepairReviewFingerprint: sess.LastRepairReviewFingerprint,
				LastRepairBlockedReason:     sess.LastRepairBlockedReason,
			}
			if sess.PRNumber != nil {
				prNumber := int32(*sess.PRNumber)
				pbSess.PrNumber = &prNumber
			}
			if !sess.UpdatedAt.IsZero() {
				pbSess.UpdatedAt = timestamppb.New(sess.UpdatedAt)
			}
			if sess.LastRepairStartedAt != nil {
				pbSess.LastRepairStartedAt = timestamppb.New(*sess.LastRepairStartedAt)
			}
			if sess.LastRepairBlockedAt != nil {
				pbSess.LastRepairBlockedAt = timestamppb.New(*sess.LastRepairBlockedAt)
			}
			if !latestChatActivity.IsZero() {
				pbSess.LastChatActivityAt = timestamppb.New(latestChatActivity)
			}
			pbSessions = append(pbSessions, pbSess)
		}
	}

	return &bossanovav1.HostServiceListSessionsResponse{Sessions: pbSessions}, nil
}

func (s *HostServiceServer) GetReviewComments(ctx context.Context, req *bossanovav1.GetReviewCommentsRequest) (*bossanovav1.GetReviewCommentsResponse, error) {
	comments, err := s.provider.GetReviewComments(ctx, req.GetRepoOriginUrl(), int(req.GetPrNumber()))
	if err != nil {
		return nil, err
	}

	pbComments := make([]*bossanovav1.ReviewComment, len(comments))
	for i, comment := range comments {
		var line *int32
		if comment.Line != nil {
			l := int32(*comment.Line)
			line = &l
		}
		pbComments[i] = &bossanovav1.ReviewComment{
			Author: comment.Author,
			Body:   comment.Body,
			State:  vcsReviewStateToProto(comment.State),
			Path:   comment.Path,
			Line:   line,
		}
	}

	return &bossanovav1.GetReviewCommentsResponse{Comments: pbComments}, nil
}

func (s *HostServiceServer) FireSessionEvent(ctx context.Context, req *bossanovav1.FireSessionEventRequest) (*bossanovav1.FireSessionEventResponse, error) {
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}

	// Load session
	session, err := s.sessionStore.Get(ctx, req.GetSessionId())
	if err != nil {
		return nil, fmt.Errorf("get session: %w", err)
	}

	// Create state machine with full session context so guards (fixOrBlock,
	// retryOrBlock) correctly evaluate AttemptCount and HasPR.
	hasPR := session.PRNumber != nil
	sm := machine.NewWithContext(session.State, &machine.SessionContext{
		AttemptCount: session.AttemptCount,
		MaxAttempts:  machine.MaxAttempts,
		HasPR:        hasPR,
	})

	// Fire event
	event := protoToSessionEvent(req.GetEvent())
	if err := sm.Fire(event); err != nil {
		return nil, fmt.Errorf("fire event %v on state %v: %w", event, session.State, err)
	}

	// Persist new state and any context mutations (AttemptCount, BlockedReason).
	newState := sm.State()
	stateInt := int(newState)
	attemptCount := sm.Context().AttemptCount
	update := db.UpdateSessionParams{
		State:        &stateInt,
		AttemptCount: &attemptCount,
	}
	if sm.State() == machine.Blocked {
		reason := sm.Context().BlockedReason
		reasonPtr := &reason
		update.BlockedReason = &reasonPtr
	}

	if _, err = s.sessionStore.Update(ctx, session.ID, update); err != nil {
		return nil, fmt.Errorf("update session: %w", err)
	}

	return &bossanovav1.FireSessionEventResponse{NewState: newState.String()}, nil
}

// --- VCS domain type → proto enum converters ---

func vcsPRStateToProto(s vcs.PRState) bossanovav1.PRState {
	switch s {
	case vcs.PRStateOpen:
		return bossanovav1.PRState_PR_STATE_OPEN
	case vcs.PRStateClosed:
		return bossanovav1.PRState_PR_STATE_CLOSED
	case vcs.PRStateMerged:
		return bossanovav1.PRState_PR_STATE_MERGED
	default:
		return bossanovav1.PRState_PR_STATE_UNSPECIFIED
	}
}

func vcsCheckStatusToProto(s vcs.CheckStatus) bossanovav1.CheckStatus {
	switch s {
	case vcs.CheckStatusQueued:
		return bossanovav1.CheckStatus_CHECK_STATUS_QUEUED
	case vcs.CheckStatusInProgress:
		return bossanovav1.CheckStatus_CHECK_STATUS_IN_PROGRESS
	case vcs.CheckStatusCompleted:
		return bossanovav1.CheckStatus_CHECK_STATUS_COMPLETED
	default:
		return bossanovav1.CheckStatus_CHECK_STATUS_UNSPECIFIED
	}
}

func vcsCheckConclusionToProto(c vcs.CheckConclusion) bossanovav1.CheckConclusion {
	switch c {
	case vcs.CheckConclusionSuccess:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_SUCCESS
	case vcs.CheckConclusionFailure:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_FAILURE
	case vcs.CheckConclusionNeutral:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_NEUTRAL
	case vcs.CheckConclusionCancelled:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_CANCELLED
	case vcs.CheckConclusionSkipped:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_SKIPPED
	case vcs.CheckConclusionTimedOut:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_TIMED_OUT
	default:
		return bossanovav1.CheckConclusion_CHECK_CONCLUSION_UNSPECIFIED
	}
}

func vcsReviewStateToProto(s vcs.ReviewState) bossanovav1.ReviewState {
	switch s {
	case vcs.ReviewStateApproved:
		return bossanovav1.ReviewState_REVIEW_STATE_APPROVED
	case vcs.ReviewStateChangesRequested:
		return bossanovav1.ReviewState_REVIEW_STATE_CHANGES_REQUESTED
	case vcs.ReviewStateCommented:
		return bossanovav1.ReviewState_REVIEW_STATE_COMMENTED
	case vcs.ReviewStateDismissed:
		return bossanovav1.ReviewState_REVIEW_STATE_DISMISSED
	default:
		return bossanovav1.ReviewState_REVIEW_STATE_UNSPECIFIED
	}
}

func vcsDisplayStatusToProto(s vcs.DisplayStatus) bossanovav1.DisplayStatus {
	return bossanovav1.DisplayStatus(s)
}

func sessionStateToProto(s machine.State) bossanovav1.SessionState {
	return bossanovav1.SessionState(s)
}

func protoToSessionEvent(e bossanovav1.SessionEvent) machine.Event {
	return machine.Event(e)
}

// --- gRPC handler adapters ---

func hostServiceListSessionsHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(bossanovav1.HostServiceListSessionsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(hostServiceHandler).ListSessions(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/bossanova.v1.HostService/ListSessions",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(hostServiceHandler).ListSessions(ctx, req.(*bossanovav1.HostServiceListSessionsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func hostServiceGetReviewCommentsHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(bossanovav1.GetReviewCommentsRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(hostServiceHandler).GetReviewComments(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/bossanova.v1.HostService/GetReviewComments",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(hostServiceHandler).GetReviewComments(ctx, req.(*bossanovav1.GetReviewCommentsRequest))
	}
	return interceptor(ctx, in, info, handler)
}

func (s *HostServiceServer) SetRepairStatus(_ context.Context, req *bossanovav1.SetRepairStatusRequest) (*bossanovav1.SetRepairStatusResponse, error) {
	if s.displayTracker == nil {
		return nil, grpcstatus.Error(codes.Internal, "PR tracker not set")
	}
	s.displayTracker.SetRepairing(req.GetSessionId(), req.GetIsRepairing())
	return &bossanovav1.SetRepairStatusResponse{}, nil
}

func hostServiceSetRepairStatusHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.SetRepairStatusRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).SetRepairStatus(ctx, req)
}

func hostServiceFireSessionEventHandler(srv interface{}, ctx context.Context, dec func(interface{}) error, interceptor grpc.UnaryServerInterceptor) (interface{}, error) {
	in := new(bossanovav1.FireSessionEventRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(hostServiceHandler).FireSessionEvent(ctx, in)
	}
	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/bossanova.v1.HostService/FireSessionEvent",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(hostServiceHandler).FireSessionEvent(ctx, req.(*bossanovav1.FireSessionEventRequest))
	}
	return interceptor(ctx, in, info, handler)
}

// StartAgentRun resolves the session's worktree and starts an agent run via
// the loaded AgentRunner plugin. Returns the resolved session ID (named
// claude_id in the proto for compatibility with the repair plugin's
// existing variable name).
//
// Enforces one active agent run per session: returns AlreadyExists if a
// previous run on the same session is still IsRunning. The activeRuns map
// is the daemon-side source of truth for that invariant; the repair
// plugin's in-memory dedup map sits in front of this as a courtesy.
func (s *HostServiceServer) StartAgentRun(ctx context.Context, req *bossanovav1.StartAgentRunHostRequest) (*bossanovav1.StartAgentRunHostResponse, error) {
	if len(s.agentClients) == 0 {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent client not configured")
	}
	if s.agentLogsDir == "" {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent logs dir not configured")
	}
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session_id is required")
	}
	if strings.TrimSpace(req.GetPrompt()) == "/boss-repair" {
		return nil, grpcstatus.Error(codes.FailedPrecondition,
			"/boss-repair must run through StartChatRun so the repair has a chat row and tmux session")
	}

	sess, err := s.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.NotFound, "session %s: %v", sessionID, err)
	}
	if sess.WorktreePath == "" {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "session %s has no worktree path", sessionID)
	}

	// Route to the agent plugin matching the session's recorded AgentName.
	// Sessions persisted before the multi-agent migration may have an empty
	// AgentName here; the SQLite store's ""→"claude" fallback (Task 1)
	// keeps that case working as long as bossd-plugin-claude is loaded.
	client, ok := s.agentClients[sess.AgentName]
	if !ok || client == nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "agent %q not configured", sess.AgentName)
	}

	s.runMu.Lock()
	if existing, ok := s.activeRuns[sessionID]; ok && s.isAgentRunning(existing.agentName, existing.agentSessionID) {
		s.runMu.Unlock()
		return nil, grpcstatus.Errorf(codes.AlreadyExists, "agent run already active for session %s", sessionID)
	}
	// Stale entry (previous run finished) — fall through and replace.
	//
	// Use context.Background, NOT the gRPC handler's ctx: passing ctx would
	// tie the spawned process's lifetime to this RPC call, which returns
	// immediately. The agent plugin has its own Stop / shutdown path.
	startReq := &bossanovav1.StartAgentRunRequest{
		WorkDir: sess.WorktreePath,
		Plan:    req.GetPrompt(),
		LogPath: filepath.Join(s.agentLogsDir, "repair-"+sessionID+".log"),
		Model:   sess.Model,
	}
	startResp, err := client.StartRun(context.Background(), startReq)
	if err != nil {
		s.runMu.Unlock()
		return nil, grpcstatus.Errorf(codes.Internal, "start agent: %v", err)
	}
	resolved := startResp.GetSessionId()
	if prev, ok := s.activeRuns[sessionID]; ok {
		// Drop the reverse-index entry from the previous (now-stale) run so
		// it doesn't leak when a session is repaired multiple times.
		delete(s.agentSessionByID, prev.agentSessionID)
	}
	s.activeRuns[sessionID] = activeRun{agentName: sess.AgentName, agentSessionID: resolved}
	s.agentSessionByID[resolved] = sess.AgentName
	s.runMu.Unlock()

	return &bossanovav1.StartAgentRunHostResponse{AgentSessionId: resolved}, nil
}

// isAgentRunning reports whether the named agent plugin still has an
// active run for the given agent session ID. RPC errors (and a missing
// client) are treated as "not running" so a transient failure or unloaded
// plugin doesn't permanently wedge a session behind a stale activeRuns
// entry.
func (s *HostServiceServer) isAgentRunning(agentName, agentSessionID string) bool {
	client, ok := s.agentClients[agentName]
	if !ok || client == nil {
		return false
	}
	resp, err := client.IsRunning(context.Background(), &bossanovav1.IsAgentRunningRequest{SessionId: agentSessionID})
	if err != nil {
		return false
	}
	return resp.GetRunning()
}

// WaitAgentRun blocks until the agent run identified by agent_session_id
// exits, then returns the agent's exit error string (empty on clean exit).
// Honours the caller's context for cancellation — the daemon's plugin
// shutdown path cancels these contexts so an in-flight repair drains
// promptly.
//
// The agent_session_id is reverse-mapped to the originating agent plugin
// via agentSessionByID (populated by StartAgentRun). When the mapping is
// missing — e.g. a daemon restart between StartAgentRun and WaitAgentRun
// dropped in-memory state — we fall back to "agent client not configured"
// rather than guessing.
func (s *HostServiceServer) WaitAgentRun(ctx context.Context, req *bossanovav1.WaitAgentRunHostRequest) (*bossanovav1.WaitAgentRunHostResponse, error) {
	if len(s.agentClients) == 0 {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent client not configured")
	}
	agentSessionID := req.GetAgentSessionId()
	if agentSessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "agent_session_id is required")
	}

	s.runMu.Lock()
	agentName, ok := s.agentSessionByID[agentSessionID]
	s.runMu.Unlock()
	if !ok {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "no agent client recorded for agent session %s", agentSessionID)
	}
	client, ok := s.agentClients[agentName]
	if !ok || client == nil {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "agent %q not configured", agentName)
	}

	ticker := time.NewTicker(waitAgentRunPollInterval)
	defer ticker.Stop()
	for {
		es, err := client.ExitStatus(ctx, &bossanovav1.AgentExitStatusRequest{SessionId: agentSessionID})
		if err != nil {
			return nil, grpcstatus.Errorf(codes.Internal, "exit status: %v", err)
		}
		if es.GetIsComplete() {
			return &bossanovav1.WaitAgentRunHostResponse{ExitError: es.GetExitError()}, nil
		}
		select {
		case <-ctx.Done():
			return nil, grpcstatus.FromContextError(ctx.Err()).Err()
		case <-ticker.C:
		}
	}
}

func hostServiceStartAgentRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartAgentRunHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).StartAgentRun(ctx, req)
}

func hostServiceWaitAgentRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.WaitAgentRunHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).WaitAgentRun(ctx, req)
}

// RecordRepairOutcome persists the repair-attempt outcome onto the
// session row. Called by the repair plugin's deferred cleanup so every
// attempt — clean exit, agent error, or runner-level failure — leaves
// a structured trace in SQLite that the TUI can surface and that
// outlives the daemon's in-memory cooldowns.
func (s *HostServiceServer) RecordRepairOutcome(ctx context.Context, req *bossanovav1.RecordRepairOutcomeRequest) (*bossanovav1.RecordRepairOutcomeResponse, error) {
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session_id is required")
	}
	startedAt := time.Unix(req.GetStartedAtUnix(), 0)
	if req.GetStartedAtUnix() == 0 {
		startedAt = time.Now()
	}
	// Blocked-refusal lane: a non-empty blocked_reason means the daemon
	// refused to START the repair chat (a FailedPrecondition displace/reclaim
	// refusal) rather than running one. Record ONLY the blocked state — never
	// fall through to UpdateRepairDiagnostics, so last_repair_attempt_count and
	// the runner/exit error fields stay untouched and a start-refusal never
	// counts as a repair failure or feeds the exponential backoff.
	if reason := strings.TrimSpace(req.GetBlockedReason()); reason != "" {
		if err := s.sessionStore.UpdateRepairBlocked(ctx, sessionID, startedAt, reason); err != nil {
			return nil, grpcstatus.Errorf(codes.Internal, "update repair blocked: %v", err)
		}
		return &bossanovav1.RecordRepairOutcomeResponse{}, nil
	}
	if err := s.sessionStore.UpdateRepairDiagnostics(ctx, db.UpdateRepairDiagnosticsParams{
		SessionID:         sessionID,
		StartedAt:         startedAt,
		RunnerError:       req.GetRunnerError(),
		ExitError:         req.GetExitError(),
		HeadSHA:           req.GetHeadSha(),
		DisplayStatus:     int32(req.GetDisplayStatus()),
		ReviewFingerprint: req.ReviewFingerprint,
	}); err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "update repair diagnostics: %v", err)
	}
	return &bossanovav1.RecordRepairOutcomeResponse{}, nil
}

// CompleteAgentRun is the host-service entry point for the
// /hooks/agent-run-complete/{agent_session_id} endpoint. It validates the
// per-run bearer token, signals the run's completion channel, and clears
// the per-run channel/active-run state.
//
// Returns:
//   - (sessionID, nil)              on success (incl. duplicate POSTs)
//   - ("", ErrAgentRunNotFound)     if agent_session_id was never registered (404)
//   - (sessionID, ErrAuthMismatch)  if token mismatch (401). sessionID is
//     returned so the caller can include it in audit logs — we leak the
//     fact that the id was registered, not the token; the alternative
//     (returning "") would also leak that fact via timing differences
//     between the registered and unregistered branches, so we return it
//     explicitly and document the choice.
//
// Idempotency: a duplicate POST for an agent_session_id that was previously
// registered still returns success. The first call clears runCompletion +
// activeRuns; runHookTokens + runSessionByID intentionally survive until
// WaitChatRun (Task 4) tears them down. The surviving token entry lets the
// duplicate authenticate, the missing channel entry makes the signal a
// no-op, and the handler still returns 200 — matching the spec's "duplicate
// POSTs are idempotent" failure-mode contract.
//
// Concurrency: holds runMu only across the map mutations, then releases
// it before invoking displayTracker.SetRepairing — that tracker has its
// own lock and we don't want to nest. Clearing IsRepairing on a duplicate
// POST is a harmless no-op (it's already false).
func (s *HostServiceServer) CompleteAgentRun(_ context.Context, agentSessionID, bearerToken, exitError string) (string, error) {
	s.runMu.Lock()
	expectedToken, ok := s.runHookTokens[agentSessionID]
	if !ok {
		// Genuinely unknown id — never registered, or already cleaned up
		// by WaitChatRun. 404.
		s.runMu.Unlock()
		return "", ErrAgentRunNotFound
	}
	if subtle.ConstantTimeCompare([]byte(bearerToken), []byte(expectedToken)) != 1 {
		sessionID := s.runSessionByID[agentSessionID]
		s.runMu.Unlock()
		return sessionID, ErrAuthMismatch
	}

	// Auth passed. Signal the waiter if it's still around. The buffered
	// channel guards against double-signal: a duplicate POST finds the
	// channel entry already gone (cleared on the first call) and the send
	// is skipped. Either way the handler returns 200.
	if run, ok := s.runCompletion[agentSessionID]; ok {
		select {
		case run.done <- completionResult{exitError: exitError}:
		default:
		}
		// INVARIANT: runHookTokens / runSessionByID intentionally survive
		// past first signal — see field-level "Lifetime note" on
		// HostServiceServer. They serve as the tombstone that lets
		// duplicate POSTs return 200 instead of 404. Task 4's WaitChatRun
		// owns their cleanup. Do not delete them here.
		delete(s.runCompletion, agentSessionID)
	}

	sessionID := s.runSessionByID[agentSessionID]
	// activeRuns is keyed by session_id, not agent_session_id — clear it
	// only when this run is the currently-recorded one for the session.
	// (For Task 3 the maps are populated in tests; Task 4 wires the
	// production path.)
	if sessionID != "" {
		if cur, ok := s.activeRuns[sessionID]; ok && cur.agentSessionID == agentSessionID {
			delete(s.activeRuns, sessionID)
			delete(s.agentSessionByID, agentSessionID)
		}
	}
	// runHookTokens / runSessionByID intentionally NOT cleared here — see
	// the lifetime note on the field declarations. WaitChatRun (Task 4)
	// owns their cleanup; leaving them lets duplicate POSTs return 200.
	s.runMu.Unlock()

	// Clear the IsRepairing flag outside runMu — DisplayTracker has its
	// own lock and the spec is explicit about not nesting. Idempotent on
	// duplicate POSTs (clearing an already-cleared flag is a no-op).
	if sessionID != "" && s.displayTracker != nil {
		s.displayTracker.SetRepairing(sessionID, false)
	}

	return sessionID, nil
}

// SignalRunComplete is the in-process completion path for hookless agents.
// It performs the same channel signal + cleanup as CompleteAgentRun but
// without bearer-token auth — callers are trusted in-process code (the
// poll-fallback goroutine driven by *agent.PollFallback).
//
// External HTTP callers must continue to use CompleteAgentRun, which
// validates the bearer token. This asymmetry is intentional: the external
// path is reachable from any process on the loopback interface; the
// internal path is reachable only from inside this Go binary.
//
// Idempotent: a call for an agent_session_id whose completion channel was
// already cleared (e.g. the finalize hook arrived first, or a previous
// SignalRunComplete fired) is a no-op.
//
// Early-completion race: a hookless poll can observe the agent exit before
// StartChatRun registers the run (the poll is armed mid-spawn, registration
// happens after the spawn returns). When that happens for a session with a
// pending StartChatRun, the result is parked in earlyChatCompletions keyed by
// sessionID so registerRunWithCompletionMode can deliver it; otherwise the
// signal is a no-op as before, so cron/finalize/bootstrap runs (which never
// register a host-service waiter) leave nothing behind.
func (s *HostServiceServer) SignalRunComplete(sessionID, agentSessionID, exitError string) {
	s.runMu.Lock()
	run, ok := s.runCompletion[agentSessionID]
	if !ok {
		if sessionID != "" && s.pendingChatRuns[sessionID] {
			s.earlyChatCompletions[sessionID] = completionResult{exitError: exitError}
		}
		s.runMu.Unlock()
		// Not yet registered: parked above if a StartChatRun is pending,
		// otherwise an idempotent no-op (e.g. hook arrived first).
		return
	}
	select {
	case run.done <- completionResult{exitError: exitError}:
	default:
	}
	delete(s.runCompletion, agentSessionID)
	// Prefer the session_id recorded at registration; it is authoritative for
	// the cleanup below and matches the historical behaviour of this path.
	sessionID = s.runSessionByID[agentSessionID]
	if sessionID != "" {
		if cur, ok := s.activeRuns[sessionID]; ok && cur.agentSessionID == agentSessionID {
			delete(s.activeRuns, sessionID)
			delete(s.agentSessionByID, agentSessionID)
		}
	}
	// runHookTokens / runSessionByID intentionally NOT cleared here —
	// matches CompleteAgentRun's lifetime contract: WaitChatRun owns the
	// final teardown of those tombstone entries.
	s.runMu.Unlock()

	if sessionID != "" && s.displayTracker != nil {
		s.displayTracker.SetRepairing(sessionID, false)
	}
}

func hostServiceRecordRepairOutcomeHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.RecordRepairOutcomeRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).RecordRepairOutcome(ctx, req)
}

// newRunHookToken mints a fresh per-run bearer token for the run-keyed
// Stop hook. We do NOT reuse session.HookToken (which is the cron path's
// session-keyed token): repair runs are operator-attachable but
// independent of the cron lifecycle, and a fresh token per run keeps the
// hook table easy to reason about (one token = one Stop POST).
//
// 32 bytes of entropy (64 hex chars) — overkill for an in-memory map
// keyed by per-process agent_session_id but matches the cron token's
// shape so logs read consistently.
func newRunHookToken() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// StartChatRun spawns a tmux-hosted agent run for the given session and
// installs run-scoped state (activeRuns, agentSessionByID, runCompletion,
// runHookTokens, runSessionByID) so the daemon's hook server and a
// matching WaitChatRun call can route Stop-hook POSTs back to the
// originating run. Use this instead of StartAgentRun when the run should
// be operator-attachable (eg. /boss-repair).
//
// Concurrency: takes runMu only for the activeRuns precondition check
// and the post-spawn registration. Lifecycle.StartTmuxChat — which
// shells out to tmux + writes config files — runs without runMu held so
// a slow spawn can't block CompleteAgentRun on a sibling run. The
// lifecycle has its own per-session idempotency check (the AlreadyExists
// branch returns the existing agent_session_id), which is the same
// invariant the activeRuns short-circuit enforces here.
func (s *HostServiceServer) StartChatRun(ctx context.Context, req *bossanovav1.StartChatRunHostRequest) (*bossanovav1.StartChatRunHostResponse, error) {
	if len(s.agentClients) == 0 {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent client not configured")
	}
	if s.agentLogsDir == "" {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "agent logs dir not configured")
	}
	if s.sessionStore == nil {
		return nil, grpcstatus.Error(codes.Internal, "session store not set")
	}
	if s.lifecycle == nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "lifecycle not configured")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session_id is required")
	}
	if req.GetPrompt() == "" && req.GetCommand() == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "prompt or command is required")
	}
	if req.GetPrompt() != "" && req.GetCommand() != "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "prompt and command are mutually exclusive")
	}
	if req.GetTitle() == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "title is required")
	}
	if req.GetReplaceExistingChat() && !isAuthorizedRepairReplacement(req) {
		return nil, grpcstatus.Error(codes.InvalidArgument, "replace_existing_chat is only allowed for boss-repair commands with a repair title and reason")
	}

	sess, err := s.sessionStore.Get(ctx, sessionID)
	if err != nil {
		return nil, grpcstatus.Errorf(codes.NotFound, "session %s: %v", sessionID, err)
	}
	if _, ok := s.agentClients[sess.AgentName]; !ok {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "agent %q not configured", sess.AgentName)
	}

	// Pre-spawn precondition: another run on the same session must not be
	// alive. The lifecycle's findLiveTmuxChat covers the persistence-side
	// case (chat row + tmux still around after a daemon restart); this
	// activeRuns check covers the in-process race window where a second
	// caller arrives before the first has finished spawning.
	replacedActiveRun := false
	s.runMu.Lock()
	if existing, ok := s.activeRuns[sessionID]; ok && s.isAgentRunning(existing.agentName, existing.agentSessionID) {
		if req.GetReplaceExistingChat() {
			blockingAgentSessionID := existing.agentSessionID
			s.runMu.Unlock()
			if err := s.replaceBlockingChatForRepair(ctx, sessionID, blockingAgentSessionID, req.GetReplaceExistingReason(), req.GetReplaceExistingObservedLastChatActivityAt()); err != nil {
				return nil, err
			}
			s.signalAndCleanupReplacedRun(blockingAgentSessionID)
			replacedActiveRun = true
		} else {
			s.runMu.Unlock()
			return nil, grpcstatus.Errorf(codes.AlreadyExists, "chat run already active for session %s", sessionID)
		}
	} else {
		s.runMu.Unlock()
	}

	token, err := newRunHookToken()
	if err != nil {
		return nil, grpcstatus.Errorf(codes.Internal, "mint hook token: %v", err)
	}

	// StartTmuxChat handles its own teardown if the spawn / hook config /
	// SendPlan steps fail mid-flight, so we don't need cleanup logic here.
	//
	// Note: the run is registered in our maps after StartTmuxChat returns,
	// so a hook POST can't arrive before we've recorded its token. In
	// practice this is enforced by the protocol: the claude Stop hook only
	// fires after the agent processes a prompt, and SendPlan injects the
	// prompt as the final step of StartTmuxChat. A POST that arrived in
	// the registration gap would 404 — operators would see a hook failure
	// rather than a stuck session, but no current path reaches that gap.
	//
	// The hookless completion poll, by contrast, IS armed inside
	// StartTmuxChat (before this call returns) for agents without a finalize
	// hook. A fast-exiting agent can make that poll signal completion before
	// the registration below lands. Mark the session pending so
	// SignalRunComplete parks that early signal (see earlyChatCompletions)
	// instead of dropping it; the deferred clear guarantees the marker can't
	// outlive this call on any error/early-return path.
	s.markPendingChatRun(sessionID)
	defer s.clearPendingChatRun(sessionID)
	input := session.ChatInput{Prompt: req.GetPrompt(), Command: req.GetCommand(), ResumeAgentSessionID: req.GetResumeAgentSessionId()}
	agentSessionID, err := s.lifecycle.StartTmuxChat(ctx, sessionID, input, req.GetTitle(), session.HookOpts{Token: token})
	if err != nil && grpcstatus.Code(err) == codes.AlreadyExists && req.GetReplaceExistingChat() && !replacedActiveRun {
		blockingAgentSessionID := agentSessionID
		if blockingAgentSessionID == "" {
			blockingAgentSessionID = parseAlreadyExistsAgentSessionID(grpcstatus.Convert(err).Message())
		}
		if blockingAgentSessionID != "" {
			if replaceErr := s.replaceBlockingChatForRepair(ctx, sessionID, blockingAgentSessionID, req.GetReplaceExistingReason(), req.GetReplaceExistingObservedLastChatActivityAt()); replaceErr != nil {
				return nil, replaceErr
			}
			s.signalAndCleanupReplacedRun(blockingAgentSessionID)
			agentSessionID, err = s.lifecycle.StartTmuxChat(ctx, sessionID, input, req.GetTitle(), session.HookOpts{Token: token})
		}
	}
	if err != nil {
		// Pass typed gRPC errors (AlreadyExists, FailedPrecondition,
		// Internal) through unchanged. Wrap untyped errors as Internal
		// so callers always get a status code they can switch on.
		if grpcstatus.Code(err) != codes.Unknown {
			// AlreadyExists carries the existing agent_session_id in the
			// returned string. We rebuild the gRPC status with the existing
			// id encoded into the message because gRPC drops the response
			// body when err != nil — so a cross-process caller would
			// otherwise lose the id. The format is parseable:
			//   "<original msg> (agent_session_id=<id>)"
			// Callers (eg. Task 5's repair plugin) can extract the id with
			// regexp `agent_session_id=([^)]+)`.
			if grpcstatus.Code(err) == codes.AlreadyExists && agentSessionID != "" {
				return &bossanovav1.StartChatRunHostResponse{AgentSessionId: agentSessionID},
					grpcstatus.Errorf(codes.AlreadyExists, "%s (agent_session_id=%s)", grpcstatus.Convert(err).Message(), agentSessionID)
			}
			return nil, err
		}
		return nil, grpcstatus.Errorf(codes.Internal, "start tmux chat: %v", err)
	}

	// Register the run state under runMu. Order matters: register first
	// so a Stop-hook POST that arrives the instant the prompt is pasted
	// finds runHookTokens populated. registerRun handles the
	// runCompletion / runHookTokens / runSessionByID triplet under its
	// own lock; we acquire runMu around the activeRuns + agentSessionByID
	// pieces it doesn't touch.
	s.runMu.Lock()
	if prev, ok := s.activeRuns[sessionID]; ok {
		// Drop the stale reverse-index entry left behind when the
		// previous run finished without re-running through this path.
		delete(s.agentSessionByID, prev.agentSessionID)
	}
	s.activeRuns[sessionID] = activeRun{agentName: sess.AgentName, agentSessionID: agentSessionID}
	s.agentSessionByID[agentSessionID] = sess.AgentName
	s.runMu.Unlock()
	// Returned channel is unused here — WaitChatRun looks it up by
	// agent_session_id when the operator (or repair plugin) calls in.
	_ = s.registerRunWithCompletionMode(sessionID, agentSessionID, token, chatRunCompletionExplicit)

	return &bossanovav1.StartChatRunHostResponse{AgentSessionId: agentSessionID}, nil
}

// replaceBlockingChatForRepair gates a repair replacement on the chat
// tracker (via chatDisplaceable) before asking the lifecycle to kill the
// blocking pane. The old pipe-pane log-mtime staleness check is gone
// (BOS-153): displaceability is now evaluated solely from sampled tracker
// evidence. The observed snapshot the repair plugin captured before the
// call is still required — a zero/nil snapshot on the replace path is an
// error — and is passed to chatDisplaceable so a chat that spoke after the
// snapshot is refused. A repair-owned blocking chat (repair-titled) also
// becomes displaceable after a long-stuck QUESTION; other titles never are.
func (s *HostServiceServer) replaceBlockingChatForRepair(ctx context.Context, sessionID, agentSessionID, reason string, observedLastActivityAt *timestamppb.Timestamp) error {
	if reason == "" {
		reason = "repair replacing existing chat"
	}
	if observedLastActivityAt == nil || observedLastActivityAt.AsTime().IsZero() {
		return grpcstatus.Error(codes.FailedPrecondition, "replace blocking repair chat: observed last chat activity is required")
	}
	pol := displacePolicy{MinIdle: repairDisplaceMinIdle}
	if title, err := s.lifecycle.GetAgentChatTitle(ctx, agentSessionID); err == nil && session.IsRepairChatTitle(title) {
		pol.QuestionIdle = repairDisplaceQuestionIdle
	}
	if err := s.chatDisplaceable(agentSessionID, observedLastActivityAt.AsTime(), pol, time.Now()); err != nil {
		return err
	}
	if _, err := s.lifecycle.ReplaceBlockingChatForRepair(ctx, sessionID, agentSessionID, reason); err != nil {
		switch {
		case errors.Is(err, session.ErrRepairChatNotReclaimable),
			errors.Is(err, session.ErrRepairChatSessionMismatch),
			errors.Is(err, session.ErrRepairChatActive):
			return grpcstatus.Errorf(codes.FailedPrecondition, "replace blocking repair chat: %v", err)
		default:
			return grpcstatus.Errorf(codes.Internal, "replace blocking repair chat: %v", err)
		}
	}
	return nil
}

func isAuthorizedRepairReplacement(req *bossanovav1.StartChatRunHostRequest) bool {
	return strings.TrimPrefix(req.GetCommand(), "/") == "boss-repair" &&
		strings.HasPrefix(req.GetTitle(), "Repair: ") &&
		strings.TrimSpace(req.GetReplaceExistingReason()) != "" &&
		req.GetReplaceExistingObservedLastChatActivityAt() != nil
}

func parseAlreadyExistsAgentSessionID(msg string) string {
	const marker = "agent_session_id="
	idx := strings.Index(msg, marker)
	if idx == -1 {
		return ""
	}
	rest := strings.TrimSpace(msg[idx+len(marker):])
	for i, r := range rest {
		if r == ')' || r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' {
			return rest[:i]
		}
	}
	return rest
}

// WaitChatRun blocks until the agent run identified by agent_session_id
// completes (CompleteAgentRun signals the run-completion channel),
// the WaitChatRunDeadline elapses, or the caller's context is cancelled.
// Cleanup of all per-run state happens before the call returns, regardless
// of which branch it took.
func (s *HostServiceServer) WaitChatRun(ctx context.Context, req *bossanovav1.WaitChatRunHostRequest) (*bossanovav1.WaitChatRunHostResponse, error) {
	agentSessionID := req.GetAgentSessionId()
	if agentSessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "agent_session_id is required")
	}

	s.runMu.Lock()
	run, ok := s.runCompletion[agentSessionID]
	s.runMu.Unlock()
	if !ok {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition, "no active chat run for agent_session_id %s", agentSessionID)
	}
	if run.completionMode == "" {
		run.completionMode = chatRunCompletionHook
	}

	deadline := time.NewTimer(s.waitChatRunDeadline)
	defer deadline.Stop()

	// Opt-in idle watchdog (BOS-153). When idle_fail_after_seconds > 0, sample
	// the chat tracker every watchdogPollInterval; if the run stays displaceable
	// (continuously IDLE past idleWindow, a repair-owned QUESTION stuck past its
	// window, or STOPPED) the watchdog fast-fails the run and tears the dead
	// pane down instead of burning the full 30m deadline. A nil channel (the
	// disabled default) simply never fires, leaving the legacy behaviour intact.
	var watchdogTick <-chan time.Time
	var idleWindow time.Duration
	var watchdogPol displacePolicy
	if secs := req.GetIdleFailAfterSeconds(); secs > 0 {
		idleWindow = time.Duration(secs) * time.Second
		watchdogPol = displacePolicy{MinIdle: idleWindow, QuestionIdle: repairDisplaceQuestionIdle}
		ticker := time.NewTicker(watchdogPollInterval)
		defer ticker.Stop()
		watchdogTick = ticker.C
	}

	for {
		select {
		case res := <-run.done:
			providerSessionID := s.providerSessionIDForAgentSession(ctx, agentSessionID)
			s.removeRunHookBestEffort(agentSessionID)
			s.cleanupRun(agentSessionID)
			return &bossanovav1.WaitChatRunHostResponse{ExitError: res.exitError, ProviderSessionId: providerSessionID}, nil
		case <-watchdogTick:
			// Not displaceable yet (WORKING, young, tracker cold): keep
			// waiting. WORKING resets naturally as LastOutputAt advances.
			if s.chatDisplaceable(agentSessionID, time.Time{}, watchdogPol, time.Now()) != nil {
				continue
			}
			// Re-evaluate once more immediately to close the snapshot race
			// between this tick and now; a fresh err means DO NOT fire.
			if s.chatDisplaceable(agentSessionID, time.Time{}, watchdogPol, time.Now()) != nil {
				continue
			}
			if res, ok := watchdogCompletionAvailable(run); ok {
				providerSessionID := s.providerSessionIDForAgentSession(ctx, agentSessionID)
				s.removeRunHookBestEffort(agentSessionID)
				s.cleanupRun(agentSessionID)
				return &bossanovav1.WaitChatRunHostResponse{ExitError: res.exitError, ProviderSessionId: providerSessionID}, nil
			}
			log.Warn().
				Str("agent_session", agentSessionID).
				Dur("idle_window", idleWindow).
				Msg("WaitChatRun: idle watchdog fired; agent went idle without completing; reclaiming pane")
			s.runMu.Lock()
			sessionID := s.runSessionByID[agentSessionID]
			s.runMu.Unlock()
			if s.lifecycle != nil {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), watchdogReclaimTimeout)
				if _, err := s.lifecycle.ReclaimRepairChat(cleanupCtx, sessionID, agentSessionID, "watchdog: agent went idle without completing"); err != nil {
					// Best-effort: a reclaim error (e.g. non-repair title) is
					// logged and does NOT change the outcome; the run still
					// fails and the pane is left for the replace path.
					log.Warn().Err(err).
						Str("agent_session", agentSessionID).
						Msg("WaitChatRun: watchdog pane reclaim failed; failing run anyway")
				}
				cancel()
			}
			providerSessionID := s.providerSessionIDForAgentSession(ctx, agentSessionID)
			s.removeRunHookBestEffort(agentSessionID)
			s.cleanupRun(agentSessionID)
			return &bossanovav1.WaitChatRunHostResponse{
				ExitError:         fmt.Sprintf("agent went idle without completing (no output for %s; watchdog)", idleWindow),
				ProviderSessionId: providerSessionID,
			}, nil
		case <-deadline.C:
			// Surface the deadline expiry so operators can correlate a
			// synthesised exit_error with later events (eg. a Stop POST
			// arriving after cleanup that gets a 404). Without this log the
			// 404 reads as an unexplained anomaly.
			log.Warn().
				Str("agent_session", agentSessionID).
				Dur("deadline", s.waitChatRunDeadline).
				Msg("WaitChatRun: agent run did not signal completion within deadline; synthesising exit_error")
			providerSessionID := s.providerSessionIDForAgentSession(ctx, agentSessionID)
			s.removeRunHookBestEffort(agentSessionID)
			s.cleanupRun(agentSessionID)
			return &bossanovav1.WaitChatRunHostResponse{
				ExitError:         waitChatRunNoSignalMessage(run.completionMode, fmt.Sprintf("deadline %s elapsed", s.waitChatRunDeadline)),
				ProviderSessionId: providerSessionID,
			}, nil
		case <-ctx.Done():
			providerSessionID := s.providerSessionIDForAgentSession(ctx, agentSessionID)
			s.removeRunHookBestEffort(agentSessionID)
			s.cleanupRun(agentSessionID)
			if run.completionMode == chatRunCompletionExplicit {
				return &bossanovav1.WaitChatRunHostResponse{
					ExitError:         waitChatRunNoSignalMessage(run.completionMode, ctx.Err().Error()),
					ProviderSessionId: providerSessionID,
				}, nil
			}
			return nil, grpcstatus.FromContextError(ctx.Err()).Err()
		}
	}
}

func watchdogCompletionAvailable(run activeChatRun) (completionResult, bool) {
	select {
	case res := <-run.done:
		return res, true
	default:
		return completionResult{}, false
	}
}

func (s *HostServiceServer) providerSessionIDForAgentSession(ctx context.Context, agentSessionID string) string {
	if s.agentChats == nil || agentSessionID == "" {
		return ""
	}
	chat, err := s.agentChats.GetByAgentSessionID(ctx, agentSessionID)
	if err != nil || chat == nil || chat.ProviderSessionID == nil {
		if err != nil {
			log.Warn().Err(err).Str("agent_session_id", agentSessionID).Msg("provider session id lookup failed")
		}
	} else if *chat.ProviderSessionID != "" {
		return *chat.ProviderSessionID
	}
	if chat == nil || chat.AgentName != "codex" || chat.CreatedAt.IsZero() || s.sessionStore == nil {
		return ""
	}
	client, ok := s.agentClients[chat.AgentName]
	if !ok || client == nil {
		return ""
	}
	sess, err := s.sessionStore.Get(ctx, chat.SessionID)
	if err != nil || sess == nil || sess.WorktreePath == "" {
		if err != nil {
			log.Warn().Err(err).Str("session_id", chat.SessionID).Msg("provider session id discovery could not load session")
		}
		return ""
	}

	discoveryCtx, cancel := context.WithTimeout(context.Background(), waitChatRunProviderIDDiscoveryTimeout)
	defer cancel()
	resp, err := client.ResolveInteractiveSessionID(discoveryCtx, &bossanovav1.ResolveInteractiveSessionIDRequest{
		WorkDir:             sess.WorktreePath,
		RequestedSessionId:  chat.AgentSessionID,
		ChatCreatedAt:       timestamppb.New(chat.CreatedAt),
		AllowLegacyBackfill: true,
	})
	if err != nil {
		log.Warn().Err(err).
			Str("agent_session_id", chat.AgentSessionID).
			Msg("provider session id discovery failed before WaitChatRun response")
		return ""
	}
	if resp == nil || !resp.GetFound() || resp.GetSessionId() == "" {
		if resp != nil && resp.GetAmbiguous() {
			log.Warn().
				Str("agent_session_id", chat.AgentSessionID).
				Str("reason", resp.GetReason()).
				Msg("provider session id discovery ambiguous before WaitChatRun response")
		}
		return ""
	}
	providerID := resp.GetSessionId()
	if err := s.agentChats.UpdateProviderSessionID(discoveryCtx, chat.AgentSessionID, &providerID); err != nil {
		log.Warn().Err(err).
			Str("agent_session_id", chat.AgentSessionID).
			Str("provider_session_id", providerID).
			Msg("persist provider session id before WaitChatRun response failed")
		return ""
	}
	return providerID
}

func waitChatRunNoSignalMessage(mode chatRunCompletionMode, reason string) string {
	if mode == chatRunCompletionExplicit {
		return fmt.Sprintf("interactive chat did not report completion before wait deadline: %s", reason)
	}
	return fmt.Sprintf("agent run did not signal completion within %s", reason)
}

func (s *HostServiceServer) ReclaimRepairChat(ctx context.Context, req *bossanovav1.ReclaimRepairChatHostRequest) (*bossanovav1.ReclaimRepairChatHostResponse, error) {
	if s.lifecycle == nil {
		return nil, grpcstatus.Error(codes.FailedPrecondition, "lifecycle not configured")
	}
	sessionID := req.GetSessionId()
	if sessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "session_id is required")
	}
	agentSessionID := req.GetAgentSessionId()
	if agentSessionID == "" {
		return nil, grpcstatus.Error(codes.InvalidArgument, "agent_session_id is required")
	}
	if s.repairChatRunStillActive(sessionID, agentSessionID) {
		return nil, grpcstatus.Errorf(codes.FailedPrecondition,
			"reclaim repair chat: agent_session_id %s is still active for session %s", agentSessionID, sessionID)
	}
	// BOS-153: gate the reclaim on sampled tracker evidence, not the
	// pipe-pane log mtime. The reclaim path has no earlier idle snapshot
	// (the repair plugin only has the chat id here), so pass the zero time
	// to skip the snapshot check; the tracker's own LastOutputAt age is the
	// evidence. Repair chats are unattended, so a long-stuck QUESTION is
	// also displaceable.
	if err := s.chatDisplaceable(agentSessionID, time.Time{}, displacePolicy{MinIdle: repairDisplaceMinIdle, QuestionIdle: repairDisplaceQuestionIdle}, time.Now()); err != nil {
		return nil, err
	}
	reason := req.GetReason()
	if reason == "" {
		reason = "reclaimed stale repair chat"
	}

	res, err := s.lifecycle.ReclaimRepairChat(ctx, sessionID, agentSessionID, reason)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrRepairChatNotReclaimable),
			errors.Is(err, session.ErrRepairChatSessionMismatch),
			errors.Is(err, session.ErrRepairChatActive):
			return nil, grpcstatus.Errorf(codes.FailedPrecondition, "reclaim repair chat: %v", err)
		default:
			return nil, grpcstatus.Errorf(codes.Internal, "reclaim repair chat: %v", err)
		}
	}
	return &bossanovav1.ReclaimRepairChatHostResponse{
		Reclaimed:       res.Reclaimed,
		TmuxSessionName: res.TmuxSessionName,
	}, nil
}

func (s *HostServiceServer) repairChatRunStillActive(sessionID, agentSessionID string) bool {
	var activeAgentName, indexedAgentName string

	s.runMu.Lock()
	if cur, ok := s.activeRuns[sessionID]; ok && cur.agentSessionID == agentSessionID {
		activeAgentName = cur.agentName
	}
	indexedAgentName = s.agentSessionByID[agentSessionID]
	s.runMu.Unlock()

	if activeAgentName != "" || indexedAgentName != "" {
		return true
	}
	return false
}

// removeRunHookBestEffort asks the agent plugin to delete the run-scoped
// Stop-hook entry from the worktree's settings.local.json now that the run
// has finished. Best-effort: on any failure the (now harmless — the hook
// endpoint treats unknown runs as a 200 no-op) entry simply lingers until the
// next run's write-time sweep removes it. MUST run before cleanupRun, which
// deletes the runSessionByID entry this reads.
//
// Agent identity is resolved via sess.AgentName (from the session store) rather
// than agentSessionByID, because both completion paths (CompleteAgentRun and
// SignalRunComplete) delete agentSessionByID before WaitChatRun's done-branch
// can acquire runMu. runSessionByID intentionally survives as a tombstone until
// cleanupRun, so it is safe to read here.
func (s *HostServiceServer) removeRunHookBestEffort(agentSessionID string) {
	s.runMu.Lock()
	sessionID := s.runSessionByID[agentSessionID]
	s.runMu.Unlock()
	if sessionID == "" || s.sessionStore == nil {
		return
	}
	// Detach from the caller ctx: WaitChatRun's ctx-cancel branch passes an
	// already-cancelled context, but we still want this cleanup write to land.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := s.sessionStore.Get(ctx, sessionID)
	if err != nil || sess == nil || sess.WorktreePath == "" || sess.AgentName == "" {
		return
	}
	client, ok := s.agentClients[sess.AgentName]
	if !ok || client == nil {
		return
	}
	if _, err := client.RemoveAgentRunHook(ctx, &bossanovav1.RemoveAgentRunHookRequest{
		WorkDir:        sess.WorktreePath,
		AgentSessionId: agentSessionID,
	}); err != nil {
		log.Warn().Err(err).Str("agent_session", agentSessionID).
			Msg("WaitChatRun: remove run hook failed; stale entry will be swept on next run")
	}
}

// cleanupRun deletes all per-run state for agentSessionID under runMu
// and clears the IsRepairing flag for the originating session. Called by
// WaitChatRun on every exit path so a future StartChatRun isn't
// shadowed by stale tokens / channels left over from the previous run.
//
// Idempotent: missing entries in any map are silently skipped, so a
// duplicate cleanup (eg. one from CompleteAgentRun's matching-session
// branch and one from WaitChatRun's exit) doesn't double-fault.
func (s *HostServiceServer) cleanupRun(agentSessionID string) {
	s.runMu.Lock()
	sessionID := s.runSessionByID[agentSessionID]
	delete(s.runCompletion, agentSessionID)
	delete(s.runHookTokens, agentSessionID)
	delete(s.runSessionByID, agentSessionID)
	delete(s.agentSessionByID, agentSessionID)
	if sessionID != "" {
		if cur, ok := s.activeRuns[sessionID]; ok && cur.agentSessionID == agentSessionID {
			delete(s.activeRuns, sessionID)
		}
	}
	s.runMu.Unlock()

	// Clear the IsRepairing flag for the originating session — outside
	// runMu, mirroring CompleteAgentRun's locking discipline so we never
	// nest the daemon-internal mutex inside DisplayTracker's lock.
	// Idempotent if CompleteAgentRun already cleared the flag on a
	// successful Stop POST.
	if sessionID != "" && s.displayTracker != nil {
		s.displayTracker.SetRepairing(sessionID, false)
	}
}

func (s *HostServiceServer) signalAndCleanupReplacedRun(agentSessionID string) {
	s.runMu.Lock()
	if run, ok := s.runCompletion[agentSessionID]; ok {
		select {
		case run.done <- completionResult{exitError: "chat run replaced by newer repair attempt"}:
		default:
		}
	}
	s.runMu.Unlock()

	s.cleanupRun(agentSessionID)
}

func hostServiceStartChatRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.StartChatRunHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).StartChatRun(ctx, req)
}

func hostServiceWaitChatRunHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.WaitChatRunHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).WaitChatRun(ctx, req)
}

func hostServiceReclaimRepairChatHandler(srv any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	req := new(bossanovav1.ReclaimRepairChatHostRequest)
	if err := dec(req); err != nil {
		return nil, err
	}
	return srv.(hostServiceHandler).ReclaimRepairChat(ctx, req)
}
