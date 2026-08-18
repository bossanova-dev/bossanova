// Package tuitest provides end-to-end test infrastructure for the Boss TUI.
// It includes a mock daemon, test harness, and integration helpers that
// allow agents to programmatically drive and verify TUI behavior.
package tuitest

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/models"
	"github.com/recurser/bossalib/socketauth"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var socketCounter atomic.Int64

// cloneMsg deep-copies a protobuf message, preserving its concrete type. It
// wraps proto.Clone (which returns the proto.Message interface) with a checked
// assertion so call sites stay free of unchecked type assertions
// (forcetypeassert). Clone always returns the same dynamic type as its input,
// so the comma-ok never fails in practice.
func cloneMsg[T proto.Message](m T) T {
	c, _ := proto.Clone(m).(T)
	return c
}

// cloneMsgs deep-copies a slice of protobuf messages: a fresh backing array
// whose every element is a cloneMsg of the corresponding input. A nil or empty
// input yields an empty, non-nil slice.
//
// Deep-clone is the contract for this daemon's call-log accessors, and this
// helper is where that contract lives. A test's whole job is to poke at what it
// was handed — read a recorded request, mutate it to build the next assertion,
// reuse the slice for the next tick — and none of that may reach mock state.
// An accessor that hands back the stored pointers lets a test corrupt the very
// record the next assertion reads, and the corruption shows up as an unrelated
// failure somewhere downstream. Clone on the read side and that is impossible.
//
// Not every accessor here has been brought onto this contract yet: the note
// accessors (CreateNoteCalls, ListNoteCalls, UpdateNoteCalls) still hand back a
// shallow `copy`, so their elements remain aliased to mock state. That is a
// known gap, not a second sanctioned shape — a NEW accessor goes through this
// helper.
func cloneMsgs[T proto.Message](in []T) []T {
	out := make([]T, 0, len(in))
	for _, m := range in {
		out = append(out, cloneMsg(m))
	}
	return out
}

// MockDaemon is a minimal ConnectRPC server that implements the DaemonService
// interface with in-memory data. Only the RPCs actually used by the TUI are
// implemented; the rest return Unimplemented.
type MockDaemon struct {
	mu              sync.RWMutex
	repos           []*pb.Repo
	sessions        []*pb.Session
	chats           []*pb.ClaudeChat
	chatStatuses    map[string]*pb.ChatStatusEntry    // keyed by agent_session_id
	sessionStatuses map[string]*pb.SessionStatusEntry // keyed by session_id
	cronJobs        map[string]*pb.CronJob            // keyed by cron job ID
	githubCallbacks map[string]*pb.GithubCallback     // keyed by callback ID
	prs             map[string][]*pb.PRSummary        // keyed by repo ID
	trackerIssues   map[string][]*pb.TrackerIssue     // keyed by repo ID

	// broadcasts and broadcastSubscriptions are keyed by their own ids. Neither
	// stored message carries a body: the daemon clears Broadcast.message on
	// every read surface and BroadcastSubscription has no body field, so the
	// mock holds exactly what a real read would return.
	broadcasts             map[string]*pb.Broadcast
	broadcastSubscriptions map[string]*pb.BroadcastSubscription

	// notes is the in-memory note store, keyed by note id. Unlike the broadcast
	// and callback stores, the stored value DOES carry the body: a note body is
	// not a secret, it is the record, so every read surface returns it.
	notes map[string]*pb.Note

	// checkSnapshots is the per-session CI check snapshot store, keyed by
	// session id and held newest-first — the order the real daemon's
	// RecentBySession query returns and the order `boss session checks`
	// renders.
	checkSnapshots map[string][]*pb.CheckSnapshot

	// lastCreateSession records the most recent CreateSession request so tests
	// can assert on what the TUI sent (e.g. that filter-narrowed selection uses
	// the correct original-index PR).
	lastCreateSession *pb.CreateSessionRequest

	// createSessionScript, when non-empty, is streamed by CreateSession instead
	// of the default Unimplemented, with createSessionFrameDelay between
	// consecutive frames. Installed by SetCreateSessionScript.
	createSessionScript     []*pb.CreateSessionResponse
	createSessionFrameDelay time.Duration

	// updateSessionCalls records every UpdateSession request so tests can
	// assert the TUI sent the expected title / field updates.
	updateSessionCalls []*pb.UpdateSessionRequest

	// switchSessionAccountCalls records every SwitchSessionAccount request so
	// tests can assert the TUI/CLI sent the expected session/account/force.
	switchSessionAccountCalls []*pb.SwitchSessionAccountRequest

	// Channel-backed AttachSession streaming. Tests push events via
	// PushOutputLine / PushStateChange / PushSessionEnded; the AttachSession
	// RPC reads from the per-session channel and forwards to the stream.
	attachEvents map[string]chan *pb.AttachSessionResponse
	attachCalls  []*pb.AttachSessionRequest

	// attachActive counts live AttachSession streams per session ID. Incremented
	// at RPC entry, decremented on return (see AttachSession). SessionAttached
	// reports whether any stream is live so the BOS-217 daemon-op pushers can
	// refuse to push to a session nobody is watching (an actionable error rather
	// than a silent buffer). Guarded by mu.
	attachActive map[string]int

	// validateRepoPathResp, when non-nil, overrides the default ValidateRepoPath
	// response (IsValid=true). Lets tests exercise RepoAddView's error-path.
	validateRepoPathResp *pb.ValidateRepoPathResponse
	validateRepoPathErr  error

	// registerRepoCalls records every RegisterRepo request so tests can assert
	// the TUI sent the expected display name / path / setup script.
	registerRepoCalls []*pb.RegisterRepoRequest

	// notifyAuthChangeCalls records the action ("login" / "logout") of every
	// NotifyAuthChange request so tests can assert the TUI notified the
	// daemon after the user authenticated or signed out.
	notifyAuthChangeCalls []string

	// cronJobCounter is used to generate deterministic cron job IDs.
	cronJobCounter int

	// createCronJobCalls records every CreateCronJob request.
	createCronJobCalls []*pb.CreateCronJobRequest

	// updateCronJobCalls records every UpdateCronJob request.
	updateCronJobCalls []*pb.UpdateCronJobRequest

	// deleteCronJobCalls records every DeleteCronJob id.
	deleteCronJobCalls []string

	// runCronJobNowCalls records every RunCronJobNow id.
	runCronJobNowCalls []string

	// githubCallbackCounter generates deterministic callback IDs.
	githubCallbackCounter int

	// createGithubCallbackCalls records every CreateGithubCallback request.
	createGithubCallbackCalls []*pb.CreateGithubCallbackRequest

	// listGithubCallbackCalls records every ListGithubCallbacks request.
	listGithubCallbackCalls []*pb.ListGithubCallbacksRequest

	// deleteGithubCallbackCalls records every DeleteGithubCallback id.
	deleteGithubCallbackCalls []string

	// broadcastCounter generates deterministic broadcast IDs.
	broadcastCounter int

	// sendBroadcastCalls records every SendBroadcast request. The request
	// carries the message body, which is exactly what lets a test assert the
	// CLI sent it while no output surface echoed it back.
	sendBroadcastCalls []*pb.SendBroadcastRequest

	// listBroadcastCalls records every ListBroadcasts request.
	listBroadcastCalls []*pb.ListBroadcastsRequest

	// deleteBroadcastCalls records every DeleteBroadcast id.
	deleteBroadcastCalls []string

	// broadcastDeliveries holds the resolved targets per broadcast id, keyed
	// separately from broadcasts because ListBroadcasts returns broadcasts
	// while the target_chat_id filter reaches through the delivery rows.
	broadcastDeliveries map[string][]*pb.BroadcastDelivery

	// broadcastSubscriptionCounter generates deterministic subscription IDs.
	broadcastSubscriptionCounter int

	// createBroadcastSubscriptionCalls records every CreateBroadcastSubscription request.
	createBroadcastSubscriptionCalls []*pb.CreateBroadcastSubscriptionRequest

	// listBroadcastSubscriptionCalls records every ListBroadcastSubscriptions request.
	listBroadcastSubscriptionCalls []*pb.ListBroadcastSubscriptionsRequest

	// deleteBroadcastSubscriptionCalls records every DeleteBroadcastSubscription id.
	deleteBroadcastSubscriptionCalls []string

	// noteCounter generates deterministic note IDs.
	noteCounter int
	// noteClock ticks once per note write (create or update) and drives the
	// deterministic created_at/updated_at stamps, so a test can assert on exact
	// timestamps and on ordering without a real clock.
	noteClock int

	// createNoteCalls records every CreateNote request. The request carries the
	// body, repo and provenance the CLI resolved, which is what lets a test
	// assert the CLI defaulted them correctly.
	createNoteCalls []*pb.CreateNoteRequest
	// listNoteCalls records every ListNotes request, so a test can prove the
	// filters were pushed to the daemon rather than applied client-side.
	listNoteCalls []*pb.ListNotesRequest
	// updateNoteCalls records every UpdateNote request. Its optional body/tags
	// fields distinguish "leave alone" from "replace", so the requests are kept
	// intact rather than reduced to their effect.
	updateNoteCalls []*pb.UpdateNoteRequest
	// deleteNoteCalls records every DeleteNote id.
	deleteNoteCalls []string

	// resolvedContext, when non-nil, is what ResolveContext returns. Seeded via
	// SetResolvedContext; nil means "this working directory is not inside any
	// registered repo or session", which is an empty response, not an error.
	resolvedContext *pb.ResolveContextResponse

	// runCronJobNowMode controls RunCronJobNow behaviour.
	// "" or "alwaysRun" → return a synthesized Session.
	// "alwaysSkip" → return a skipped response with runCronJobNowSkipReason.
	runCronJobNowMode       string
	runCronJobNowSkipReason string

	// accounts is the in-memory account registry, keyed by account ID.
	accounts map[string]*pb.Account
	// accountCredentials mirrors the daemon's keyring: credential blobs keyed by
	// account ID. Tests assert a removed account's credential is purged too.
	accountCredentials map[string][]byte
	// accountCounter generates deterministic account IDs.
	accountCounter int
	// addAccountCalls records every AddAccount request.
	addAccountCalls []*pb.AddAccountRequest
	// updateAccountCalls records every UpdateAccount request.
	updateAccountCalls []*pb.UpdateAccountRequest
	// removeAccountCalls records every RemoveAccount id.
	removeAccountCalls []string
	// testAccountCalls records every TestAccount id.
	testAccountCalls []string

	// agents controls what ListAgents returns. Tests can override via
	// SetAgents to drive multi-agent UI (provider picker, settings render).
	agents []*pb.AgentInfo

	socketPath string
	httpServer *http.Server
	listener   net.Listener

	archiveDelay  time.Duration
	archiveError  string
	chatListDelay time.Duration
	chatListError string

	// chatStatusesErrorCode/chatStatusesErrorMsg, when the message is non-empty,
	// make GetChatStatuses fail with that connect error instead of answering.
	// A status read that cannot be served is the case `boss chats` has to
	// degrade correctly — loudly under --json, visibly under the table — and
	// only an injected failure can produce it here.
	chatStatusesErrorCode connect.Code
	chatStatusesErrorMsg  string

	// mergeSessionCalls records the id of every MergeSession request. This is
	// what lets a test prove a declined confirmation issued NO merge RPC —
	// asserting only on stdout would pass even if the merge had gone through.
	mergeSessionCalls []string
	// mergeDetail is echoed as MergeSessionResponse.detail. Empty by default so
	// the no-detail output path is the default shape.
	mergeDetail string
	// mergeErrorCode/mergeErrorMsg, when mergeErrorMsg is non-empty, make
	// MergeSession fail with that connect error instead of merging — used to
	// drive the daemon's `merge blocked: gate=<slug>; ...` refusal through the
	// CLI without reimplementing the gate here.
	mergeErrorCode connect.Code
	mergeErrorMsg  string
	// mergeResponseState, when non-zero, is the state MergeSession reports in
	// its response instead of MERGED. The real daemon can genuinely answer a
	// successful merge with a pre-merge state — its handler reads the session
	// before its own deferred display refresh lands the Merged transition — and
	// the mock's eager transition below cannot express that, so a test asserting
	// the CLI passes state through verbatim would only ever be agreeing with the
	// mock. Setting this drives the lagging shape.
	mergeResponseState pb.SessionState

	// recordChatCalls records every RecordChat request. `boss chat new` mints the
	// agent_session_id itself, so the request is the only place a test can see
	// what id, title, agent and resume flag actually reached the daemon —
	// stdout shows what the CLI chose to print, which is a different question.
	recordChatCalls []*pb.RecordChatRequest
	// recordChatResponse, when non-nil, is returned verbatim by RecordChat
	// instead of the minted row. The variant that matters is one whose
	// tmux_session_name is empty: the real daemon genuinely persists a chat row
	// with no live agent behind it on a host without tmux, and the mock's
	// unconditional backfill below cannot express that, so a test asserting the
	// CLI rejects it would only be agreeing with the mock.
	recordChatResponse *pb.ClaudeChat
	// sendChatMessageCalls records every SendChatMessage request, which is the
	// only place the wake/submit flags are observable — the response says
	// nothing about them.
	sendChatMessageCalls []*pb.SendChatMessageRequest
	// reportChatStatusCalls records every ReportChatStatus request. The RPC's
	// response is an empty message, so the request is the ONLY place a test can
	// see what the heartbeat actually reported — which chats, in which status.
	// Without it, a heartbeat that reported nothing and one that reported the
	// wrong status are the same observation (BOS-811).
	reportChatStatusCalls []*pb.ReportChatStatusRequest
	// updateChatTitleCalls records every UpdateChatTitle request. The handler
	// below also persists the title onto the stored chat, so the stored state
	// answers "did the rename land"; this log answers the separate question of
	// whether the caller asked at all, and with what — the two come apart for a
	// title aimed at a chat this daemon never saw, which is a deliberate no-op.
	updateChatTitleCalls []*pb.UpdateChatTitleRequest

	// rpcStall is the current wedge window applied to unary handlers by
	// rpcStallInterceptor. See SetRPCStall / SetRPCStallFor in harness.go.
	rpcStall rpcStall
	// rpcStallChanged is closed (and replaced) on every stall change so a
	// handler already parked inside a window re-reads it instead of sleeping out
	// a stall the scenario has since cleared. Lazily created under m.mu.
	rpcStallChanged chan struct{}
}

// rpcStall describes one wedge window: block every matching unary handler until
// until, where an empty procedure means every procedure. The zero value is "not
// stalled" (until is the zero time, which is always in the past).
type rpcStall struct {
	until     time.Time
	procedure string
}

// blocks reports whether this window still applies to procedure at now.
//
// procedure is the full Connect path ("/bossanova.v1.DaemonService/RecordChat"),
// so a scoped window matches on the method segment EXACTLY: a suffix match on
// "/" + s.procedure. A substring match would make the short name "Chat" wedge
// RecordChat, ListChats, AddChat and DeleteChat together, which is not what a
// scenario naming one call means.
func (s rpcStall) blocks(procedure string, now time.Time) bool {
	if !now.Before(s.until) {
		return false
	}
	return s.procedure == "" || strings.HasSuffix(procedure, "/"+s.procedure)
}

// rpcStallInterceptor wedges the mock daemon on demand: it makes matching unary
// handlers accept the request and then simply not answer until the stall window
// closes, which is the daemon failure BOS-723's client-side bound exists to
// survive. It is an interceptor rather than an edit to ~60 handlers so a wedge
// covers every unary RPC, including ones added later.
//
// WrapStreamingHandler is a deliberate pass-through: AttachSession is long-lived
// by nature and boss does not bound it, so stalling it would model a failure the
// TUI is not meant to recover from and would break every scenario that wedges
// the daemon while attached.
type rpcStallInterceptor struct{ m *MockDaemon }

func (i rpcStallInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		i.m.waitOutRPCStall(ctx, req.Spec().Procedure)
		return next(ctx, req)
	}
}

func (i rpcStallInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i rpcStallInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// waitOutRPCStall blocks until the current stall window stops applying to
// procedure, the window is cleared or replaced, or ctx is done — whichever comes
// first. It never sleeps past a cancelled context: a client that has already hit
// its own deadline must not keep a handler goroutine parked.
func (m *MockDaemon) waitOutRPCStall(ctx context.Context, procedure string) {
	for {
		m.mu.Lock()
		stall := m.rpcStall
		changed := m.rpcStallChangedLocked()
		m.mu.Unlock()

		now := time.Now()
		if !stall.blocks(procedure, now) {
			return
		}
		timer := time.NewTimer(stall.until.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-changed:
			// The stall was cleared or replaced; re-read it rather than
			// serving an answer the scenario may still want withheld.
			timer.Stop()
		case <-timer.C:
			return
		}
	}
}

// rpcStallChangedLocked returns the current change-signal channel, creating it
// on first use. Callers must hold m.mu.
func (m *MockDaemon) rpcStallChangedLocked() chan struct{} {
	if m.rpcStallChanged == nil {
		m.rpcStallChanged = make(chan struct{})
	}
	return m.rpcStallChanged
}

// StartMockDaemon binds a Unix socket and serves the mock DaemonService.
// Returns the daemon and a stop() that closes the server and removes the socket.
func StartMockDaemon(socketPath string) (*MockDaemon, func() error, error) {
	_ = removeSocket(socketPath)

	// Generate the socket auth token co-located with the socket so client.NewLocal
	// (which reads it from the same directory) authenticates transparently, just
	// as it does against the real daemon.
	token, err := socketauth.LoadOrCreateToken(socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load socket auth token: %w", err)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, nil, fmt.Errorf("listen unix: %w", err)
	}
	m := &MockDaemon{
		socketPath:             socketPath,
		listener:               ln,
		cronJobs:               make(map[string]*pb.CronJob),
		githubCallbacks:        make(map[string]*pb.GithubCallback),
		broadcasts:             make(map[string]*pb.Broadcast),
		broadcastDeliveries:    make(map[string][]*pb.BroadcastDelivery),
		broadcastSubscriptions: make(map[string]*pb.BroadcastSubscription),
		notes:                  make(map[string]*pb.Note),
		checkSnapshots:         make(map[string][]*pb.CheckSnapshot),
		accounts:               make(map[string]*pb.Account),
		accountCredentials:     make(map[string][]byte),
		prs:                    make(map[string][]*pb.PRSummary),
		trackerIssues:          make(map[string][]*pb.TrackerIssue),
		attachEvents:           make(map[string]chan *pb.AttachSessionResponse),
		attachActive:           make(map[string]int),
	}
	mux := http.NewServeMux()
	// Auth first, then the stall: an unauthenticated request should be refused
	// immediately rather than held for the length of a wedge window.
	path, handler := bossanovav1connect.NewDaemonServiceHandler(m, connect.WithInterceptors(
		socketauth.NewServerInterceptor(token),
		rpcStallInterceptor{m: m},
	))
	mux.Handle(path, handler)
	m.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = m.httpServer.Serve(ln) }()
	stop := func() error {
		err := m.httpServer.Close()
		_ = removeSocket(socketPath)
		_ = os.Remove(socketauth.TokenPath(socketPath))
		return err
	}
	return m, stop, nil
}

// NewMockDaemon starts a mock daemon on a temporary Unix socket.
// The server is cleaned up when the test finishes.
func NewMockDaemon(t *testing.T) *MockDaemon {
	t.Helper()

	// Use /tmp directly — t.TempDir() paths can exceed the 104-char macOS Unix socket limit.
	// Include PID so parallel test binaries (tuitest + clitest run side-by-side under
	// `go test ./...`) don't collide on `/tmp/boss-tuitest-1.sock`: each package gets
	// its own counter starting at 1, so without the PID qualifier the second binary
	// would remove and rebind the first binary's still-active socket.
	//
	// Each daemon gets its own *directory*, not just its own socket filename.
	// socketauth.TokenPath derives the token file from filepath.Dir(socketPath),
	// so daemons sharing a directory share one token file — and every stop()
	// removes it, pulling the token out from under peers that are still serving.
	// That surfaced as unrelated tests failing with "socket auth token missing or
	// stale" whenever runs overlapped. A per-daemon directory keeps the token
	// private, so cleanup can only ever affect its own daemon. The name stays
	// short to stay clear of the socket-length limit.
	dir := filepath.Join("/tmp", fmt.Sprintf("boss-tuitest-%d-%d", os.Getpid(), socketCounter.Add(1)))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create mock daemon dir: %v", err)
	}
	socketPath := filepath.Join(dir, "d.sock")
	m, stop, err := StartMockDaemon(socketPath)
	if err != nil {
		t.Fatalf("%v", err)
	}
	t.Cleanup(func() {
		_ = stop()
		_ = os.RemoveAll(dir)
	})
	return m
}

// SocketPath returns the Unix socket path for the mock daemon.
func (m *MockDaemon) SocketPath() string {
	return m.socketPath
}

// --- Data store accessors ---

// AddRepo adds a repo to the mock daemon's in-memory store.
func (m *MockDaemon) AddRepo(repo *pb.Repo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repos = append(m.repos, repo)
}

// AddSession adds a session to the mock daemon's in-memory store.
func (m *MockDaemon) AddSession(sess *pb.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = append(m.sessions, sess)
}

// AddChat adds a claude chat to the mock daemon's in-memory store.
func (m *MockDaemon) AddChat(c *pb.ClaudeChat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chats = append(m.chats, c)
}

// AddChatStatus records a daemon-heartbeat status for a chat (keyed by
// agent_session_id), so GetChatStatuses can serve deterministic
// working/idle/question statuses in proof scenarios.
func (m *MockDaemon) AddChatStatus(e *pb.ChatStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chatStatuses == nil {
		m.chatStatuses = make(map[string]*pb.ChatStatusEntry)
	}
	m.chatStatuses[e.AgentSessionId] = e
}

// AddSessionStatus records a daemon-heartbeat status for a session (keyed by
// session_id), so GetSessionStatuses can serve deterministic aggregate statuses
// — and, for a session parked on an external event, the waiting_reason the home
// list renders on its own sub-row (BOS-668).
func (m *MockDaemon) AddSessionStatus(e *pb.SessionStatusEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionStatuses == nil {
		m.sessionStatuses = make(map[string]*pb.SessionStatusEntry)
	}
	m.sessionStatuses[e.SessionId] = e
}

// AddPRs adds pull request summaries for a repo to the mock daemon's in-memory store.
func (m *MockDaemon) AddPRs(repoID string, prs []*pb.PRSummary) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prs[repoID] = append(m.prs[repoID], prs...)
}

// AddTrackerIssues adds tracker (Linear) issues for a repo to the mock daemon's in-memory store.
func (m *MockDaemon) AddTrackerIssues(repoID string, issues []*pb.TrackerIssue) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.trackerIssues[repoID] = append(m.trackerIssues[repoID], issues...)
}

// LastCreateSession returns the most recent CreateSession request received
// by the mock, or nil if none was received.
func (m *MockDaemon) LastCreateSession() *pb.CreateSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastCreateSession
}

// UpdateSessionCalls returns a copy of every UpdateSession request recorded
// by the mock.
func (m *MockDaemon) UpdateSessionCalls() []*pb.UpdateSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateSessionRequest, len(m.updateSessionCalls))
	copy(out, m.updateSessionCalls)
	return out
}

// Sessions returns a copy of the current sessions.
func (m *MockDaemon) Sessions() []*pb.Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.Session, len(m.sessions))
	copy(out, m.sessions)
	return out
}

// Repos returns a copy of the current repos.
func (m *MockDaemon) Repos() []*pb.Repo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.Repo, len(m.repos))
	copy(out, m.repos)
	return out
}

// AddCronJob seeds the mock daemon with a cron job.
func (m *MockDaemon) AddCronJob(job *pb.CronJob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cronJobs[job.Id] = job
}

// AddGithubCallback seeds the mock daemon with a GitHub callback.
func (m *MockDaemon) AddGithubCallback(cb *pb.GithubCallback) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.githubCallbacks[cb.Id] = cb
}

// AddNote seeds the mock daemon with a note. The note is stored as given —
// tags are NOT re-normalised — so a test can seed the exact rows a real daemon
// would already hold.
func (m *MockDaemon) AddNote(n *pb.Note) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notes[n.GetId()] = cloneMsg(n)
}

// AddCheckSnapshots seeds a session's CI check snapshots. Pass them in the
// order the real daemon returns them — newest first — because
// ListCheckSnapshots truncates from the front to honour `limit`, exactly as the
// daemon's RecentBySession query does.
func (m *MockDaemon) AddCheckSnapshots(sessionID string, snaps ...*pb.CheckSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range snaps {
		m.checkSnapshots[sessionID] = append(m.checkSnapshots[sessionID], cloneMsg(s))
	}
}

// SetResolvedContext seeds what ResolveContext reports for any working
// directory. Either argument may be nil; passing two nils restores the default
// "not inside a registered repo or session" answer. Both arguments are cloned,
// as AddNote clones its note: a test that mutates a seeded repo afterwards is
// changing its own fixture, not the daemon's state.
func (m *MockDaemon) SetResolvedContext(repo *pb.Repo, session *pb.Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.resolvedContext = &pb.ResolveContextResponse{Repo: cloneMsg(repo), Session: cloneMsg(session)}
}

// Notes returns a copy of every stored note, ordered by id. Tests assert on
// the daemon's state here rather than on CLI output, so an update that printed
// the right thing but stored the wrong one cannot pass.
func (m *MockDaemon) Notes() []*pb.Note {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.notes))
	for id := range m.notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.Note, 0, len(ids))
	for _, id := range ids {
		out = append(out, cloneMsg(m.notes[id]))
	}
	return out
}

// CreateNoteCalls returns a copy of every CreateNote request.
func (m *MockDaemon) CreateNoteCalls() []*pb.CreateNoteRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.CreateNoteRequest, len(m.createNoteCalls))
	copy(out, m.createNoteCalls)
	return out
}

// ListNoteCalls returns a copy of every ListNotes request.
func (m *MockDaemon) ListNoteCalls() []*pb.ListNotesRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.ListNotesRequest, len(m.listNoteCalls))
	copy(out, m.listNoteCalls)
	return out
}

// UpdateNoteCalls returns a copy of every UpdateNote request.
func (m *MockDaemon) UpdateNoteCalls() []*pb.UpdateNoteRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateNoteRequest, len(m.updateNoteCalls))
	copy(out, m.updateNoteCalls)
	return out
}

// DeleteNoteCalls returns a copy of every DeleteNote id.
func (m *MockDaemon) DeleteNoteCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.deleteNoteCalls))
	copy(out, m.deleteNoteCalls)
	return out
}

// CreateGithubCallbackCalls returns a copy of every CreateGithubCallback request.
func (m *MockDaemon) CreateGithubCallbackCalls() []*pb.CreateGithubCallbackRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.CreateGithubCallbackRequest, len(m.createGithubCallbackCalls))
	copy(out, m.createGithubCallbackCalls)
	return out
}

// ListGithubCallbackCalls returns a copy of every ListGithubCallbacks request.
func (m *MockDaemon) ListGithubCallbackCalls() []*pb.ListGithubCallbacksRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.ListGithubCallbacksRequest, len(m.listGithubCallbackCalls))
	copy(out, m.listGithubCallbackCalls)
	return out
}

// DeleteGithubCallbackCalls returns a copy of every DeleteGithubCallback id.
func (m *MockDaemon) DeleteGithubCallbackCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.deleteGithubCallbackCalls))
	copy(out, m.deleteGithubCallbackCalls)
	return out
}

// SendBroadcastCalls returns a copy of every SendBroadcast request. These
// requests DO carry the message body — that is the point: a test asserts the
// CLI transmitted the body here while no output surface echoed it back.
func (m *MockDaemon) SendBroadcastCalls() []*pb.SendBroadcastRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.SendBroadcastRequest, len(m.sendBroadcastCalls))
	copy(out, m.sendBroadcastCalls)
	return out
}

// DeleteBroadcastCalls returns a copy of every DeleteBroadcast id, so a test
// can prove an idempotent `rm` of an unknown id still reached the daemon.
func (m *MockDaemon) DeleteBroadcastCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.deleteBroadcastCalls))
	copy(out, m.deleteBroadcastCalls)
	return out
}

// CreateBroadcastSubscriptionCalls returns a copy of every
// CreateBroadcastSubscription request. As with SendBroadcastCalls, these carry
// the registered body so a leak test has something real to contrast against.
func (m *MockDaemon) CreateBroadcastSubscriptionCalls() []*pb.CreateBroadcastSubscriptionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.CreateBroadcastSubscriptionRequest, len(m.createBroadcastSubscriptionCalls))
	copy(out, m.createBroadcastSubscriptionCalls)
	return out
}

// DeleteBroadcastSubscriptionCalls returns a copy of every
// DeleteBroadcastSubscription id.
func (m *MockDaemon) DeleteBroadcastSubscriptionCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.deleteBroadcastSubscriptionCalls))
	copy(out, m.deleteBroadcastSubscriptionCalls)
	return out
}

// CronJobs returns a snapshot of all cron jobs in the mock.
func (m *MockDaemon) CronJobs() map[string]*pb.CronJob {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*pb.CronJob, len(m.cronJobs))
	for k, v := range m.cronJobs {
		out[k] = v
	}
	return out
}

// CreateCronJobCallCount returns how many CreateCronJob calls were received.
func (m *MockDaemon) CreateCronJobCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.createCronJobCalls)
}

// CreateCronJobCalls returns a copy of every CreateCronJob request recorded.
func (m *MockDaemon) CreateCronJobCalls() []*pb.CreateCronJobRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.CreateCronJobRequest, len(m.createCronJobCalls))
	for i, req := range m.createCronJobCalls {
		out[i] = cloneMsg(req)
	}
	return out
}

// UpdateCronJobCalls returns a copy of every UpdateCronJob request recorded.
func (m *MockDaemon) UpdateCronJobCalls() []*pb.UpdateCronJobRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateCronJobRequest, len(m.updateCronJobCalls))
	copy(out, m.updateCronJobCalls)
	return out
}

// DeleteCronJobCallCount returns how many DeleteCronJob calls were received.
func (m *MockDaemon) DeleteCronJobCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.deleteCronJobCalls)
}

// RunCronJobNowCallCount returns how many RunCronJobNow calls were received.
func (m *MockDaemon) RunCronJobNowCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.runCronJobNowCalls)
}

// SeedAccount seeds the mock daemon with an account (and optional credential).
func (m *MockDaemon) SeedAccount(acct *pb.Account, credential []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[acct.Id] = acct
	if credential != nil {
		m.accountCredentials[acct.Id] = credential
	}
}

// Accounts returns a snapshot of all accounts in the mock.
func (m *MockDaemon) Accounts() map[string]*pb.Account {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*pb.Account, len(m.accounts))
	for k, v := range m.accounts {
		out[k] = v
	}
	return out
}

// AccountCredential returns the stored credential blob for an account, or nil.
func (m *MockDaemon) AccountCredential(id string) []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.accountCredentials[id]
}

// AddAccountCalls returns a copy of every AddAccount request recorded.
func (m *MockDaemon) AddAccountCalls() []*pb.AddAccountRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.AddAccountRequest, len(m.addAccountCalls))
	for i, req := range m.addAccountCalls {
		out[i] = cloneMsg(req)
	}
	return out
}

// UpdateAccountCalls returns a copy of every UpdateAccount request recorded.
func (m *MockDaemon) UpdateAccountCalls() []*pb.UpdateAccountRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.UpdateAccountRequest, len(m.updateAccountCalls))
	for i, req := range m.updateAccountCalls {
		out[i] = cloneMsg(req)
	}
	return out
}

// RemoveAccountCallCount returns how many RemoveAccount calls were received.
func (m *MockDaemon) RemoveAccountCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.removeAccountCalls)
}

// TestAccountCallCount returns how many TestAccount calls were received.
func (m *MockDaemon) TestAccountCallCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.testAccountCalls)
}

// SetRunCronJobNowMode configures RunCronJobNow behaviour.
// mode "alwaysSkip" returns a skipped response with skipReason populated.
// Any other value (including "") causes a synthesized Session to be returned.
func (m *MockDaemon) SetRunCronJobNowMode(mode string, skipReason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runCronJobNowMode = mode
	m.runCronJobNowSkipReason = skipReason
}

// --- DaemonServiceHandler implementation ---

func (m *MockDaemon) ListRepos(_ context.Context, _ *connect.Request[pb.ListReposRequest]) (*connect.Response[pb.ListReposResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return connect.NewResponse(&pb.ListReposResponse{Repos: m.repos}), nil
}

func (m *MockDaemon) ListSessions(_ context.Context, req *connect.Request[pb.ListSessionsRequest]) (*connect.Response[pb.ListSessionsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stateSet := make(map[pb.SessionState]bool, len(req.Msg.States))
	for _, st := range req.Msg.States {
		stateSet[st] = true
	}
	var out []*pb.Session
	for _, s := range m.sessions {
		if s.ArchivedAt != nil && !req.Msg.IncludeArchived {
			continue
		}
		if req.Msg.RepoId != nil && *req.Msg.RepoId != "" && s.RepoId != *req.Msg.RepoId {
			continue
		}
		if len(stateSet) > 0 && !stateSet[s.State] {
			continue
		}
		out = append(out, s)
	}
	return connect.NewResponse(&pb.ListSessionsResponse{Sessions: out}), nil
}

func (m *MockDaemon) GetSession(_ context.Context, req *connect.Request[pb.GetSessionRequest]) (*connect.Response[pb.GetSessionResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			return connect.NewResponse(&pb.GetSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) ArchiveSession(_ context.Context, req *connect.Request[pb.ArchiveSessionRequest]) (*connect.Response[pb.ArchiveSessionResponse], error) {
	// Snapshot the injected delay/error under the lock: SetArchiveDelay can be
	// called on the already-serving daemon (BOS-251 proof bridge), so a lock-free
	// read here would be a data race under `go test -race`.
	m.mu.RLock()
	archiveDelay := m.archiveDelay
	archiveError := m.archiveError
	m.mu.RUnlock()
	if archiveDelay > 0 {
		time.Sleep(archiveDelay)
	}
	if archiveError != "" {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s", archiveError))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.ArchivedAt = timestamppb.Now()
			return connect.NewResponse(&pb.ArchiveSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) ResurrectSession(_ context.Context, req *connect.Request[pb.ResurrectSessionRequest]) (*connect.Response[pb.ResurrectSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.ArchivedAt = nil
			return connect.NewResponse(&pb.ResurrectSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) RemoveSession(_ context.Context, req *connect.Request[pb.RemoveSessionRequest]) (*connect.Response[pb.RemoveSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, s := range m.sessions {
		if s.Id == req.Msg.Id {
			m.sessions = append(m.sessions[:i], m.sessions[i+1:]...)
			return connect.NewResponse(&pb.RemoveSessionResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) EmptyTrash(_ context.Context, _ *connect.Request[pb.EmptyTrashRequest]) (*connect.Response[pb.EmptyTrashResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var kept []*pb.Session
	var count int32
	for _, s := range m.sessions {
		if s.ArchivedAt != nil {
			count++
		} else {
			kept = append(kept, s)
		}
	}
	m.sessions = kept
	return connect.NewResponse(&pb.EmptyTrashResponse{DeletedCount: count}), nil
}

func (m *MockDaemon) ListChats(_ context.Context, req *connect.Request[pb.ListChatsRequest]) (*connect.Response[pb.ListChatsResponse], error) {
	if m.chatListDelay > 0 {
		time.Sleep(m.chatListDelay)
	}
	if m.chatListError != "" {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("%s", m.chatListError))
	}

	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*pb.ClaudeChat
	for _, c := range m.chats {
		if req.Msg.SessionId == "" || c.SessionId == req.Msg.SessionId {
			// Cloned, not shared: connect marshals this response after RUnlock,
			// and RecordChat backfills TmuxSessionName on the stored message
			// under the write lock. Handing out the stored pointer races that
			// write — cloning on the read side is what actually closes it.
			out = append(out, cloneMsg(c))
		}
	}
	return connect.NewResponse(&pb.ListChatsResponse{Chats: out}), nil
}

// ReportChatStatus records the request and answers success. The heartbeat
// (services/boss/internal/views/heartbeat.go) and the attach view both push
// statuses here and discard the response — which is an empty message anyway —
// so the recorded request is the whole observable surface. ReportChatStatusCalls
// is what turns "the heartbeat says it reported" into something a test can
// check.
//
// The request is cloned on the way in: the caller builds its reports slice once
// per tick and is free to reuse or mutate it afterwards, and mock state that
// changed under a test's feet would be worse than no record at all.
func (m *MockDaemon) ReportChatStatus(_ context.Context, req *connect.Request[pb.ReportChatStatusRequest]) (*connect.Response[pb.ReportChatStatusResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reportChatStatusCalls = append(m.reportChatStatusCalls, cloneMsg(req.Msg))
	return connect.NewResponse(&pb.ReportChatStatusResponse{}), nil
}

// ReportChatStatusCalls returns a copy of every ReportChatStatus request
// recorded, in arrival order. Each request carries the whole reports slice the
// caller sent, so a test can assert on the agent session ids and their statuses.
func (m *MockDaemon) ReportChatStatusCalls() []*pb.ReportChatStatusRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMsgs(m.reportChatStatusCalls)
}

// SetChatStatusesError makes GetChatStatuses fail with the given connect code
// and message instead of answering. Pass an empty message to clear it. Use this
// to drive the status-unavailable path a caller meets in the field — the
// daemon answering CodeUnimplemented, or RemoteClient.GetChatStatuses refusing
// under --remote (errLocalOnly, also Unimplemented) — through the real CLI
// without standing up a remote transport.
func (m *MockDaemon) SetChatStatusesError(code connect.Code, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.chatStatusesErrorCode, m.chatStatusesErrorMsg = code, msg
}

func (m *MockDaemon) GetChatStatuses(_ context.Context, req *connect.Request[pb.GetChatStatusesRequest]) (*connect.Response[pb.GetChatStatusesResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.chatStatusesErrorMsg != "" {
		return nil, connect.NewError(m.chatStatusesErrorCode, fmt.Errorf("%s", m.chatStatusesErrorMsg))
	}
	var out []*pb.ChatStatusEntry
	// Serve statuses for chats in the requested session, mirroring ListChats'
	// session filter so the picker only sees its own chats' heartbeats.
	for _, c := range m.chats {
		if req.Msg.SessionId != "" && c.SessionId != req.Msg.SessionId {
			continue
		}
		if e, ok := m.chatStatuses[c.AgentSessionId]; ok {
			out = append(out, e)
		}
	}
	return connect.NewResponse(&pb.GetChatStatusesResponse{Statuses: out}), nil
}

func (m *MockDaemon) GetSessionStatuses(_ context.Context, req *connect.Request[pb.GetSessionStatusesRequest]) (*connect.Response[pb.GetSessionStatusesResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	// Only answer for the ids the caller asked about, mirroring the real daemon.
	// Unseeded sessions are simply absent (not zero-valued): the home list treats
	// a missing entry as "no daemon-side status", which is what a session with no
	// live chat looks like.
	var out []*pb.SessionStatusEntry
	for _, id := range req.Msg.SessionIds {
		if e, ok := m.sessionStatuses[id]; ok {
			out = append(out, e)
		}
	}
	return connect.NewResponse(&pb.GetSessionStatusesResponse{Statuses: out}), nil
}

func (m *MockDaemon) ListRepoPRs(_ context.Context, req *connect.Request[pb.ListRepoPRsRequest]) (*connect.Response[pb.ListRepoPRsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	prs := m.prs[req.Msg.RepoId]
	return connect.NewResponse(&pb.ListRepoPRsResponse{PullRequests: prs}), nil
}

func (m *MockDaemon) ListTrackerIssues(_ context.Context, req *connect.Request[pb.ListTrackerIssuesRequest]) (*connect.Response[pb.ListTrackerIssuesResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	issues := m.trackerIssues[req.Msg.RepoId]
	// When the TUI sends a query, simulate the Linear-side filter by narrowing
	// to issues whose title contains the query (case-insensitive). This lets
	// tests exercise the debounced-search code path without spinning up a real
	// Linear API.
	if q := strings.TrimSpace(req.Msg.Query); q != "" {
		filtered := issues[:0:0]
		needle := strings.ToLower(q)
		for _, i := range issues {
			if strings.Contains(strings.ToLower(i.Title), needle) {
				filtered = append(filtered, i)
			}
		}
		issues = filtered
	}
	return connect.NewResponse(&pb.ListTrackerIssuesResponse{Issues: issues}), nil
}

// --- Repo management RPCs ---

func (m *MockDaemon) RemoveRepo(_ context.Context, req *connect.Request[pb.RemoveRepoRequest]) (*connect.Response[pb.RemoveRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.repos {
		if r.Id == req.Msg.Id {
			m.repos = append(m.repos[:i], m.repos[i+1:]...)
			return connect.NewResponse(&pb.RemoveRepoResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %q not found", req.Msg.Id))
}

func (m *MockDaemon) GetRepoSettings(_ context.Context, req *connect.Request[pb.GetRepoSettingsRequest]) (*connect.Response[pb.GetRepoSettingsResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos {
		if r.Id == req.Msg.Id {
			settings := &pb.RepoSettings{
				Id:                     r.Id,
				DisplayName:            r.DisplayName,
				SetupScript:            r.SetupScript,
				CanAutoMerge:           r.CanAutoMerge,
				CanAutoMergeDependabot: r.CanAutoMergeDependabot,
				CanAutoRepair:          r.CanAutoRepair,
				SentryOrg:              r.SentryOrg,
				HasLinearKey:           r.LinearApiKey != "",
				HasSentryKey:           r.SentryApiKey != "",
				UpdatedAt:              r.UpdatedAt,
			}
			switch r.MergeStrategy {
			case "merge":
				settings.MergeStrategy = pb.MergeStrategy_MERGE_STRATEGY_MERGE
			case "rebase":
				settings.MergeStrategy = pb.MergeStrategy_MERGE_STRATEGY_REBASE
			case "squash":
				settings.MergeStrategy = pb.MergeStrategy_MERGE_STRATEGY_SQUASH
			}
			return connect.NewResponse(&pb.GetRepoSettingsResponse{Settings: settings}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %q not found", req.Msg.Id))
}

func (m *MockDaemon) UpdateRepo(_ context.Context, req *connect.Request[pb.UpdateRepoRequest]) (*connect.Response[pb.UpdateRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.repos {
		if r.Id == req.Msg.Id {
			if req.Msg.DisplayName != nil {
				r.DisplayName = *req.Msg.DisplayName
			}
			if req.Msg.CanAutoMerge != nil {
				r.CanAutoMerge = *req.Msg.CanAutoMerge
			}
			if req.Msg.CanAutoMergeDependabot != nil {
				r.CanAutoMergeDependabot = *req.Msg.CanAutoMergeDependabot
			}
			if req.Msg.ShouldKeepBranchesCurrent != nil {
				r.ShouldKeepBranchesCurrent = *req.Msg.ShouldKeepBranchesCurrent
			}
			if req.Msg.CanAutoRepair != nil {
				r.CanAutoRepair = *req.Msg.CanAutoRepair
			}
			if req.Msg.MergeStrategy != nil {
				r.MergeStrategy = *req.Msg.MergeStrategy
			}
			if req.Msg.SetupScript != nil {
				r.SetupScript = req.Msg.SetupScript
			}
			r.UpdatedAt = timestamppb.Now()
			return connect.NewResponse(&pb.UpdateRepoResponse{Repo: r}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("repo %q not found", req.Msg.Id))
}

func (m *MockDaemon) ValidateRepoPath(_ context.Context, _ *connect.Request[pb.ValidateRepoPathRequest]) (*connect.Response[pb.ValidateRepoPathResponse], error) {
	m.mu.RLock()
	resp := m.validateRepoPathResp
	err := m.validateRepoPathErr
	m.mu.RUnlock()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if resp != nil {
		return connect.NewResponse(resp), nil
	}
	return connect.NewResponse(&pb.ValidateRepoPathResponse{
		IsValid:       true,
		IsGithub:      true,
		DefaultBranch: "main",
	}), nil
}

// SetValidateRepoPathResult overrides the default ValidateRepoPath response.
// Passing nil clears the override.
func (m *MockDaemon) SetValidateRepoPathResult(resp *pb.ValidateRepoPathResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validateRepoPathResp = resp
}

// SetValidateRepoPathError makes every ValidateRepoPath call return err.
func (m *MockDaemon) SetValidateRepoPathError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.validateRepoPathErr = err
}

func (m *MockDaemon) RegisterRepo(_ context.Context, req *connect.Request[pb.RegisterRepoRequest]) (*connect.Response[pb.RegisterRepoResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.registerRepoCalls = append(m.registerRepoCalls, req.Msg)
	repo := &pb.Repo{
		Id:                fmt.Sprintf("repo-%d", len(m.repos)+1),
		DisplayName:       req.Msg.DisplayName,
		LocalPath:         req.Msg.LocalPath,
		DefaultBaseBranch: req.Msg.DefaultBaseBranch,
		WorktreeBaseDir:   req.Msg.WorktreeBaseDir,
		SetupScript:       req.Msg.SetupScript,
		CreatedAt:         timestamppb.Now(),
		UpdatedAt:         timestamppb.Now(),
	}
	m.repos = append(m.repos, repo)
	return connect.NewResponse(&pb.RegisterRepoResponse{Repo: repo}), nil
}

// RegisterRepoCalls returns a copy of every RegisterRepo request recorded
// by the mock.
func (m *MockDaemon) RegisterRepoCalls() []*pb.RegisterRepoRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.RegisterRepoRequest, len(m.registerRepoCalls))
	copy(out, m.registerRepoCalls)
	return out
}

// --- Chat RPCs ---

func (m *MockDaemon) DeleteChat(_ context.Context, req *connect.Request[pb.DeleteChatRequest]) (*connect.Response[pb.DeleteChatResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.chats {
		if c.AgentSessionId == req.Msg.AgentSessionId {
			m.chats = append(m.chats[:i], m.chats[i+1:]...)
			return connect.NewResponse(&pb.DeleteChatResponse{}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %q not found", req.Msg.AgentSessionId))
}

// UpdateChatTitle persists the new title onto the seeded chat so a subsequent
// ListChats reports it. Before BOS-836 this was inert, which made the TUI's
// inline [r]ename look like it silently reverted the moment the next chat poll
// landed — the fixture, not the feature, was wrong.
//
// A title for a chat this daemon has never seen is answered with success rather
// than NotFound: the chat list is seeded per test, and a stricter answer would
// turn "this test did not seed that chat" into a confusing RPC failure far from
// its cause. DeleteChat is the one that needs NotFound, because deleting a
// missing chat is what its own tests assert on.
//
// The request is also recorded, because the stored chat cannot answer every
// question on its own: an unseeded chat leaves no trace of the rename, and two
// renames that settle on the same title are indistinguishable in the stored
// state. UpdateChatTitleResponse is an empty proto message, so the call log is
// the only place that detail survives (BOS-811).
func (m *MockDaemon) UpdateChatTitle(_ context.Context, req *connect.Request[pb.UpdateChatTitleRequest]) (*connect.Response[pb.UpdateChatTitleResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateChatTitleCalls = append(m.updateChatTitleCalls, cloneMsg(req.Msg))
	for _, c := range m.chats {
		if c.AgentSessionId == req.Msg.AgentSessionId {
			c.Title = req.Msg.Title
			break
		}
	}
	return connect.NewResponse(&pb.UpdateChatTitleResponse{}), nil
}

// UpdateChatTitleCalls returns a copy of every UpdateChatTitle request
// recorded, in arrival order — the agent session id each rename targeted and the
// title it asked for, including renames aimed at chats this daemon never saw.
func (m *MockDaemon) UpdateChatTitleCalls() []*pb.UpdateChatTitleRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMsgs(m.updateChatTitleCalls)
}

// ResolveContext answers with whatever SetResolvedContext seeded, ignoring the
// working directory the caller passed: a test controls the answer directly
// rather than by arranging the subprocess's cwd. An unseeded daemon returns an
// EMPTY response, not an error — "this directory is not inside a registered
// repo or session" is a normal answer, and CLI commands that fall back to it
// must see it as such.
func (m *MockDaemon) ResolveContext(context.Context, *connect.Request[pb.ResolveContextRequest]) (*connect.Response[pb.ResolveContextResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.resolvedContext == nil {
		return connect.NewResponse(&pb.ResolveContextResponse{}), nil
	}
	return connect.NewResponse(cloneMsg(m.resolvedContext)), nil
}

// --- Unimplemented RPCs (streaming or not used by tested views) ---
//
// Every RPC on this daemon belongs to one of exactly two categories, and there
// is no third one:
//
//  1. Fixture-backed — state guarded by m.mu, an exported seeder (Add*/Set*)
//     and/or an exported reader (*Calls(), Notes(), CronJobs()) so a test can
//     both stage the answer and observe what was asked. An empty proto response
//     message is fine here: DeleteChat and ReportChatStatus have nothing to say
//     back, and their fixture state is what the test reads.
//  2. Unimplemented — the handlers below, answering CodeUnimplemented. Nothing
//     the TUI renders calls them today, so there is no fixture to get right.
//
// What must never come back is the third shape this file used to carry: a
// handler that returns a zero-valued success with no fixture behind it. It
// hands a caller a plausible "nothing here" — an empty plugin list, an empty
// doctor report — and the test that drove it reads as covered while exercising
// nothing. A stub that answers Unimplemented fails on the first line the moment
// a view starts calling it, and whoever added that view writes the fixture then.
// That failure is the point: keep the gap loud (BOS-811).
//
// So when a view does start calling one of these, promote it to category 1 —
// give it fixture state and an exported seeder — rather than softening it back
// into an empty success.
//
// TestMockDaemonUnfixturedRPCsStayUnimplemented (mock_daemon_notes_test.go) is
// what makes the paragraph above enforceable rather than advisory: it asserts
// CodeUnimplemented for the five handlers BOS-811 flipped, so softening one back
// fails there first.

func (m *MockDaemon) CloneAndRegisterRepo(context.Context, *connect.Request[pb.CloneAndRegisterRepoRequest]) (*connect.Response[pb.CloneAndRegisterRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

// RepairDoctor, StartRepairWorkflow and ListPlugins reach the daemon only from
// the `boss repair …` and `boss plugin list` CLI commands
// (services/boss/cmd/repair_doctor.go, repair_start.go, handlers.go).
// GetSettings and UpdateSettings have no caller in services/boss at all — the
// global-settings pair is driven by the MCP backend
// (services/mcp/internal/socketbackend) and lib/bossalib/bossmcp, neither of
// which can reach this mock. So none of the five is on a TUI view's path, and
// no test here drives them through this mock. They returned empty successes
// until BOS-811.

func (m *MockDaemon) RepairDoctor(context.Context, *connect.Request[pb.RepairDoctorRequest]) (*connect.Response[pb.RepairDoctorResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) StartRepairWorkflow(context.Context, *connect.Request[pb.StartRepairWorkflowRequest]) (*connect.Response[pb.StartRepairWorkflowResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) ListPlugins(context.Context, *connect.Request[pb.ListPluginsRequest]) (*connect.Response[pb.ListPluginsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) GetSettings(context.Context, *connect.Request[pb.GetSettingsRequest]) (*connect.Response[pb.GetSettingsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) UpdateSettings(context.Context, *connect.Request[pb.UpdateSettingsRequest]) (*connect.Response[pb.UpdateSettingsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

// SetCreateSessionScript makes CreateSession stream the given frames, pausing
// frameDelay between consecutive ones, instead of returning Unimplemented. It
// is what lets a proof scenario reach the BOS-720 accepted-then-settled shape
// deterministically: a script of SessionCreated, SetupOutput, SessionCreated
// with a delay wide enough to capture the intermediate frame.
//
// The session carried by each frame has its title overwritten with the
// request's, so a scenario can assert on the title it typed.
func (m *MockDaemon) SetCreateSessionScript(frames []*pb.CreateSessionResponse, frameDelay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createSessionScript = frames
	m.createSessionFrameDelay = frameDelay
}

func (m *MockDaemon) CreateSession(ctx context.Context, req *connect.Request[pb.CreateSessionRequest], stream *connect.ServerStream[pb.CreateSessionResponse]) error {
	m.mu.Lock()
	m.lastCreateSession = req.Msg
	script := m.createSessionScript
	frameDelay := m.createSessionFrameDelay
	m.mu.Unlock()

	if len(script) == 0 {
		// Return Unimplemented so the TUI surfaces an error banner after recording
		// the request — tests assert on the captured request, not on created sessions.
		return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
	}

	for i, frame := range script {
		if i > 0 && frameDelay > 0 {
			select {
			case <-time.After(frameDelay):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		resp := cloneMsg(frame)
		if sc := resp.GetSessionCreated(); sc.GetSession() != nil {
			sc.Session.Title = req.Msg.GetTitle()
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
	return nil
}

// AttachSession reads events from the per-session channel populated by
// PushOutputLine / PushStateChange / PushSessionEnded and forwards them to
// the stream. Returns nil on SessionEnded or ctx cancellation.
func (m *MockDaemon) AttachSession(ctx context.Context, req *connect.Request[pb.AttachSessionRequest], stream *connect.ServerStream[pb.AttachSessionResponse]) error {
	m.mu.Lock()
	m.attachCalls = append(m.attachCalls, req.Msg)
	m.attachActive[req.Msg.Id]++
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.attachActive[req.Msg.Id]--
		if m.attachActive[req.Msg.Id] <= 0 {
			delete(m.attachActive, req.Msg.Id)
		}
		m.mu.Unlock()
	}()

	ch := m.ensureAttachChannel(req.Msg.Id)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(ev); err != nil {
				return err
			}
			if _, ended := ev.Event.(*pb.AttachSessionResponse_SessionEnded); ended {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// AttachSessionCalls returns a copy of every AttachSession request recorded
// by the mock.
func (m *MockDaemon) AttachSessionCalls() []*pb.AttachSessionRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.AttachSessionRequest, len(m.attachCalls))
	copy(out, m.attachCalls)
	return out
}

// PushOutputLine enqueues an OutputLine event on the session's attach stream.
// Blocks if the channel is full (64-event buffer should be enough for tests).
func (m *MockDaemon) PushOutputLine(sessionID, text string) {
	m.ensureAttachChannel(sessionID) <- &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_OutputLine{
			OutputLine: &pb.OutputLine{
				Text:      text,
				Timestamp: timestamppb.Now(),
			},
		},
	}
}

// PushStateChange enqueues a StateChange event on the session's attach stream.
func (m *MockDaemon) PushStateChange(sessionID string, previous, next pb.SessionState) {
	m.ensureAttachChannel(sessionID) <- &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{
				PreviousState: previous,
				NewState:      next,
			},
		},
	}
}

// PushSessionEnded enqueues a SessionEnded event. The active AttachSession
// stream returns nil after sending this event, closing the stream cleanly.
func (m *MockDaemon) PushSessionEnded(sessionID string, finalState pb.SessionState) {
	m.ensureAttachChannel(sessionID) <- &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_SessionEnded{
			SessionEnded: &pb.SessionEnded{
				FinalState: finalState,
			},
		},
	}
}

// SessionAttached reports whether at least one AttachSession stream is currently
// live for sessionID. The BOS-217 daemon-op pushers use this to refuse a push to
// a session no client is watching (an actionable error, never a silent buffer).
func (m *MockDaemon) SessionAttached(sessionID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.attachActive[sessionID] > 0
}

// TryPushOutputLine is the NON-BLOCKING sibling of PushOutputLine: it enqueues an
// OutputLine event but gives up after timeout with an error instead of blocking
// when the 64-event buffer is full. The BOS-217 daemon op uses it so a stalled or
// undrained TUI can never freeze the bridge's single-threaded serve loop. The
// blocking Push* variants are untouched for the existing tuitest suites.
func (m *MockDaemon) TryPushOutputLine(sessionID, text string, timeout time.Duration) error {
	return m.tryPush(sessionID, &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_OutputLine{
			OutputLine: &pb.OutputLine{Text: text, Timestamp: timestamppb.Now()},
		},
	}, timeout)
}

// TryPushStateChange is the non-blocking sibling of PushStateChange.
func (m *MockDaemon) TryPushStateChange(sessionID string, previous, next pb.SessionState, timeout time.Duration) error {
	return m.tryPush(sessionID, &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_StateChange{
			StateChange: &pb.StateChange{PreviousState: previous, NewState: next},
		},
	}, timeout)
}

// TryPushSessionEnded is the non-blocking sibling of PushSessionEnded.
func (m *MockDaemon) TryPushSessionEnded(sessionID string, finalState pb.SessionState, timeout time.Duration) error {
	return m.tryPush(sessionID, &pb.AttachSessionResponse{
		Event: &pb.AttachSessionResponse_SessionEnded{
			SessionEnded: &pb.SessionEnded{FinalState: finalState},
		},
	}, timeout)
}

// tryPush sends ev on the session's attach channel, returning an error if the
// buffer stays full for timeout. Shared by the three TryPush* wrappers.
func (m *MockDaemon) tryPush(sessionID string, ev *pb.AttachSessionResponse, timeout time.Duration) error {
	ch := m.ensureAttachChannel(sessionID)
	// NewTimer + Stop (rather than time.After) so the timer is released on the
	// common success path instead of lingering until it fires.
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case ch <- ev:
		return nil
	case <-timer.C:
		return fmt.Errorf("attach channel for session %q full after %s; TUI not draining", sessionID, timeout)
	}
}

// ensureAttachChannel returns the buffered event channel for a session,
// creating it if needed. Safe for concurrent callers.
func (m *MockDaemon) ensureAttachChannel(sessionID string) chan *pb.AttachSessionResponse {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch, ok := m.attachEvents[sessionID]
	if !ok {
		ch = make(chan *pb.AttachSessionResponse, 64)
		m.attachEvents[sessionID] = ch
	}
	return ch
}

func (m *MockDaemon) UpdateSession(_ context.Context, req *connect.Request[pb.UpdateSessionRequest]) (*connect.Response[pb.UpdateSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateSessionCalls = append(m.updateSessionCalls, req.Msg)
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			if req.Msg.Title != nil {
				s.Title = *req.Msg.Title
			}
			return connect.NewResponse(&pb.UpdateSessionResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) LinkSessionPR(_ context.Context, req *connect.Request[pb.LinkSessionPRRequest]) (*connect.Response[pb.LinkSessionPRResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prNumber, prURL, err := parseMockPRRef(req.Msg.Pr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	for _, s := range m.sessions {
		if s.Id == req.Msg.Id {
			s.PrNumber = &prNumber
			s.PrUrl = &prURL
			return connect.NewResponse(&pb.LinkSessionPRResponse{Session: s}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.Id))
}

func (m *MockDaemon) SwitchSessionAccount(_ context.Context, req *connect.Request[pb.SwitchSessionAccountRequest]) (*connect.Response[pb.SwitchSessionAccountResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.switchSessionAccountCalls = append(m.switchSessionAccountCalls, req.Msg)
	return connect.NewResponse(&pb.SwitchSessionAccountResponse{
		Resumed:     true,
		TargetLabel: req.Msg.AccountId,
		NoticeText:  "switched to " + req.Msg.AccountId,
	}), nil
}

// SwitchSessionAccountCalls returns every SwitchSessionAccount request the mock
// received so tests can assert on what the TUI/CLI sent.
func (m *MockDaemon) SwitchSessionAccountCalls() []*pb.SwitchSessionAccountRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*pb.SwitchSessionAccountRequest(nil), m.switchSessionAccountCalls...)
}

func parseMockPRRef(ref string) (int32, string, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.ParseInt(ref, 10, 32); err == nil {
		if n <= 0 {
			return 0, "", fmt.Errorf("PR number must be positive")
		}
		return int32(n), fmt.Sprintf("https://github.com/owner/repo/pull/%d", n), nil
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host == "" {
		return 0, "", fmt.Errorf("invalid PR URL")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return 0, "", fmt.Errorf("invalid PR URL")
	}
	n, err := strconv.ParseInt(parts[3], 10, 32)
	if err != nil || n <= 0 {
		return 0, "", fmt.Errorf("invalid PR number")
	}
	return int32(n), fmt.Sprintf("https://%s/%s/%s/pull/%d", u.Host, parts[0], parts[1], n), nil
}

func (m *MockDaemon) StopSession(context.Context, *connect.Request[pb.StopSessionRequest]) (*connect.Response[pb.StopSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) PauseSession(context.Context, *connect.Request[pb.PauseSessionRequest]) (*connect.Response[pb.PauseSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) ResumeSession(context.Context, *connect.Request[pb.ResumeSessionRequest]) (*connect.Response[pb.ResumeSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) RetrySession(context.Context, *connect.Request[pb.RetrySessionRequest]) (*connect.Response[pb.RetrySessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) CloseSession(context.Context, *connect.Request[pb.CloseSessionRequest]) (*connect.Response[pb.CloseSessionResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

// SetMergeDetail sets the human-readable note echoed as
// MergeSessionResponse.detail (e.g. a merge-strategy substitution). Empty — the
// default — means the merge ran exactly as configured and no note is returned.
func (m *MockDaemon) SetMergeDetail(detail string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mergeDetail = detail
}

// SetMergeError makes MergeSession fail with the given connect code and message
// instead of merging. Pass an empty message to clear it. Use this to drive the
// real daemon's merge-gate refusal shape (CodeFailedPrecondition with
// "merge blocked: gate=<slug>; <detail>") through a client.
func (m *MockDaemon) SetMergeError(code connect.Code, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mergeErrorCode, m.mergeErrorMsg = code, msg
}

// MergeSessionCalls returns the session ids MergeSession was called with, in
// order. A test asserting a merge did NOT happen must check this rather than
// stdout — stdout alone cannot distinguish "not merged" from "merged silently".
func (m *MockDaemon) MergeSessionCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]string(nil), m.mergeSessionCalls...)
}

// SetMergeResponseState makes a successful MergeSession answer with state
// instead of MERGED, reproducing the real daemon's read-before-refresh lag (see
// mergeResponseState). Pass SESSION_STATE_UNSPECIFIED to restore the default.
func (m *MockDaemon) SetMergeResponseState(state pb.SessionState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mergeResponseState = state
}

// MergeSession marks the stored session merged and echoes the configured
// detail. The real daemon's merge gate, per-repo serialization, and provider
// merge have no analogue here — SetMergeError injects the refusal instead, and
// SetMergeResponseState overrides the state it reports.
func (m *MockDaemon) MergeSession(_ context.Context, req *connect.Request[pb.MergeSessionRequest]) (*connect.Response[pb.MergeSessionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mergeSessionCalls = append(m.mergeSessionCalls, req.Msg.GetId())
	if m.mergeErrorMsg != "" {
		return nil, connect.NewError(m.mergeErrorCode, fmt.Errorf("%s", m.mergeErrorMsg))
	}
	for _, s := range m.sessions {
		if s.Id == req.Msg.GetId() {
			s.State = pb.SessionState_SESSION_STATE_MERGED
			if m.mergeResponseState != pb.SessionState_SESSION_STATE_UNSPECIFIED {
				// Answer with the lagging state without disturbing the stored
				// session, so a later list/get still sees the merge.
				lagging := cloneMsg(s)
				lagging.State = m.mergeResponseState
				return connect.NewResponse(&pb.MergeSessionResponse{Session: lagging, Detail: m.mergeDetail}), nil
			}
			return connect.NewResponse(&pb.MergeSessionResponse{Session: s, Detail: m.mergeDetail}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("session %q not found", req.Msg.GetId()))
}

// SetRecordChatResponse makes RecordChat answer with chat verbatim instead of
// minting a row. Pass a chat whose TmuxSessionName is empty to reproduce the
// real daemon's "row persisted, no live agent" outcome; pass nil to restore the
// default minting behaviour.
func (m *MockDaemon) SetRecordChatResponse(chat *pb.ClaudeChat) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordChatResponse = chat
}

// RecordChatCalls returns every RecordChat request in order. A test asserting a
// command issued NO RecordChat must check this rather than stdout, which cannot
// distinguish "no chat created" from "created and not printed".
func (m *MockDaemon) RecordChatCalls() []*pb.RecordChatRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*pb.RecordChatRequest, 0, len(m.recordChatCalls))
	for _, c := range m.recordChatCalls {
		out = append(out, cloneMsg(c))
	}
	return out
}

// SendChatMessageCalls returns every SendChatMessage request in order. The
// wake/submit flags are observable nowhere else.
func (m *MockDaemon) SendChatMessageCalls() []*pb.SendChatMessageRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneMsgs(m.sendChatMessageCalls)
}

// RecordChat mints (or returns) the chat row the attach flow needs, including a
// deterministic tmux session name. The real daemon creates the tmux session as a
// side effect; the mock cannot and does not — which is exactly the state an
// attach has to handle, so the flow reaches its own liveness check rather than
// dying on an Unimplemented RPC before it gets there.
func (m *MockDaemon) RecordChat(_ context.Context, req *connect.Request[pb.RecordChatRequest]) (*connect.Response[pb.RecordChatResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recordChatCalls = append(m.recordChatCalls, cloneMsg(req.Msg))
	if m.recordChatResponse != nil {
		return connect.NewResponse(&pb.RecordChatResponse{Chat: cloneMsg(m.recordChatResponse)}), nil
	}
	for _, c := range m.chats {
		if c.AgentSessionId == req.Msg.AgentSessionId {
			if c.TmuxSessionName == "" {
				c.TmuxSessionName = mockTmuxSessionName(req.Msg.AgentSessionId)
			}
			// Cloned like every other read path here: connect marshals this
			// after the lock is released, so returning the stored message would
			// race the backfill above. ListChats clones on its side too — both
			// are needed, since this is the only writer to a stored chat.
			return connect.NewResponse(&pb.RecordChatResponse{Chat: cloneMsg(c)}), nil
		}
	}
	chat := &pb.ClaudeChat{
		SessionId:       req.Msg.SessionId,
		AgentSessionId:  req.Msg.AgentSessionId,
		Title:           req.Msg.Title,
		TmuxSessionName: mockTmuxSessionName(req.Msg.AgentSessionId),
	}
	m.chats = append(m.chats, chat)
	return connect.NewResponse(&pb.RecordChatResponse{Chat: cloneMsg(chat)}), nil
}

// mockTmuxSessionName derives a stable tmux session name from an agent session
// id, mirroring the daemon's "one tmux session per chat" naming closely enough
// for a captured frame to look real.
func mockTmuxSessionName(agentSessionID string) string {
	return "boss-chat-" + agentSessionID
}

func (m *MockDaemon) WakeChat(context.Context, *connect.Request[pb.WakeChatRequest]) (*connect.Response[pb.WakeChatResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) GetChatTranscript(context.Context, *connect.Request[pb.GetChatTranscriptRequest]) (*connect.Response[pb.GetChatTranscriptResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) DescribeChatLaunch(context.Context, *connect.Request[pb.DescribeChatLaunchRequest]) (*connect.Response[pb.DescribeChatLaunchResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) DescribeChatMCP(context.Context, *connect.Request[pb.DescribeChatMCPRequest]) (*connect.Response[pb.DescribeChatMCPResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

// SendChatMessage records the request and reports delivery. The real daemon
// pastes into a tmux pane; the mock has none, so the interesting part here is
// the request itself — SendChatMessageCalls is what a test reads back.
func (m *MockDaemon) SendChatMessage(_ context.Context, req *connect.Request[pb.SendChatMessageRequest]) (*connect.Response[pb.SendChatMessageResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendChatMessageCalls = append(m.sendChatMessageCalls, cloneMsg(req.Msg))
	for _, c := range m.chats {
		if c.AgentSessionId == req.Msg.GetAgentSessionId() {
			return connect.NewResponse(&pb.SendChatMessageResponse{
				Delivered:       true,
				TmuxSessionName: mockTmuxSessionName(c.AgentSessionId),
			}), nil
		}
	}
	return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("chat %q not found", req.Msg.GetAgentSessionId()))
}

func (m *MockDaemon) DeliverVCSEvent(context.Context, *connect.Request[pb.DeliverVCSEventRequest]) (*connect.Response[pb.DeliverVCSEventResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("not implemented"))
}

func (m *MockDaemon) NotifyAuthChange(_ context.Context, req *connect.Request[pb.NotifyAuthChangeRequest]) (*connect.Response[pb.NotifyAuthChangeResponse], error) {
	m.mu.Lock()
	m.notifyAuthChangeCalls = append(m.notifyAuthChangeCalls, req.Msg.Action)
	m.mu.Unlock()
	return connect.NewResponse(&pb.NotifyAuthChangeResponse{}), nil
}

// NotifyAuthChangeCalls returns a copy of the actions ("login" / "logout")
// passed to every NotifyAuthChange request recorded by the mock.
func (m *MockDaemon) NotifyAuthChangeCalls() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, len(m.notifyAuthChangeCalls))
	copy(out, m.notifyAuthChangeCalls)
	return out
}

// --- Cron job RPCs ---

func (m *MockDaemon) CreateCronJob(_ context.Context, req *connect.Request[pb.CreateCronJobRequest]) (*connect.Response[pb.CreateCronJobResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createCronJobCalls = append(m.createCronJobCalls, req.Msg)
	m.cronJobCounter++
	job := &pb.CronJob{
		Id:        fmt.Sprintf("cron-%d", m.cronJobCounter),
		RepoId:    req.Msg.RepoId,
		Name:      req.Msg.Name,
		Prompt:    req.Msg.Prompt,
		Schedule:  req.Msg.Schedule,
		Timezone:  req.Msg.Timezone,
		IsEnabled: req.Msg.IsEnabled,
		AgentName: req.Msg.AgentName,
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
	m.cronJobs[job.Id] = job
	return connect.NewResponse(&pb.CreateCronJobResponse{CronJob: cloneMsg(job)}), nil
}

func (m *MockDaemon) ListCronJobs(_ context.Context, req *connect.Request[pb.ListCronJobsRequest]) (*connect.Response[pb.ListCronJobsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.cronJobs))
	for id := range m.cronJobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.CronJob, 0, len(ids))
	for _, id := range ids {
		job := m.cronJobs[id]
		if req.Msg.RepoId != nil && *req.Msg.RepoId != "" && job.RepoId != *req.Msg.RepoId {
			continue
		}
		// Clone so concurrent writers (e.g. UpdateCronJob) cannot race with
		// the response marshaler.
		out = append(out, cloneMsg(job))
	}
	return connect.NewResponse(&pb.ListCronJobsResponse{CronJobs: out}), nil
}

func (m *MockDaemon) GetCronJob(_ context.Context, req *connect.Request[pb.GetCronJobRequest]) (*connect.Response[pb.GetCronJobResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job, ok := m.cronJobs[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job %q not found", req.Msg.Id))
	}
	return connect.NewResponse(&pb.GetCronJobResponse{CronJob: cloneMsg(job)}), nil
}

func (m *MockDaemon) UpdateCronJob(_ context.Context, req *connect.Request[pb.UpdateCronJobRequest]) (*connect.Response[pb.UpdateCronJobResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateCronJobCalls = append(m.updateCronJobCalls, cloneMsg(req.Msg))
	job, ok := m.cronJobs[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job %q not found", req.Msg.Id))
	}
	if req.Msg.Name != nil {
		job.Name = *req.Msg.Name
	}
	if req.Msg.Prompt != nil {
		job.Prompt = *req.Msg.Prompt
	}
	if req.Msg.Schedule != nil {
		job.Schedule = *req.Msg.Schedule
	}
	if req.Msg.Timezone != nil {
		job.Timezone = *req.Msg.Timezone
	}
	if req.Msg.IsEnabled != nil {
		job.IsEnabled = *req.Msg.IsEnabled
	}
	if req.Msg.AgentName != nil {
		job.AgentName = *req.Msg.AgentName
	}
	job.UpdatedAt = timestamppb.Now()
	// Clone before returning: connect-go marshals the response after we
	// release the lock, and a subsequent UpdateCronJob would otherwise mutate
	// the same pointer concurrently with the in-flight marshal.
	return connect.NewResponse(&pb.UpdateCronJobResponse{CronJob: cloneMsg(job)}), nil
}

func (m *MockDaemon) DeleteCronJob(_ context.Context, req *connect.Request[pb.DeleteCronJobRequest]) (*connect.Response[pb.DeleteCronJobResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCronJobCalls = append(m.deleteCronJobCalls, req.Msg.Id)
	if _, ok := m.cronJobs[req.Msg.Id]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("cron job %q not found", req.Msg.Id))
	}
	delete(m.cronJobs, req.Msg.Id)
	return connect.NewResponse(&pb.DeleteCronJobResponse{}), nil
}

func (m *MockDaemon) CreateGithubCallback(_ context.Context, req *connect.Request[pb.CreateGithubCallbackRequest]) (*connect.Response[pb.CreateGithubCallbackResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createGithubCallbackCalls = append(m.createGithubCallbackCalls, cloneMsg(req.Msg))
	m.githubCallbackCounter++
	cb := &pb.GithubCallback{
		Id:           fmt.Sprintf("cb-%d", m.githubCallbackCounter),
		TargetChatId: req.Msg.TargetChatId,
		RepoOwner:    req.Msg.RepoOwner,
		RepoName:     req.Msg.RepoName,
		PrNumber:     req.Msg.PrNumber,
		Trigger:      req.Msg.Trigger,
		State:        string(models.GithubCallbackStateActive),
		ExpiresAt:    req.Msg.ExpiresAt,
	}
	if req.Msg.GroupId != nil {
		cb.GroupId = req.Msg.GetGroupId()
	}
	m.githubCallbacks[cb.Id] = cb
	return connect.NewResponse(&pb.CreateGithubCallbackResponse{GithubCallback: cloneMsg(cb)}), nil
}

func (m *MockDaemon) ListGithubCallbacks(_ context.Context, req *connect.Request[pb.ListGithubCallbacksRequest]) (*connect.Response[pb.ListGithubCallbacksResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listGithubCallbackCalls = append(m.listGithubCallbackCalls, cloneMsg(req.Msg))
	ids := make([]string, 0, len(m.githubCallbacks))
	for id := range m.githubCallbacks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.GithubCallback, 0, len(ids))
	for _, id := range ids {
		cb := m.githubCallbacks[id]
		if req.Msg.TargetChatId != nil && cb.TargetChatId != req.Msg.GetTargetChatId() {
			continue
		}
		if req.Msg.RepoOwner != nil && cb.RepoOwner != req.Msg.GetRepoOwner() {
			continue
		}
		if req.Msg.RepoName != nil && cb.RepoName != req.Msg.GetRepoName() {
			continue
		}
		if req.Msg.Trigger != nil && cb.Trigger != req.Msg.GetTrigger() {
			continue
		}
		if req.Msg.State != nil && cb.State != req.Msg.GetState() {
			continue
		}
		out = append(out, cloneMsg(cb))
	}
	return connect.NewResponse(&pb.ListGithubCallbacksResponse{GithubCallbacks: out}), nil
}

func (m *MockDaemon) DeleteGithubCallback(_ context.Context, req *connect.Request[pb.DeleteGithubCallbackRequest]) (*connect.Response[pb.DeleteGithubCallbackResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteGithubCallbackCalls = append(m.deleteGithubCallbackCalls, req.Msg.Id)
	delete(m.githubCallbacks, req.Msg.Id)
	return connect.NewResponse(&pb.DeleteGithubCallbackResponse{}), nil
}

// The broadcast RPCs keep real bookkeeping, as the GithubCallback trio above
// does, so `boss broadcast send|ls|rm` can be driven end to end against the
// mock.
//
// SendBroadcast deliberately mirrors the daemon's secret-body contract: the
// stored Broadcast carries no message, so a CLI leak test cannot pass merely
// because the mock never had the body to echo.

func (m *MockDaemon) SendBroadcast(_ context.Context, req *connect.Request[pb.SendBroadcastRequest]) (*connect.Response[pb.SendBroadcastResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendBroadcastCalls = append(m.sendBroadcastCalls, cloneMsg(req.Msg))
	m.broadcastCounter++
	b := &pb.Broadcast{
		Id:           fmt.Sprintf("bc-%d", m.broadcastCounter),
		Selector:     req.Msg.GetSelector(),
		OriginChatId: req.Msg.GetOriginChatId(),
		State:        "pending",
	}
	// Resolve one delivery per chat id named in the selector, honouring the
	// origin self-exclusion rule so the CLI's target table has something
	// meaningful to render.
	var deliveries []*pb.BroadcastDelivery
	for _, clause := range req.Msg.GetSelector().GetClauses() {
		for _, chatID := range clause.GetChatIds() {
			if chatID == req.Msg.GetOriginChatId() && !req.Msg.GetIncludeOrigin() {
				continue
			}
			deliveries = append(deliveries, &pb.BroadcastDelivery{
				BroadcastId:  b.Id,
				TargetChatId: chatID,
				State:        "pending",
			})
		}
	}
	m.broadcasts[b.Id] = b
	m.broadcastDeliveries[b.Id] = deliveries
	return connect.NewResponse(&pb.SendBroadcastResponse{
		Broadcast:  cloneMsg(b),
		Deliveries: deliveries,
	}), nil
}

func (m *MockDaemon) ListBroadcasts(_ context.Context, req *connect.Request[pb.ListBroadcastsRequest]) (*connect.Response[pb.ListBroadcastsResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listBroadcastCalls = append(m.listBroadcastCalls, cloneMsg(req.Msg))
	ids := make([]string, 0, len(m.broadcasts))
	for id := range m.broadcasts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.Broadcast, 0, len(ids))
	for _, id := range ids {
		b := m.broadcasts[id]
		if req.Msg.State != nil && b.State != req.Msg.GetState() {
			continue
		}
		if req.Msg.OriginChatId != nil && b.OriginChatId != req.Msg.GetOriginChatId() {
			continue
		}
		if req.Msg.TargetChatId != nil && !broadcastHasTarget(m.broadcastDeliveries[id], req.Msg.GetTargetChatId()) {
			continue
		}
		out = append(out, cloneMsg(b))
	}
	if limit := int(req.Msg.GetLimit()); limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return connect.NewResponse(&pb.ListBroadcastsResponse{Broadcasts: out}), nil
}

// broadcastHasTarget reports whether any delivery addressed the given chat,
// backing ListBroadcastsRequest.target_chat_id — the "what have I been sent"
// query, which reaches through the delivery rows rather than the broadcast.
func broadcastHasTarget(deliveries []*pb.BroadcastDelivery, chatID string) bool {
	for _, d := range deliveries {
		if d.GetTargetChatId() == chatID {
			return true
		}
	}
	return false
}

func (m *MockDaemon) DeleteBroadcast(_ context.Context, req *connect.Request[pb.DeleteBroadcastRequest]) (*connect.Response[pb.DeleteBroadcastResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteBroadcastCalls = append(m.deleteBroadcastCalls, req.Msg.Id)
	// Idempotent, matching the daemon: deleting an unknown id is not an error.
	delete(m.broadcasts, req.Msg.Id)
	delete(m.broadcastDeliveries, req.Msg.Id)
	return connect.NewResponse(&pb.DeleteBroadcastResponse{}), nil
}

// The note RPCs (BOS-550) keep real bookkeeping, as the GithubCallback trio
// above does, so `boss notes add|ls|show|edit|rm` can be driven end to end
// against the mock.
//
// They deliberately reproduce the daemon's two load-bearing behaviours rather
// than passing requests through: tags are NORMALISED on write, and every list
// filter is APPLIED. A mock that never filtered would let a CLI that filtered
// client-side — or never sent the filter at all — pass.

// noteEpoch anchors the mock's deterministic note clock. Each note write
// (create or update) advances it by a minute, so created_at/updated_at are
// exact, ordering is meaningful, and no test depends on wall-clock time.
var noteEpoch = time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)

// tickNoteClock advances the deterministic note clock. Callers must hold m.mu.
func (m *MockDaemon) tickNoteClock() *timestamppb.Timestamp {
	m.noteClock++
	return timestamppb.New(noteEpoch.Add(time.Duration(m.noteClock) * time.Minute))
}

// normaliseNoteTags mirrors the daemon's write-side normalisation: trim,
// lowercase, de-duplicate, and sort ascending. It is lossy by design — display
// casing is not preserved — and returns a nil slice when nothing survives, so
// a note with no usable tags stores no tags.
func normaliseNoteTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		t := strings.ToLower(strings.TrimSpace(tag))
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

func (m *MockDaemon) CreateNote(_ context.Context, req *connect.Request[pb.CreateNoteRequest]) (*connect.Response[pb.CreateNoteResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createNoteCalls = append(m.createNoteCalls, cloneMsg(req.Msg))
	m.noteCounter++
	now := m.tickNoteClock()
	note := &pb.Note{
		Id:        fmt.Sprintf("note-%d", m.noteCounter),
		RepoId:    req.Msg.GetRepoId(),
		SessionId: req.Msg.GetSessionId(),
		ChatId:    req.Msg.GetChatId(),
		Body:      req.Msg.GetBody(),
		Tags:      normaliseNoteTags(req.Msg.GetTags()),
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.notes[note.Id] = note
	return connect.NewResponse(&pb.CreateNoteResponse{Note: cloneMsg(note)}), nil
}

func (m *MockDaemon) GetNote(_ context.Context, req *connect.Request[pb.GetNoteRequest]) (*connect.Response[pb.GetNoteResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	note, ok := m.notes[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("note %q not found", req.Msg.GetId()))
	}
	return connect.NewResponse(&pb.GetNoteResponse{Note: cloneMsg(note)}), nil
}

func (m *MockDaemon) ListNotes(_ context.Context, req *connect.Request[pb.ListNotesRequest]) (*connect.Response[pb.ListNotesResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listNoteCalls = append(m.listNoteCalls, cloneMsg(req.Msg))

	// The tag filter is ANY-OF and fails closed: a non-empty list whose entries
	// all normalise away matches nothing rather than everything.
	wantTags := make(map[string]bool, len(req.Msg.GetTags()))
	for _, tag := range req.Msg.GetTags() {
		if t := strings.ToLower(strings.TrimSpace(tag)); t != "" {
			wantTags[t] = true
		}
	}
	tagFilterSet := len(req.Msg.GetTags()) > 0
	// A search term that is blank after trimming means NO search: an empty
	// substring already matches every body.
	search := strings.ToLower(strings.TrimSpace(req.Msg.GetSearch()))

	ids := make([]string, 0, len(m.notes))
	for id := range m.notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]*pb.Note, 0, len(ids))
	for _, id := range ids {
		n := m.notes[id]
		if !noteProvenanceMatches(req.Msg.RepoId, n.GetRepoId()) ||
			!noteProvenanceMatches(req.Msg.SessionId, n.GetSessionId()) ||
			!noteProvenanceMatches(req.Msg.ChatId, n.GetChatId()) {
			continue
		}
		if tagFilterSet && !noteHasAnyTag(n, wantTags) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(n.GetBody()), search) {
			continue
		}
		out = append(out, cloneMsg(n))
	}
	// Ordered by (created_at, id) ascending, as the daemon orders them.
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].GetCreatedAt().AsTime(), out[j].GetCreatedAt().AsTime()
		if a.Equal(b) {
			return out[i].GetId() < out[j].GetId()
		}
		return a.Before(b)
	})
	if limit := req.Msg.GetLimit(); limit > 0 && int(limit) < len(out) {
		out = out[:limit]
	}
	return connect.NewResponse(&pb.ListNotesResponse{Notes: out}), nil
}

// noteProvenanceMatches applies one repo/session/chat filter the way the
// daemon's SQL does. Two details matter, and getting either wrong inverts the
// result set:
//
//   - The daemon stores an absent session/chat as SQL NULL (optionalTrimmed in
//     services/bossd/internal/db/note_store.go), and `session_id = ?` never
//     matches NULL. So a note with no session is invisible to EVERY set session
//     filter — including `--session ""`, which therefore matches nothing rather
//     than "the notes that have no session".
//   - The daemon trims the filter value before binding it, so `--session
//     " s1 "` finds the same rows as `--session s1`.
//
// An unset (nil) filter is not applied at all and matches every note.
func noteProvenanceMatches(filter *string, stored string) bool {
	if filter == nil {
		return true
	}
	value := strings.TrimSpace(stored)
	if value == "" {
		return false
	}
	return value == strings.TrimSpace(*filter)
}

// noteHasAnyTag reports whether the note carries any of the wanted tags,
// backing the ANY-OF (OR) tag filter.
func noteHasAnyTag(n *pb.Note, want map[string]bool) bool {
	for _, tag := range n.GetTags() {
		if want[tag] {
			return true
		}
	}
	return false
}

func (m *MockDaemon) UpdateNote(_ context.Context, req *connect.Request[pb.UpdateNoteRequest]) (*connect.Response[pb.UpdateNoteResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateNoteCalls = append(m.updateNoteCalls, cloneMsg(req.Msg))
	note, ok := m.notes[req.Msg.GetId()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("note %q not found", req.Msg.GetId()))
	}
	// Nothing to change: the daemon returns the current note rather than
	// bumping updated_at for a write that alters no field, so an empty update
	// must not look like work was done.
	if req.Msg.Body == nil && req.Msg.Tags == nil {
		return connect.NewResponse(&pb.UpdateNoteResponse{Note: cloneMsg(note)}), nil
	}
	// Each field is optional: unset leaves that part alone. A SET tag set
	// REPLACES the whole set, including replacing it with nothing.
	if req.Msg.Body != nil {
		note.Body = req.Msg.GetBody()
	}
	if req.Msg.Tags != nil {
		note.Tags = normaliseNoteTags(req.Msg.GetTags().GetTags())
	}
	note.UpdatedAt = m.tickNoteClock()
	return connect.NewResponse(&pb.UpdateNoteResponse{Note: cloneMsg(note)}), nil
}

func (m *MockDaemon) DeleteNote(_ context.Context, req *connect.Request[pb.DeleteNoteRequest]) (*connect.Response[pb.DeleteNoteResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteNoteCalls = append(m.deleteNoteCalls, req.Msg.GetId())
	// Idempotent, matching the daemon: deleting an unknown id is not an error.
	delete(m.notes, req.Msg.GetId())
	return connect.NewResponse(&pb.DeleteNoteResponse{}), nil
}

// The standing-subscription trio (BOS-557) keeps real bookkeeping too, so
// `boss broadcast subscribe|subscriptions|unsubscribe` can be driven end to
// end. As with SendBroadcast, the stored subscription carries no body —
// pb.BroadcastSubscription has no message field at all — so a leak test cannot
// pass vacuously.
func (m *MockDaemon) CreateBroadcastSubscription(_ context.Context, req *connect.Request[pb.CreateBroadcastSubscriptionRequest]) (*connect.Response[pb.CreateBroadcastSubscriptionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.createBroadcastSubscriptionCalls = append(m.createBroadcastSubscriptionCalls, cloneMsg(req.Msg))
	m.broadcastSubscriptionCounter++
	sub := &pb.BroadcastSubscription{
		Id:             fmt.Sprintf("bsub-%d", m.broadcastSubscriptionCounter),
		OwnerSessionId: req.Msg.GetOwnerSessionId(),
		OriginChatId:   req.Msg.GetOriginChatId(),
		TriggerEvent:   req.Msg.GetTriggerEvent(),
		Selector:       req.Msg.GetSelector(),
		State:          "active",
	}
	m.broadcastSubscriptions[sub.Id] = sub
	return connect.NewResponse(&pb.CreateBroadcastSubscriptionResponse{Subscription: cloneMsg(sub)}), nil
}

func (m *MockDaemon) ListBroadcastSubscriptions(_ context.Context, req *connect.Request[pb.ListBroadcastSubscriptionsRequest]) (*connect.Response[pb.ListBroadcastSubscriptionsResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listBroadcastSubscriptionCalls = append(m.listBroadcastSubscriptionCalls, cloneMsg(req.Msg))
	ids := make([]string, 0, len(m.broadcastSubscriptions))
	for id := range m.broadcastSubscriptions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.BroadcastSubscription, 0, len(ids))
	for _, id := range ids {
		sub := m.broadcastSubscriptions[id]
		if req.Msg.OwnerSessionId != nil && sub.OwnerSessionId != req.Msg.GetOwnerSessionId() {
			continue
		}
		if req.Msg.OriginChatId != nil && sub.OriginChatId != req.Msg.GetOriginChatId() {
			continue
		}
		if req.Msg.State != nil && sub.State != req.Msg.GetState() {
			continue
		}
		// Matches the registered trigger EXACTLY: "settled" does not match a
		// "completed" filter, mirroring the daemon's query semantics.
		if req.Msg.TriggerEvent != nil && sub.TriggerEvent != req.Msg.GetTriggerEvent() {
			continue
		}
		out = append(out, cloneMsg(sub))
	}
	if limit := int(req.Msg.GetLimit()); limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return connect.NewResponse(&pb.ListBroadcastSubscriptionsResponse{Subscriptions: out}), nil
}

func (m *MockDaemon) DeleteBroadcastSubscription(_ context.Context, req *connect.Request[pb.DeleteBroadcastSubscriptionRequest]) (*connect.Response[pb.DeleteBroadcastSubscriptionResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteBroadcastSubscriptionCalls = append(m.deleteBroadcastSubscriptionCalls, req.Msg.Id)
	// Idempotent, matching the daemon and `DeleteBroadcast` above.
	delete(m.broadcastSubscriptions, req.Msg.Id)
	return connect.NewResponse(&pb.DeleteBroadcastSubscriptionResponse{}), nil
}

// ListCheckSnapshots mirrors the real daemon's semantics
// (services/bossd/internal/server/check_snapshots.go): session_id is required,
// a non-positive limit means 10, and the newest snapshots are returned first.
func (m *MockDaemon) ListCheckSnapshots(_ context.Context, req *connect.Request[pb.ListCheckSnapshotsRequest]) (*connect.Response[pb.ListCheckSnapshotsResponse], error) {
	sessionID := req.Msg.GetSessionId()
	if sessionID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("session_id is required"))
	}
	limit := int(req.Msg.GetLimit())
	if limit <= 0 {
		limit = 10
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	snaps := m.checkSnapshots[sessionID]
	if len(snaps) > limit {
		snaps = snaps[:limit]
	}
	resp := &pb.ListCheckSnapshotsResponse{}
	for _, s := range snaps {
		resp.Snapshots = append(resp.Snapshots, cloneMsg(s))
	}
	return connect.NewResponse(resp), nil
}

func (m *MockDaemon) RunCronJobNow(_ context.Context, req *connect.Request[pb.RunCronJobNowRequest]) (*connect.Response[pb.RunCronJobNowResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runCronJobNowCalls = append(m.runCronJobNowCalls, req.Msg.Id)
	if m.runCronJobNowMode == "alwaysSkip" {
		return connect.NewResponse(&pb.RunCronJobNowResponse{
			SkippedReason: m.runCronJobNowSkipReason,
		}), nil
	}
	// Default: alwaysRun — return a synthesized session.
	sess := &pb.Session{
		Id:    fmt.Sprintf("cron-run-%s", req.Msg.Id),
		State: pb.SessionState_SESSION_STATE_IMPLEMENTING_PLAN,
	}
	return connect.NewResponse(&pb.RunCronJobNowResponse{Session: sess}), nil
}

func (m *MockDaemon) ListAccounts(_ context.Context, req *connect.Request[pb.ListAccountsRequest]) (*connect.Response[pb.ListAccountsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.accounts))
	for id := range m.accounts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*pb.Account, 0, len(ids))
	for _, id := range ids {
		acct := m.accounts[id]
		if req.Msg.Provider != nil && *req.Msg.Provider != "" && acct.Provider != *req.Msg.Provider {
			continue
		}
		out = append(out, cloneMsg(acct))
	}
	return connect.NewResponse(&pb.ListAccountsResponse{Accounts: out}), nil
}

func (m *MockDaemon) AddAccount(_ context.Context, req *connect.Request[pb.AddAccountRequest]) (*connect.Response[pb.AddAccountResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addAccountCalls = append(m.addAccountCalls, cloneMsg(req.Msg))
	m.accountCounter++
	acct := &pb.Account{
		Id:        fmt.Sprintf("acct-%d", m.accountCounter),
		Provider:  req.Msg.Provider,
		Label:     req.Msg.Label,
		Priority:  req.Msg.Priority,
		Status:    "active",
		Health:    "ok",
		CreatedAt: timestamppb.Now(),
		UpdatedAt: timestamppb.Now(),
	}
	m.accounts[acct.Id] = acct
	// Store the credential in the keyring mirror; never surface it in a response.
	if len(req.Msg.Credential) > 0 {
		m.accountCredentials[acct.Id] = append([]byte(nil), req.Msg.Credential...)
	}
	return connect.NewResponse(&pb.AddAccountResponse{Account: cloneMsg(acct)}), nil
}

func (m *MockDaemon) RefreshAccount(_ context.Context, req *connect.Request[pb.RefreshAccountRequest]) (*connect.Response[pb.RefreshAccountResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	acct, ok := m.accounts[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account %q not found", req.Msg.Id))
	}
	if len(req.Msg.Credential) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("credential is required"))
	}
	m.accountCredentials[req.Msg.Id] = append([]byte(nil), req.Msg.Credential...)
	acct.UpdatedAt = timestamppb.Now()
	resp := &pb.RefreshAccountResponse{
		Account: cloneMsg(acct),
		Detail:  "credential refreshed",
	}
	if req.Msg.TestAfterSave {
		acct.LastTestOkAt = timestamppb.Now()
		acct.LastTestError = ""
		resp.Account = cloneMsg(acct)
		resp.Detail = "credential validated (provider verification unavailable in mock)"
	}
	return connect.NewResponse(resp), nil
}

func (m *MockDaemon) UpdateAccount(_ context.Context, req *connect.Request[pb.UpdateAccountRequest]) (*connect.Response[pb.UpdateAccountResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateAccountCalls = append(m.updateAccountCalls, cloneMsg(req.Msg))
	acct, ok := m.accounts[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account %q not found", req.Msg.Id))
	}
	if req.Msg.Label != nil {
		acct.Label = *req.Msg.Label
	}
	if req.Msg.Priority != nil {
		acct.Priority = *req.Msg.Priority
	}
	if req.Msg.Status != nil {
		acct.Status = *req.Msg.Status
	}
	if len(req.Msg.AllowedModels) > 0 {
		acct.AllowedModels = req.Msg.AllowedModels
	}
	acct.UpdatedAt = timestamppb.Now()
	return connect.NewResponse(&pb.UpdateAccountResponse{Account: cloneMsg(acct)}), nil
}

func (m *MockDaemon) RemoveAccount(_ context.Context, req *connect.Request[pb.RemoveAccountRequest]) (*connect.Response[pb.RemoveAccountResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeAccountCalls = append(m.removeAccountCalls, req.Msg.Id)
	if _, ok := m.accounts[req.Msg.Id]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account %q not found", req.Msg.Id))
	}
	delete(m.accounts, req.Msg.Id)
	// RemoveAccount purges the keyring credential too.
	delete(m.accountCredentials, req.Msg.Id)
	return connect.NewResponse(&pb.RemoveAccountResponse{}), nil
}

func (m *MockDaemon) TestAccount(_ context.Context, req *connect.Request[pb.TestAccountRequest]) (*connect.Response[pb.TestAccountResponse], error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.testAccountCalls = append(m.testAccountCalls, req.Msg.Id)
	acct, ok := m.accounts[req.Msg.Id]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("account %q not found", req.Msg.Id))
	}
	// No provider verification runner in the mock: validate presence of a
	// credential and report live_smoke_ran=false, mirroring the daemon's
	// nil-runner degrade.
	acct.LastTestOkAt = timestamppb.Now()
	acct.LastTestError = ""
	acct.UpdatedAt = timestamppb.Now()
	return connect.NewResponse(&pb.TestAccountResponse{
		Account:      cloneMsg(acct),
		LiveSmokeRan: false,
		Detail:       "credential validated (provider verification unavailable in mock)",
	}), nil
}

func (m *MockDaemon) ListAgents(_ context.Context, _ *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	agents := make([]*pb.AgentInfo, len(m.agents))
	copy(agents, m.agents)
	return connect.NewResponse(&pb.ListAgentsResponse{Agents: agents}), nil
}

// SetAgents overrides the agents returned by ListAgents. Tests use this to
// drive multi-agent flows (onboarding, agent picker, settings render).
func (m *MockDaemon) SetAgents(agents []*pb.AgentInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.agents = append([]*pb.AgentInfo(nil), agents...)
}

// removeSocket removes a socket file, ignoring "not exist" errors.
func removeSocket(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
