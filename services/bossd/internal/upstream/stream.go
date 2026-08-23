// Package upstream — stream.go houses the new reverse-stream client that
// replaces the heartbeat + SyncSessions loops in upstream.go. This file
// owns the outer reconnect loop and the per-connection orchestration
// (snapshot send, event forwarding, command dispatch, token refresh). The
// legacy Manager in upstream.go is preserved until P8 deletion; both can
// compile side-by-side so the switchover in T3.7 is a one-line swap.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/safego"
	"github.com/rs/zerolog"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Backoff bounds for the stream outer loop. Start cheap, cap tight so a
// dead orchestrator doesn't leave the daemon silent for a full minute.
const (
	streamInitialBackoff = 1 * time.Second
	streamMaxBackoff     = 30 * time.Second
)

// authWedgeWarnBudget is how many consecutive suppressed auth failures may
// pass before the wedge is re-stated at WARN. It mirrors the
// terminalReadyTimeoutBudget idiom in terminal_liveness.go: stay quiet through
// the ordinary case, but never go permanently silent about a condition an
// operator has to act on.
//
// This is a count of loop iterations, NOT a wall-clock period. The outer loop
// backs off from streamInitialBackoff to streamMaxBackoff, so the real-world
// cadence is backoff-dependent: at the streamMaxBackoff ceiling a budget of 30
// is roughly one WARN every 15 minutes; during the early ramp it is sooner.
// Deliberate — the point is bounded log volume, not a calendar.
const authWedgeWarnBudget = 30

// streamAuthSustainedFor is how long a stream must stay OPEN before the run
// loop treats it as positive proof bosso accepted this daemon's credentials.
//
// It exists because the only other direct proof — an inbound
// OrchestratorCommand (noteUpstreamAccepted) — is not something bosso is
// obliged to ever send. There is no ping or keepalive in the
// OrchestratorCommand oneof; bosso writes only when a webhook, a web-UI
// action, or a cron job produces a command. So a daemon that recovers and
// then holds a long-lived IDLE stream would otherwise keep reporting an
// hours-old wedge forever, and `boss daemon doctor` would print a FAIL that
// survives the very `boss login` it recommends.
//
// Duration, rather than the mere fact of an open stream, is the whole point:
// bosso authenticates on the request HEADERS, so a rejected daemon's stream
// dies within about one round trip of the handshake — the shape every wedge
// test drives. 30s is roughly an order of magnitude more than that rejection
// latency and still fast enough that an operator running the doctor after a
// login sees the recovery.
const streamAuthSustainedFor = 30 * time.Second

// The two escalated auth messages the Run loop emits. Both carry the same
// auth_failing_since / auth_failing_for / relogin_reason fields; only the
// claim differs, so an operator grepping the log can tell "one rejected open"
// from "this has been going on and needs you". Named constants because the
// wedge suite asserts on them and a silent divergence between the log and the
// test that pins it is precisely the failure BOS-944 is about.
const (
	streamAuthRejectedMsg = "stream closed, reconnecting (upstream auth rejected)"
	streamAuthWedgedMsg   = "stream closed, reconnecting (upstream auth wedged; run `boss login`)"
)

// Token refresh cadence. The refresher wakes every defaultRefreshInterval and
// exchanges the token once it is within defaultRefreshThreshold of expiry.
//
// The two MUST NOT be equal. They both used to be 60s, which meant a tick
// seeing 61s remaining skipped, and the next tick — a full interval later —
// found ~1s left. Production logs show exactly that: refreshes attempted with
// 1.16s, 0.88s, and on one occasion -0.86s (already expired) of validity. The
// refresh token itself does not care that the access token is stale, but the
// zero margin meant a single failed exchange had no time for a second attempt
// before the stream had to drop, so one transient network stall became a
// permanent sign-out (see the ErrRefreshOutcomeUnknown path in tokens.go).
//
// With a threshold well above the interval the first attempt starts ~2 minutes
// before expiry and a failure leaves room for several more. The threshold must
// still stay comfortably below the access-token TTL (300s in production):
// setting it at or above the TTL would make every tick eligible and refresh the
// token continuously, rotating the one-shot refresh token far more often than
// necessary and widening the very window this is meant to narrow.
const (
	defaultRefreshInterval  = 15 * time.Second
	defaultRefreshThreshold = 120 * time.Second
)

// bidirectionalStream abstracts the subset of *connect.BidiStreamForClient
// the StreamClient actually touches. The real ConnectRPC type already
// provides Send / Receive / CloseRequest, so this interface is satisfied
// without an adapter in production. Tests drop in a lightweight mock.
type bidirectionalStream interface {
	Send(*pb.DaemonEvent) error
	Receive() (*pb.OrchestratorCommand, error)
	CloseRequest() error
}

// streamOpener is the one bit of ConnectRPC glue the outer loop needs. The
// real OrchestratorServiceClient satisfies this via DaemonStream(ctx); the
// indirection keeps the test harness free of a full connect server.
type streamOpener interface {
	DaemonStream(ctx context.Context) bidirectionalStream
}

// SessionTokenHolder is a tiny mutex-protected token cache shared by every
// opener that needs to send X-Daemon-Token on the wire. The daemon's
// re-register flow (StreamClient.tryReRegister) conditionally swaps the holder
// after RegisterDaemon issues a fresh token; every opener that reads from the
// same holder picks up the new value transparently on its next dial.
//
// The holder exists because there are now TWO openers (DaemonStream and
// TerminalStream) that present the same daemon session_token and that must
// stay in lockstep. Without a shared holder, a token rotation would update
// only DaemonStream's opener and TerminalStream auth would silently fail
// until the daemon restarted.
type SessionTokenHolder struct {
	mu  sync.RWMutex
	tok string
	// clock is the injectable time source the lastSetAt stamps come from, so
	// a test can drive the holder from the same fake clock that drives the
	// stream client instead of racing wall time. Nil means "never set through
	// a constructor" (a zero-value holder), which now() answers with the real
	// clock rather than panicking — see now().
	clock Clock
	// lastSetAt is when this holder last took a non-empty token. Every write
	// path below happens only AFTER a successful upstream.Register — the
	// startup seed, the reRegister closure, and the opener's rotation calls
	// driven by tryReRegister — so the stamp is exactly "last successful
	// upstream registration in this daemon process" with no parallel
	// bookkeeping to drift out of step. Zero means this process has never
	// completed one. Read by the GetAuthState RPC (BOS-944).
	lastSetAt time.Time
}

// NewSessionTokenHolder constructs a holder seeded with the given token.
// Callers typically pass the SessionToken returned by upstream.Register.
// An empty seed leaves LastSetAt zero: startup Register failed, so there is
// no successful registration to report.
func NewSessionTokenHolder(tok string) *SessionTokenHolder {
	return newSessionTokenHolderWithClock(tok, realClock{})
}

// newSessionTokenHolderWithClock is the injectable-clock constructor behind
// NewSessionTokenHolder. Unexported because only this package's tests need to
// control the stamp; production always wants realClock.
func newSessionTokenHolderWithClock(tok string, clock Clock) *SessionTokenHolder {
	if clock == nil {
		clock = realClock{}
	}
	h := &SessionTokenHolder{tok: tok, clock: clock}
	if tok != "" {
		h.lastSetAt = clock.Now()
	}
	return h
}

// now reads the holder's clock, tolerating a zero-value holder. A holder built
// with a composite literal instead of the constructor has no clock, and a
// diagnostic timestamp is not worth a nil-pointer panic on a write path that
// every reconnect takes. Caller may hold h.mu; the clock is immutable after
// construction.
func (h *SessionTokenHolder) now() time.Time {
	if h.clock == nil {
		return time.Now()
	}
	return h.clock.Now()
}

// LastSetAt reports when the holder last took a non-empty token, i.e. the
// last successful upstream registration in this process. Zero when there has
// never been one. Safe for concurrent callers.
func (h *SessionTokenHolder) LastSetAt() time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastSetAt
}

// Get returns the current token. Safe for concurrent callers.
func (h *SessionTokenHolder) Get() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.tok
}

// Set replaces the cached token. Safe for concurrent callers.
func (h *SessionTokenHolder) Set(tok string) {
	h.mu.Lock()
	h.tok = tok
	if tok != "" {
		h.lastSetAt = h.now()
	}
	h.mu.Unlock()
}

// CompareAndSwap replaces the token only if it still matches old. It returns
// true when the replacement happened. Safe for concurrent callers.
func (h *SessionTokenHolder) CompareAndSwap(old, tok string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.tok != old {
		return false
	}
	h.tok = tok
	if tok != "" {
		h.lastSetAt = h.now()
	}
	return true
}

// connectOpener adapts a real bossanovav1connect.OrchestratorServiceClient
// to the streamOpener interface above. DaemonStream on that client returns
// a *connect.BidiStreamForClient which already implements bidirectionalStream
// structurally — the concrete return type is just wrapped here so the
// interface satisfaction is explicit and compile-checked.
type connectOpener struct {
	client    bossanovav1connect.OrchestratorServiceClient
	authToken string // fallback WorkOS JWT — used only when tokens is nil
	// tokens, when set, is consulted on every DaemonStream() call so that
	// reconnects after a bosso outage use a freshly-refreshed JWT rather
	// than the stale one bossd started with. The periodic in-band
	// refresher only runs while a stream is alive, so without this the
	// daemon tight-loops on "invalid credentials" once the initial JWT
	// expires during any reconnect gap > 5 min.
	tokens TokenProvider

	// sessionToken holds the daemon session_token from RegisterDaemon
	// (sent in X-Daemon-Token). Shared with the TerminalStream opener so a
	// rotation after a stale-token auth failure updates both transparently.
	sessionToken *SessionTokenHolder

	// authState is the shared "needs re-login" flag. When Refresh returns
	// ErrAuthExpired, the opener marks this state so the StreamClient.Run
	// loop pauses instead of tight-looping on a dead refresh token. Shared
	// with the TerminalStream opener so a rejection on one bidi pauses the
	// other. nil-safe — local-only / test paths can omit it.
	authState *AuthState

	logger zerolog.Logger // used for refresh-failure diagnostics in DaemonStream
}

// reloader is an optional capability on TokenProvider implementations:
// re-read whatever durable store backs the provider (keychain, file, env)
// into the in-memory cache. Used by DaemonStream as a recovery hatch — an
// external `boss login` might have written fresh tokens to the keychain
// even if the NotifyLogin RPC never reached us (daemon socket busy, boss
// run from a different machine via -remote, etc.).
type reloader interface {
	Reload()
}

// SetSessionToken swaps the daemon session_token used on subsequent
// DaemonStream opens. Called by StreamClient.Run after a successful
// re-register in response to a stale-token auth failure. Delegates to the
// shared holder so peer openers (TerminalStream) see the same value.
func (o *connectOpener) SetSessionToken(tok string) {
	if o.sessionToken == nil {
		return
	}
	o.sessionToken.Set(tok)
}

func (o *connectOpener) SessionToken() string {
	if o.sessionToken == nil {
		return ""
	}
	return o.sessionToken.Get()
}

func (o *connectOpener) CompareAndSwapSessionToken(old, tok string) bool {
	if o.sessionToken == nil {
		return false
	}
	return o.sessionToken.CompareAndSwap(old, tok)
}

// sessionTokenHolder is the capability StreamClient looks for on its
// opener to rotate the daemon session_token after a re-register. Real
// openers (connectOpener) satisfy this; bare test fakes that don't care
// about re-register simply don't implement it.
type sessionTokenHolder interface {
	SessionToken() string
	SetSessionToken(tok string)
	CompareAndSwapSessionToken(old, tok string) bool
}

// DaemonStream opens a new bidi stream attaching the WorkOS JWT as a
// Bearer token and the daemon session_token as X-Daemon-Token. bosso
// requires both and cross-checks that the JWT's user owns the daemon
// identified by the session token (see services/bosso/internal/server/
// stream.go).
func (o *connectOpener) DaemonStream(ctx context.Context) bidirectionalStream {
	raw := o.client.DaemonStream(ctx)
	jwt := o.authToken
	if o.tokens != nil {
		// Refresh if the cached token is expired or within 60s of expiry
		// — the typical reconnect path. This window is deliberately
		// tighter than defaultRefreshThreshold: the periodic refresher
		// owns staying ahead of expiry, and this path only has to
		// guarantee the token outlives the register handshake. Widening
		// it to the threshold would make every reconnect force an
		// exchange the refresher had already scheduled.
		if exp := o.tokens.ExpiresAt(); !exp.IsZero() && time.Until(exp) < 60*time.Second {
			// o.logger rides on the context so the provider's in-window replay
			// warning is logged rather than dropped (see logRefreshReplay).
			if _, err := o.tokens.Refresh(o.logger.WithContext(ctx)); err != nil {
				// Distinguish terminal from transient. ErrAuthExpired
				// covers both BOS-659 terminal states — the refresh token
				// was authoritatively rejected, or the exchange outcome
				// could never be confirmed — and neither is fixable by a
				// keychain reload (the keychain holds the same unusable
				// token, now flagged), while continuing to dial just
				// produces log spam at 1 Hz. Mark the shared AuthState so
				// the Run loop pauses on the next iteration; log one
				// sanitized line on the state change, carrying the
				// enumerated reason rather than the raw error, so an
				// ambiguous timeout is never reported as invalid_grant.
				//
				// For transient failures (network, 5xx, malformed body)
				// keep the original behaviour: log + reload keychain so
				// an out-of-band `boss login` is picked up even if its
				// NotifyLogin RPC never reached us.
				if errors.Is(err, ErrAuthExpired) && o.authState != nil {
					if o.authState.MarkNeedsLogin() {
						logReloginPause(&o.logger, "", err)
					}
				} else {
					o.logger.Warn().Err(err).Msg("token refresh failed; reloading keychain")
					if r, ok := o.tokens.(reloader); ok {
						r.Reload()
					}
				}
			}
		} else if exp.IsZero() {
			// No expiry recorded — usually means the in-memory cache is
			// empty (e.g. bossd started before the user ran `boss
			// login`, or NotifyLogin was missed). Reload from the
			// keychain so a later `boss login` is picked up here even
			// without an explicit notification.
			if r, ok := o.tokens.(reloader); ok {
				r.Reload()
			}
		}
		if t := o.tokens.Token(); t != "" {
			jwt = t
		}
	}
	if jwt != "" {
		raw.RequestHeader().Set("Authorization", "Bearer "+jwt)
	} else if o.authState != nil {
		// No JWT to send and no static fallback either. Common causes:
		// the daemon started before the user ran `boss login`, a transient
		// refresh failure returned an empty cache, or — since BOS-659 —
		// the provider reloaded a RETAINED record flagged for re-login,
		// which deliberately exposes no bearer token. In every case
		// dialling produces a "missing or invalid Authorization header"
		// rejection that the Run loop would otherwise retry forever. Mark
		// NeedsLogin so it pauses on the next iteration; log only on the
		// state change, and let the flagged case explain itself rather
		// than reporting the retained credentials as absent.
		if o.authState.MarkNeedsLogin() {
			o.logger.Warn().Msg(noCredentialsPauseMessage(o.tokens))
		}
	}
	if o.sessionToken != nil {
		if tok := o.sessionToken.Get(); tok != "" {
			raw.RequestHeader().Set("X-Daemon-Token", tok)
		}
	}
	return connectBidiAdapter{stream: raw}
}

// connectBidiAdapter bridges connect's BidiStreamForClient (which has all
// three methods already) to the local interface. Kept as a value type so
// nil-check pitfalls are obvious.
type connectBidiAdapter struct {
	stream *connect.BidiStreamForClient[pb.DaemonEvent, pb.OrchestratorCommand]
}

func (a connectBidiAdapter) Send(ev *pb.DaemonEvent) error { return a.stream.Send(ev) }

func (a connectBidiAdapter) Receive() (*pb.OrchestratorCommand, error) {
	return a.stream.Receive()
}

func (a connectBidiAdapter) CloseRequest() error { return a.stream.CloseRequest() }

// TokenProvider hands out WorkOS access tokens and refreshes them on
// demand. Mirrors the Manager's existing keychain-backed path but
// expressed as an interface so the stream client can be tested without
// a real keychain.
type TokenProvider interface {
	// Token returns the currently-cached access token. Empty when no
	// token is available (caller decides whether to proceed).
	Token() string
	// ExpiresAt returns the expiry timestamp for the cached token. Zero
	// value means "unknown" — the refresher should treat that as "do not
	// refresh proactively".
	ExpiresAt() time.Time
	// Refresh obtains a new access token from WorkOS. Implementations
	// must update the cached Token()/ExpiresAt() on success so the next
	// reconnect uses the fresh token.
	Refresh(ctx context.Context) (string, error)
}

// SessionCommandHandler encapsulates the daemon's existing stop/pause/resume
// paths behind a stream-shaped interface. The concrete implementation (wired
// in T3.7) delegates to *session.Lifecycle / *server.Server; the interface
// keeps command_dispatcher.go free of a dependency on the server package,
// avoiding an import cycle with upstream.
type SessionCommandHandler interface {
	Stop(ctx context.Context, sessionID string) (*pb.Session, error)
	Pause(ctx context.Context, sessionID string) (*pb.Session, error)
	Resume(ctx context.Context, sessionID string) (*pb.Session, error)
	// WakeChat brings a stopped chat's tmux+claude back to life. Returns
	// the chosen outcome (already_live / resumed / fresh_fallback) and the
	// persisted tmux session name. errorCode classifies any failure so
	// the dispatcher can attach a typed CommandResult.error_code (the
	// proxy maps it back to the right ConnectRPC code without parsing
	// the human-readable error string).
	WakeChat(ctx context.Context, agentSessionID string, forceFresh bool) (outcome pb.WakeChatResult_Outcome, tmuxName string, reason string, errorCode pb.CommandResult_ErrorCode, err error)
	// SwitchAccount switches a session's live chat to a different account
	// (stop+swap+resume). agentSessionID, when empty, resolves the session's
	// primary live chat. accountID "" means the system default (account 0).
	// errorCode classifies any failure so the dispatcher can attach a typed
	// CommandResult.error_code (the proxy maps it back to the right ConnectRPC
	// code without parsing the human-readable error string).
	SwitchAccount(ctx context.Context, sessionID, agentSessionID, accountID string, force bool) (resumed bool, targetLabel, noticeText string, errorCode pb.CommandResult_ErrorCode, err error)
	MergeSession(ctx context.Context, sessionID string) (*pb.Session, error)
	ArchiveSession(ctx context.Context, sessionID string) (*pb.Session, error)
	// RetrySession retries a failed session and returns the updated row.
	RetrySession(ctx context.Context, sessionID string) (*pb.Session, error)
	// UpdateSession updates a session's title/tracker fields (carried on the
	// stream command's optional pointers) and returns the updated row.
	UpdateSession(ctx context.Context, req *pb.UpdateSessionCommand) (*pb.Session, error)
	// LinkSessionPR attaches an existing PR to a session and returns the updated row.
	LinkSessionPR(ctx context.Context, sessionID, pr string) (*pb.Session, error)
	RecordChat(ctx context.Context, sessionID, agentSessionID, title string, resume bool, agentName string) (*pb.ClaudeChat, error)
	// DeleteChat removes a chat by agent_session_id. sessionID, when non-empty,
	// scopes the delete: the handler rejects the request if the chat does not
	// belong to that session (so bosso's session-level authz is enforced
	// end-to-end, not advisory).
	DeleteChat(ctx context.Context, sessionID, agentSessionID string) error
	// UpdateChatTitle renames a chat by agent_session_id. Returns no payload.
	UpdateChatTitle(ctx context.Context, agentSessionID, title string) error
	// ReportChatStatus forwards one or more chat status heartbeats. Returns no
	// payload. Bosso resolves each report's owning daemon before dispatch, so the
	// slice handed here is already scoped to this daemon's chats.
	ReportChatStatus(ctx context.Context, reports []*pb.ChatStatusReport) error
	// ListRepos returns the daemon's full Repo set. Not session-scoped —
	// used by bosso's repo-first new-session wizard to aggregate repos
	// across every live daemon.
	ListRepos(ctx context.Context) (*pb.ListReposResponse, error)
	// ListAgents returns the daemon's installed agents. Not session-scoped —
	// bosso proxies a per-daemon agent listing for the wizard.
	ListAgents(ctx context.Context) (*pb.ListAgentsResponse, error)
	// ListAccounts returns the daemon's rotation accounts (metadata only — never
	// credentials), optionally filtered by provider ("" = all). Bosso proxies
	// this for the web account-switch picker, scoped to a session's owning
	// daemon. refresh, when true, requests a live per-account usage probe
	// before the re-read (fail-soft — see services/bossd/internal/server/account.go).
	ListAccounts(ctx context.Context, provider string, refresh bool) (*pb.ListAccountsResponse, error)
	// GetRepo returns a repo's web-safe settings for the hosted repo-management
	// surface. The direct daemon handler never exposes plaintext keys.
	GetRepo(ctx context.Context, repoID string) (*pb.GetRepoSettingsResponse, error)
	// UpdateRepo forwards the browser-safe update command to the direct daemon
	// handler. SecretUpdate values retain their tri-state semantics unchanged.
	UpdateRepo(ctx context.Context, req *pb.UpdateRepoCommand) (*pb.UpdateRepoResponse, error)
	// RemoveRepo removes a repo by ID.
	RemoveRepo(ctx context.Context, repoID string) error
	// ListRepoPRs returns a repo's open PRs for the web "existing PR" picker.
	ListRepoPRs(ctx context.Context, repoID string) (*pb.ListRepoPRsResponse, error)
	// ListTrackerIssues returns a repo's tracker issues (optional server-side
	// query + source) for the web Linear/Sentry pickers.
	ListTrackerIssues(ctx context.Context, repoID, query string, source *string) (*pb.ListTrackerIssuesResponse, error)
	// GetChatTranscript reads a chat's transcript by agent_session_id. sessionID,
	// when non-empty, scopes the read (the handler rejects the request if the chat
	// does not belong to that session). Network/tmux-bound — dispatched async.
	GetChatTranscript(ctx context.Context, sessionID, agentSessionID string, maxMessages int32) (*pb.GetChatTranscriptResponse, error)
	// SendChatMessage delivers a user message into a chat's live agent, optionally
	// waking it first. submit routes verified-submit vs. prefill delivery (BOS-242
	// Gap 1). Network/tmux-bound — dispatched async.
	SendChatMessage(ctx context.Context, agentSessionID, message string, wakeIfAsleep, submit bool) (*pb.SendChatMessageResponse, error)
	// ListCronJobs returns the daemon's cron jobs for the web cron-management
	// surface. Not session-scoped. Store-bound — dispatched async.
	ListCronJobs(ctx context.Context) (*pb.ListCronJobsResponse, error)
	// CreateCronJob registers a new scheduled prompt. Validation (required fields,
	// schedule parse, agent/model) happens in the daemon's cron handler; the
	// daemon's connect-coded error surfaces via CommandResult.error. Async.
	CreateCronJob(ctx context.Context, cmd *pb.CreateCronJobCommand) (*pb.CreateCronJobResponse, error)
	// UpdateCronJob mutates an existing cron job. Only the optional fields the
	// command sets are updated; the rest are left untouched by the daemon handler.
	// Async.
	UpdateCronJob(ctx context.Context, cmd *pb.UpdateCronJobCommand) (*pb.UpdateCronJobResponse, error)
	// DeleteCronJob removes a cron job by ID (scheduler + database). Async.
	DeleteCronJob(ctx context.Context, id string) error
	// RunCronJobNow fires a cron job immediately, honoring the same overlap and
	// concurrency-cap rules as scheduled fires. Async.
	RunCronJobNow(ctx context.Context, id string) (*pb.RunCronJobNowResponse, error)
	// CreateGithubCallback registers a pending GitHub-callback that delivers a
	// message to a chat when a PR trigger fires. Validation happens in the
	// daemon handler; its connect-coded error surfaces via CommandResult.error.
	// The Message field is a secret and is never logged. Async.
	CreateGithubCallback(ctx context.Context, cmd *pb.CreateGithubCallbackCommand) (*pb.CreateGithubCallbackResponse, error)
	// ListGithubCallbacks returns pending GitHub-callbacks, filtered by the
	// optional fields the command sets. Store-bound — dispatched async.
	ListGithubCallbacks(ctx context.Context, cmd *pb.ListGithubCallbacksCommand) (*pb.ListGithubCallbacksResponse, error)
	// DeleteGithubCallback removes a pending GitHub-callback by ID. Async.
	DeleteGithubCallback(ctx context.Context, id string) error
	// CreateNote records a note against a repository (BOS-552). Validation and
	// tag normalisation live in the daemon's note store; its connect-coded
	// error surfaces via CommandResult.error. Store-bound — dispatched async.
	CreateNote(ctx context.Context, cmd *pb.CreateNoteCommand) (*pb.CreateNoteResponse, error)
	// GetNote reads one note by ID. An absent ID maps to NotFound. Async.
	GetNote(ctx context.Context, cmd *pb.GetNoteCommand) (*pb.GetNoteResponse, error)
	// ListNotes returns the notes matching the optional filters the command
	// sets, in the store's deterministic order. Async.
	ListNotes(ctx context.Context, cmd *pb.ListNotesCommand) (*pb.ListNotesResponse, error)
	// UpdateNote edits a note's body and/or tags. An unset field is left alone;
	// a set tag list replaces the whole tag set. Async.
	UpdateNote(ctx context.Context, cmd *pb.UpdateNoteCommand) (*pb.UpdateNoteResponse, error)
	// DeleteNote removes a note by ID. The store's delete is idempotent, so
	// deleting an already-absent ID succeeds. Async.
	DeleteNote(ctx context.Context, id string) error
	// DeliverBroadcast materialises an inbound cross-daemon broadcast LOCALLY
	// (BOS-558): bosso routed here a broadcast some OTHER daemon originated, and
	// this daemon turns it into delivery rows for the existing worker to drain.
	//
	// Everything that makes that safe lives BEHIND this method, in
	// internal/broadcast, not in the dispatcher: the loop guard that drops a
	// command this daemon originated, the idempotency probe that makes an
	// at-least-once redelivery a no-op, and local-only selector resolution under
	// the same fan-out cap and start-error filter a local send gets. The
	// dispatcher forwards the command verbatim and decides nothing.
	//
	// It NEVER re-publishes upstream. pb.BroadcastEgress (daemon->bosso) and
	// pb.BroadcastCommand (bosso->daemon) are deliberately separate message
	// types so a receipt cannot become a send, and the ingress structurally
	// holds no egress publisher — that absence is the anti-storm guarantee.
	//
	// SECRET BODY: cmd.message is the broadcast prompt. The returned error text
	// travels back to bosso on CommandResult.error, so no error from this path
	// may contain it. Store-bound — dispatched async.
	DeliverBroadcast(ctx context.Context, cmd *pb.BroadcastCommand) error
	// AddAccount registers a new provider login. The credential blob is
	// inbound-only (consumed into the keyring, never echoed); the response
	// carries account metadata only. Store/keyring-bound — dispatched async.
	AddAccount(ctx context.Context, cmd *pb.AddAccountCommand) (*pb.AddAccountResponse, error)
	// RefreshAccount replaces an account's stored credential in place and,
	// optionally, tests it after save. Credential inbound-only. Async.
	RefreshAccount(ctx context.Context, cmd *pb.RefreshAccountCommand) (*pb.RefreshAccountResponse, error)
	// UpdateAccount mutates account metadata (present-only semantics: only the
	// optional fields the command sets are applied). Async.
	UpdateAccount(ctx context.Context, cmd *pb.UpdateAccountCommand) (*pb.UpdateAccountResponse, error)
	// RemoveAccount deletes the metadata row and purges the keyring credential.
	// Async.
	RemoveAccount(ctx context.Context, id string) error
	// TestAccount validates the account's stored credential (and runs a provider
	// smoke check when a runner is wired), recording the outcome. Async.
	TestAccount(ctx context.Context, cmd *pb.TestAccountCommand) (*pb.TestAccountResponse, error)
	// ListChats returns a session's chats. sessionID scopes the read for authz.
	// Store-bound — dispatched async.
	ListChats(ctx context.Context, sessionID string) (*pb.ListChatsResponse, error)
	// GetChatStatuses returns a session's per-chat statuses. sessionID scopes the
	// read for authz. Distinct from GetSessionStatuses, which collapses a
	// session's chats into one aggregate. Store-bound — dispatched async.
	GetChatStatuses(ctx context.Context, sessionID string) (*pb.GetChatStatusesResponse, error)
	// GetSessionStatuses returns the aggregate status for the given sessions.
	// Store-bound — dispatched async.
	GetSessionStatuses(ctx context.Context, sessionIDs []string) (*pb.GetSessionStatusesResponse, error)
	// ListCheckSnapshots returns a session's recent CI check snapshots
	// (newest-first; limit defaults to 10 when zero). Store-bound — dispatched async.
	ListCheckSnapshots(ctx context.Context, sessionID string, limit int32) (*pb.ListCheckSnapshotsResponse, error)
	// ListPlugins returns the daemon's configured plugins and load state. Not
	// session-scoped. Async.
	ListPlugins(ctx context.Context) (*pb.ListPluginsResponse, error)
	// GetCronJob returns a single cron job by id. Not session-scoped. Async.
	GetCronJob(ctx context.Context, id string) (*pb.GetCronJobResponse, error)
	// RepairDoctor runs the daemon's repair-doctor diagnostics. Not
	// session-scoped. Async.
	RepairDoctor(ctx context.Context) (*pb.RepairDoctorResponse, error)
	// CloseSession closes (abandons) a session, returning the updated Session.
	CloseSession(ctx context.Context, sessionID string) (*pb.Session, error)
	// ResurrectSession restores an archived session, returning the updated Session.
	ResurrectSession(ctx context.Context, sessionID string) (*pb.Session, error)
	// RemoveSession permanently removes a session and its worktree. Filesystem-bound
	// — dispatched async.
	RemoveSession(ctx context.Context, sessionID string) error
	// EmptyTrash permanently deletes archived sessions, optionally only those
	// archived before olderThan (nil = all). Returns the deleted count.
	// Filesystem-bound — dispatched async. Not session-scoped.
	EmptyTrash(ctx context.Context, olderThan *timestamppb.Timestamp) (int32, error)
}

// WebhookCommandDispatcher forwards a webhook payload to whatever in-daemon
// subscriber handles it. Returning (ok, err) keeps the dispatcher
// boilerplate uniform with the Stop/Pause/Resume paths.
type WebhookCommandDispatcher interface {
	Dispatch(ctx context.Context, ev *pb.WebhookEvent) error
}

// TransferHandler encapsulates the daemon's participation in the
// coordinated transfer protocol (decision #14). One daemon can be either
// source or target in a given transfer; the handler figures out its role
// from the session_id and its own state.
//
// Transfer: bosso is initiating. If this daemon owns the session it takes
// the source role (pause, mark transferring_to) and returns (nil, nil) so
// the ACK carries no payload. If it does not own the session yet it takes
// the target role (create with transferring_from, resume) and returns a
// non-nil TransferConfirmed payload.
//
// Confirmed: bosso has seen the target's resume succeed. Source daemons
// emit SessionDelta{DELETED} for the session.
//
// Cancel: bosso is rolling back. Source daemons clear their
// transferring_to marker; target daemons delete any copy they created.
// Idempotent — safe to call when the daemon has no matching state.
//
// The interface keeps command_dispatcher.go free of a dependency on the
// session package (avoiding an import cycle), matching the
// SessionCommandHandler pattern.
type TransferHandler interface {
	Transfer(ctx context.Context, req *pb.TransferSessionCommand) (*pb.TransferConfirmed, error)
	Confirmed(ctx context.Context, req *pb.TransferConfirmed) error
	Cancel(ctx context.Context, req *pb.TransferCancel) error
}

// SessionAttacher kicks off a tmux reader for the given session and
// streams SessionAttachChunk events on the returned channel until the
// session ends or ctx is cancelled. Implementations are responsible for
// closing the channel when the attach ends.
type SessionAttacher interface {
	Attach(ctx context.Context, sessionID, commandID string) (<-chan *pb.SessionAttachChunk, error)
}

// SessionCreator kicks off a streaming session creation for the given
// command and streams SessionCreateChunk events on the returned channel until
// creation completes (terminal `created`) or fails (terminal `error`).
// Implementations are responsible for closing the channel when done. Mirrors
// SessionAttacher.
type SessionCreator interface {
	Create(ctx context.Context, cmd *pb.CreateSessionCommand, commandID string) (<-chan *pb.SessionCreateChunk, error)
}

// StreamStores bundles the SQLite-backed readers the snapshot builder
// needs. Grouped into one struct so NewStreamClient stays readable when
// the caller site adds another store (repo store, workflow store) — new
// fields land here rather than widening the constructor signature.
type StreamStores struct {
	Sessions  SessionSnapshotReader
	Chats     ChatSnapshotReader
	Repos     RepoSnapshotReader
	Statuses  StatusSnapshotReader
	Interests CallbackInterestReader
}

// SessionSnapshotReader returns the slim projection of every active
// session the daemon currently knows about. Built to take the *pb.Session
// directly so the snapshot path never touches models.Session — that
// conversion happens once in the adapter, not per-field here.
type SessionSnapshotReader interface {
	SnapshotSessions(ctx context.Context) ([]*pb.Session, error)
}

// ChatSnapshotReader returns the ClaudeChat metadata projection (no
// transcripts — just the preview). Kept separate from SessionSnapshot so
// the two calls can run in parallel if that ever becomes a hotspot.
type ChatSnapshotReader interface {
	SnapshotChats(ctx context.Context) ([]*pb.ClaudeChatMetadata, error)
}

// RepoSnapshotReader lists the repos the daemon is currently managing.
// Snapshot uses the IDs only; the full Repo proto isn't sent up.
type RepoSnapshotReader interface {
	SnapshotRepoIDs(ctx context.Context) ([]string, error)
}

// StatusSnapshotReader returns the current ChatStatusEntry set from the
// in-memory chat-status tracker.
type StatusSnapshotReader interface {
	SnapshotStatuses(ctx context.Context) ([]*pb.ChatStatusEntry, error)
}

// CallbackInterestReader returns the daemon's complete current GitHub
// callback-interest set (distinct repo_origin_url + pr_number over every
// non-terminal callback). Carried on the DaemonSnapshot so bosso can reconcile
// callback interests atomically on every (re)connect. The bossd wiring adapts
// callback.DeriveInterests.
type CallbackInterestReader interface {
	SnapshotCallbackInterests(ctx context.Context) ([]*pb.CallbackInterest, error)
}

// StreamEvent is the union of session/chat/status events the daemon
// publishes internally for the reverse stream. It intentionally does not
// reuse the plugin-facing EventNotification (which has a disjoint oneof
// set) — keeping stream events in-package avoids enlarging the plugin
// proto for a purely internal pipeline.
type StreamEvent struct {
	// Exactly one of the following is non-nil.
	Session         *SessionEvent
	Chat            *ChatEvent
	Status          *StatusEvent
	Interests       *InterestsEvent
	EgressBroadcast *BroadcastEgressEvent
}

// BroadcastEgressEvent carries an outbound cross-daemon broadcast up the
// reverse stream: "this daemon originated a broadcast whose audience reaches
// beyond itself; route it". It wraps the already-built proto rather than
// restating its fields so the envelope never holds a second, drifting copy.
//
// SECRET BODY: Egress.Message is the broadcast prompt. It rides the bus to
// bosso for delivery and MUST NEVER be logged, echoed on a read surface, or
// put in an error detail — the same rule daemon.proto states on
// Broadcast.message. Nothing in this package logs the contents of this event;
// log broadcast id, origin daemon id, and counts only.
type BroadcastEgressEvent struct {
	Egress *pb.BroadcastEgress
}

// InterestsEvent carries the daemon's complete current GitHub callback-interest
// set as a steady-state refresh. It has snapshot semantics: the full set every
// time, and an empty slice is a valid message meaning "withdraw all" (the daemon
// holds no live callbacks). The connect/reconnect DaemonSnapshot carries the
// guaranteed-first full set; this event carries subsequent changes.
type InterestsEvent struct {
	Interests []*pb.CallbackInterest
}

// SessionEvent describes a session lifecycle change. Kind mirrors the
// SessionDelta proto one-to-one; Session is omitted on delete.
type SessionEvent struct {
	Kind    pb.SessionDelta_Kind
	Session *pb.Session
}

// ChatEvent describes a chat lifecycle change.
type ChatEvent struct {
	Kind pb.ChatDelta_Kind
	Chat *pb.ClaudeChatMetadata
}

// StatusEvent is a raw chat-status heartbeat, pre-coalescing. The
// coalescer (T3.4) dedupes bursts before they reach the wire.
type StatusEvent struct {
	Status *pb.ChatStatusDelta
}

// EventSource is the subscribe-side of the daemon's internal stream
// event bus. Implementations close the returned channel when ctx is
// cancelled or the source shuts down. A concrete eventbus adapter lands
// in T3.7 when the publishers (lifecycle, chat store, display computer)
// get wired to it.
type EventSource interface {
	Subscribe(ctx context.Context) <-chan StreamEvent
}

// Metrics is an optional sink for stream-level counters. The production
// wiring lands in a later task; callers that don't care can pass nil.
type Metrics interface {
	IncReconnect()
	IncStreamError(err error)
}

// noopMetrics is the default when the caller passes nil. Having a real
// zero-value receiver means every metric call site can skip nil checks.
type noopMetrics struct{}

func (noopMetrics) IncReconnect()          {}
func (noopMetrics) IncStreamError(_ error) {}

// StreamClient runs the bossd side of the reverse-stream protocol. It
// opens a long-lived DaemonStream, sends an initial DaemonSnapshot, then
// forwards session/chat/status deltas plus token refreshes until the
// stream terminates. The outer Run loop reconnects with exponential
// backoff on all errors other than context cancellation.
// ReRegisterFunc obtains a fresh daemon session_token by calling
// RegisterDaemon again. Invoked by the Run loop after bosso returns
// CodeUnauthenticated on DaemonStream — the common cause is that another
// bossd with the same daemon_id re-registered and rotated the token, or
// that bosso's daemons row for us was cleared. Callers wrap
// upstream.Register with the daemon's identity + current JWT.
type ReRegisterFunc func(ctx context.Context) (sessionToken string, err error)

type StreamClient struct {
	opener           streamOpener
	stores           StreamStores
	events           EventSource
	tokenProvider    TokenProvider
	commandHandler   SessionCommandHandler
	transferHandler  TransferHandler
	webhooks         WebhookCommandDispatcher
	attacher         SessionAttacher
	creator          SessionCreator
	reRegister       ReRegisterFunc
	authState        *AuthState
	onHandshake      func()
	daemonID         string
	hostname         string
	logger           zerolog.Logger
	metrics          Metrics
	clock            Clock
	coalesceWindow   time.Duration
	refreshInterval  time.Duration
	refreshThreshold time.Duration

	// reconnectCh is a coalesced wake signal for the Run loop. Reconnect()
	// does a non-blocking send here; the loop's backoff select drains it to
	// skip the remaining sleep and re-open immediately. Buffered size 1 so a
	// signal arriving while the loop is mid-open is not lost, and repeated
	// signals coalesce into one pending wake. Used by the login path
	// (streamAuthAdapter.NotifyLogin) after a proactive re-register so the
	// stream re-opens with the fresh token without waiting out the backoff.
	reconnectCh chan struct{}

	// connected flips to true once Send(snapshot) succeeds on a stream.
	// Reset across reconnects so the backoff resets only on a fresh
	// successful handshake, not on dial-level flakes.
	mu        sync.Mutex
	connected bool
	// streamOpen is the LIVE connectivity bit, distinct from `connected`.
	// `connected` is a latch the backoff logic reads AFTER openStream has
	// returned ("did the last attempt reach snapshot-accepted?"), so it must
	// survive the teardown; a diagnostic that reused it would report
	// `stream connected: true` for the whole reconnect backoff gap, which is
	// exactly when an operator runs `boss daemon doctor`. streamOpen is
	// cleared the moment openStream returns.
	streamOpen bool
	// streamOpenedAt is when the currently-open stream reached
	// snapshot-accepted; zero when no stream is open. Read only through
	// streamSustainedLocked. It is the clock behind streamAuthSustainedFor.
	streamOpenedAt time.Time
	// authFailingSince is the instant the current unbroken run of
	// unrecoverable auth failures began; zero when auth is not failing. It is
	// the discriminating fact for the BOS-942 wedge — a daemon whose
	// NeedsLogin() reads false while re-registration fails every 30s — and it
	// is read by BOTH the periodic wedge WARN and the GetAuthState RPC, so the
	// log and `boss daemon doctor` cannot disagree about how long the daemon
	// has been broken.
	authFailingSince time.Time

	asyncMu      sync.Mutex
	asyncCancels map[string]context.CancelFunc
	asyncDone    map[string]<-chan struct{}
	// asyncCanceled records bounded, short-lived tombstones for CommandCancel
	// frames that arrive before the async command they target is registered.
	asyncCanceled map[string]time.Time
	// authStuckStreak counts consecutive suppressed (debug-level) auth
	// failures since the last WARN. Crossing authWedgeWarnBudget re-states the
	// wedge in the log and resets the counter.
	authStuckStreak int
	// zeroExpiryWarned gates the token refresher's zero-expiry-with-a-relogin
	// -reason warning to once per transition, mirroring the authStuck idiom.
	zeroExpiryWarned bool
}

// AuthSnapshot is the immutable view of the stream client's auth-relevant
// state that the GetAuthState RPC reports. Deliberately tiny and free of any
// token material.
type AuthSnapshot struct {
	// Connected reports whether a stream is open RIGHT NOW — it is set when
	// the snapshot is accepted and cleared as soon as that attempt returns.
	// It reads false during an ordinary reconnect gap on a healthy daemon, so
	// it is context, never a failure verdict on its own. In particular it is
	// NOT evidence that the upstream accepted our credentials: the snapshot
	// Send completes locally before bosso's header-only rejection arrives.
	Connected bool
	// AuthFailingSince is the start of the current unbroken run of
	// unrecoverable auth failures; zero when auth is not currently failing.
	AuthFailingSince time.Time
}

// AuthSnapshot returns the current auth-relevant stream state. Safe for
// concurrent callers.
func (c *StreamClient) AuthSnapshot() AuthSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	failingSince := c.authFailingSince
	if c.streamSustainedLocked() {
		// A stream that has stayed open past streamAuthSustainedFor is
		// proof the credentials were accepted, and it is the ONLY proof
		// available on an idle stream (see streamAuthSustainedFor). The
		// stored field is left alone rather than cleared here: a read
		// path that mutates would make the wedge state depend on whether
		// anyone happened to run the doctor. markStreamClosed does the
		// real clear when this same stream ends.
		failingSince = time.Time{}
	}
	return AuthSnapshot{Connected: c.streamOpen, AuthFailingSince: failingSince}
}

// streamSustainedLocked reports whether a stream is open RIGHT NOW and has
// been for at least streamAuthSustainedFor. Caller must hold c.mu.
func (c *StreamClient) streamSustainedLocked() bool {
	if !c.streamOpen || c.streamOpenedAt.IsZero() {
		return false
	}
	return c.clock.Now().Sub(c.streamOpenedAt) >= streamAuthSustainedFor
}

// authFailureNote is what one unrecoverable auth failure tells the caller.
type authFailureNote struct {
	// Escalate is true when this iteration must log at WARN rather than DEBUG.
	Escalate bool
	// First is true only for the opening failure of a run. It is escalated,
	// but it is not yet evidence of a wedge — one rejected open is ordinary.
	// Only a re-escalation after a full budget of suppressed repeats is.
	First bool
	// Since is the start of the current failure run.
	Since time.Time
}

// noteAuthFailure records one unrecoverable auth failure and reports whether
// this iteration should escalate to a WARN. It escalates on the FIRST failure
// of a run (the streak starts fresh) and again every time the suppressed
// streak crosses authWedgeWarnBudget, resetting the streak so the next
// escalation needs a full budget again.
//
// It also stamps authFailingSince on the first failure of a run, which is what
// makes the log line's duration and the doctor's duration the same number.
func (c *StreamClient) noteAuthFailure() authFailureNote {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authFailingSince.IsZero() {
		c.authFailingSince = c.clock.Now()
		c.authStuckStreak = 0
		return authFailureNote{Escalate: true, First: true, Since: c.authFailingSince}
	}
	c.authStuckStreak++
	if c.authStuckStreak >= authWedgeWarnBudget {
		c.authStuckStreak = 0
		return authFailureNote{Escalate: true, Since: c.authFailingSince}
	}
	return authFailureNote{Since: c.authFailingSince}
}

// clearAuthFailure resets the wedge state after a recovery — an attempt that
// reached the handshake and then died of something that was NOT an auth
// rejection, or the post-login NeedsLogin reset — so the next failure warns as
// a first again. The whole state machine resets, not just the log level.
// markStreamClosed clears it too, for a stream that stayed open long enough to
// prove itself (streamAuthSustainedFor).
//
// Note what is NOT on that list: a successful re-register. Rotating a token
// proves only that a token was stored, so clearing the clock there hides the
// rotate-and-reject loop (see the Run loop's hotRetry comment).
func (c *StreamClient) clearAuthFailure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetAuthFailureLocked()
}

// noteUpstreamAccepted records the strongest positive proof bosso accepted
// this daemon's credentials: a message arriving FROM it. A successful Send
// proves nothing (see markConnected), a successful re-register proves only
// that a token was stored (see the Run loop's hotRetry comment), and the
// loop's remaining recovery signals — a post-login reset, an attempt that
// reached the handshake and then died of something other than an auth
// rejection — are inferences drawn after the stream is already gone.
//
// It is direct, but it is not guaranteed to ever arrive: bosso sends a command
// only when a webhook, a UI action, or a cron produces one, so an idle stream
// can be healthy for hours without delivering a single one. That gap is why
// streamAuthSustainedFor exists as a second live signal; this one is what
// makes a busy daemon recover immediately rather than after the grace period.
func (c *StreamClient) noteUpstreamAccepted() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetAuthFailureLocked()
}

// noteZeroExpiry reports whether the token refresher should warn about a
// zero-expiry-with-a-relogin-marker provider on this tick. True exactly once
// per transition: the flag is cleared only by a recovery (see
// resetAuthFailureLocked), so a permanent condition costs one line, not one
// line per refreshInterval.
//
// Note the deliberate cross-component coupling, because it looks like a bug
// from either side: this gate belongs to the token REFRESHER, but the only
// thing that reopens it is a DaemonStream recovery (noteUpstreamAccepted, a
// stream that stayed open past streamAuthSustainedFor, a clean non-auth close
// after a handshake, or a post-login reset — every path that runs
// resetAuthFailureLocked). That is intentional and load-bearing. The condition
// the warning describes — a provider reloaded from a re-login-marked record —
// can only end when fresh credentials arrive, and the observable proof that
// they did is a stream that authenticates again. The refresher itself never
// sees that moment: it skips every tick while ExpiresAt() is zero, so a gate
// cleared from inside the refresher would either never reopen or reopen on a
// tick that proves nothing. Do not "decouple" this by clearing the flag on a
// refresher tick; that reintroduces one warn per refreshInterval for a
// condition that is permanent until `boss login`.
func (c *StreamClient) noteZeroExpiry() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.zeroExpiryWarned {
		return false
	}
	c.zeroExpiryWarned = true
	return true
}

// resetAuthFailureLocked clears the wedge state. Caller holds c.mu.
//
// zeroExpiryWarned is cleared here even though nothing in the stream loop sets
// it — the token refresher does (noteZeroExpiry). That coupling is deliberate:
// a recovery is the ONLY event that can end the condition that warning
// describes, and the refresher cannot observe one because it skips every tick
// while the expiry is zero. See noteZeroExpiry for the full argument before
// deciding this line is a stray.
func (c *StreamClient) resetAuthFailureLocked() {
	c.authFailingSince = time.Time{}
	c.authStuckStreak = 0
	c.zeroExpiryWarned = false
}

// StreamClientConfig bundles the constructor inputs. Pointer fields are
// optional; leaving them nil picks a sensible default.
type StreamClientConfig struct {
	// Opener is the live ConnectRPC client. If nil, the client will be
	// built from Client + AuthToken at construction time.
	Opener    streamOpener
	Client    bossanovav1connect.OrchestratorServiceClient
	AuthToken string // WorkOS JWT for Authorization header
	// SessionToken, when non-nil, is the shared holder for the daemon
	// session_token sent in X-Daemon-Token. The same holder should be
	// passed to any peer openers (e.g. TerminalStream) so a rotation
	// triggered by re-register fans out to all of them. If nil and
	// Opener is nil, NewStreamClient creates an empty holder; tests that
	// supply their own Opener can ignore this field.
	SessionToken *SessionTokenHolder

	// Identity.
	DaemonID string
	Hostname string

	// Data sources.
	Stores        StreamStores
	Events        EventSource
	TokenProvider TokenProvider

	// Command-side collaborators.
	CommandHandler SessionCommandHandler
	// TransferHandler is optional. When nil, the dispatcher ACKs
	// TransferConfirmed and TransferCancel as idempotent no-ops and fails
	// the initial Transfer command with "not yet implemented" — preserving
	// the T3.6 behaviour. Real daemons wire a concrete implementation in
	// the task that lands the source/target session-lifecycle work.
	TransferHandler TransferHandler
	Webhooks        WebhookCommandDispatcher
	Attacher        SessionAttacher
	// Creator, when set, handles streaming CreateSessionCommands from bosso.
	// Wired the same way as Attacher. Nil is safe — the dispatcher returns a
	// "creator not wired" CommandResult.
	Creator SessionCreator

	// ReRegister, when set, is called by the Run loop after a stream
	// attempt fails with CodeUnauthenticated. On success the returned
	// session_token replaces the one in the opener so the next reconnect
	// authenticates cleanly. Nil is safe — callers who don't wire it
	// keep the previous tight-loop behaviour.
	ReRegister ReRegisterFunc

	// AuthState, when set, lets the opener flag a terminal credential
	// failure (a WorkOS rejection, or an unconfirmed exchange outcome —
	// both re-login states) and the Run loop pause until
	// NotifyLogin clears it. Production wiring should pass the same
	// AuthState to the TerminalStreamClient so both bidis pause together.
	// Nil keeps the legacy tight-loop behaviour.
	AuthState *AuthState

	// OnHandshake, when set, is invoked after every successful snapshot
	// handshake — i.e. once per registration bosso observes for this
	// daemon, including reconnects. bosso builds a fresh DaemonState per
	// registration, so any sibling stream bound to the prior state (the
	// TerminalStream sender) is stranded from that moment on. Production
	// wiring points this at TerminalStreamClient.CycleStream so the
	// terminal sender rebinds to the current DaemonState after every
	// registration (2026-07-11 incident). The hook runs on the openStream
	// goroutine — keep it non-blocking. Nil is safe.
	OnHandshake func()

	// Observability / testing knobs.
	Logger  zerolog.Logger
	Metrics Metrics
	Clock   Clock // nil → realClock
	// CoalesceWindow is the ChatStatus flush interval (T3.4). Zero picks
	// the default 100ms.
	CoalesceWindow time.Duration
	// RefreshInterval is how often the token refresher wakes. Zero picks
	// 60s (decision #2).
	RefreshInterval time.Duration
	// RefreshThreshold is how much headroom we keep before expiry before
	// forcing a refresh. Zero picks 60s.
	RefreshThreshold time.Duration
}

// NewStreamClient assembles the client from the given config. Pointer
// fields in StreamClientConfig are optional — defaults are filled in
// here so the caller site stays compact.
func NewStreamClient(cfg StreamClientConfig) *StreamClient {
	opener := cfg.Opener
	if opener == nil && cfg.Client != nil {
		holder := cfg.SessionToken
		if holder == nil {
			holder = NewSessionTokenHolder("")
		}
		opener = &connectOpener{
			client:       cfg.Client,
			authToken:    cfg.AuthToken,
			sessionToken: holder,
			tokens:       cfg.TokenProvider,
			authState:    cfg.AuthState,
			logger:       cfg.Logger.With().Str("component", "stream-opener").Logger(),
		}
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = noopMetrics{}
	}
	clock := cfg.Clock
	if clock == nil {
		clock = realClock{}
	}
	window := cfg.CoalesceWindow
	if window == 0 {
		window = 100 * time.Millisecond
	}
	refreshInterval := cfg.RefreshInterval
	if refreshInterval == 0 {
		refreshInterval = defaultRefreshInterval
	}
	refreshThreshold := cfg.RefreshThreshold
	if refreshThreshold == 0 {
		refreshThreshold = defaultRefreshThreshold
	}
	return &StreamClient{
		opener:           opener,
		reconnectCh:      make(chan struct{}, 1),
		stores:           cfg.Stores,
		events:           cfg.Events,
		tokenProvider:    cfg.TokenProvider,
		commandHandler:   cfg.CommandHandler,
		transferHandler:  cfg.TransferHandler,
		webhooks:         cfg.Webhooks,
		attacher:         cfg.Attacher,
		creator:          cfg.Creator,
		reRegister:       cfg.ReRegister,
		authState:        cfg.AuthState,
		onHandshake:      cfg.OnHandshake,
		daemonID:         cfg.DaemonID,
		hostname:         cfg.Hostname,
		logger:           cfg.Logger.With().Str("component", "stream-client").Logger(),
		metrics:          metrics,
		clock:            clock,
		coalesceWindow:   window,
		refreshInterval:  refreshInterval,
		refreshThreshold: refreshThreshold,
	}
}

// Run is the reconnect outer loop. It returns only when ctx is cancelled.
// On every stream-close error the loop backs off (1s → 30s cap) before
// retrying. Successful stream completion (Send of snapshot + at least
// one round-trip) resets the backoff to 1s so a one-off flake doesn't
// escalate.
func (c *StreamClient) Run(ctx context.Context) {
	backoff := streamInitialBackoff
	// The "we're in a sustained CodeUnauthenticated loop where ReRegister also
	// can't recover" state used to be a local bool. It now lives on the client
	// under c.mu (authFailingSince / authStuckStreak) because the GetAuthState
	// RPC has to read it from another goroutine: the whole point of BOS-944 is
	// that this loop is the only place that knows the daemon is wedged, and a
	// function-local flag is unreachable from the RPC handler.
	c.clearAuthFailure()
	for {
		if ctx.Err() != nil {
			return
		}

		// If the opener has flagged the refresh token as terminally dead,
		// stop dialling. The previous behaviour was to retry every 1s
		// forever, filling the log with "invalid credentials". Block on
		// the AuthState wake channel until NotifyLogin clears it (or ctx
		// cancels), then drop straight back into the dial path.
		if c.authState != nil && c.authState.NeedsLogin() {
			c.logger.Warn().Msg("stream paused: re-login required (waiting for boss login)")
			select {
			case <-ctx.Done():
				return
			case <-c.authState.Wait():
				c.logger.Info().Msg("stream resumed after re-login signal")
			}
			// Reset backoff and the wedge state so the post-login dial
			// doesn't inherit a long backoff — or a stale authFailingSince —
			// from the paused era.
			backoff = streamInitialBackoff
			c.clearAuthFailure()
			continue
		}

		c.markDisconnected()
		err := c.openStream(ctx)
		// authWedged is set by the default arm below when this attempt died
		// on credentials we could not repair. It is hoisted out of the switch
		// because the exponential ramp at the bottom of the loop needs it:
		// see the comment there.
		authWedged := false
		switch {
		case ctx.Err() != nil:
			return
		case c.authState != nil && c.authState.NeedsLogin():
			// openStream returned because logout cancelled the inner
			// streamCtx (the outer ctx is still live, so the case above
			// didn't fire). This is an intentional pause, not a stream
			// failure — skip IncStreamError, the "reconnecting" warn, and
			// the backoff sleep, and loop straight to the NeedsLogin() gate
			// at the top, which logs the pause once and blocks on Wait()
			// until the next login. Without this, a clean logout surfaces a
			// misleading reconnect warning and an unnecessary backoff delay.
			continue
		case err == nil:
			// Stream closed cleanly (server shutdown etc). Reset backoff
			// and try again immediately — this is usually a recycle, not
			// a sustained outage.
			backoff = streamInitialBackoff
			c.clearAuthFailure()
		default:
			c.metrics.IncStreamError(err)
			// CodeUnauthenticated from the stream means bosso rejected
			// our credentials. The JWT is checked on open too, but the
			// typical cause is a stale session_token (another bossd
			// with the same daemon_id rotated it via UPSERT, or bosso's
			// daemons row for us was cleared). Without self-healing the
			// outer loop tight-loops forever presenting the same bad
			// token. Call ReRegister on any Unauthenticated; if the JWT
			// is also bad it'll fail and we fall through to regular
			// backoff.
			authFailed := connect.CodeOf(err) == connect.CodeUnauthenticated
			rotated := false
			// hotRetry means "this rotation is still plausibly a recovery".
			// It is true only for the FIRST rejection of a run, and it is
			// deliberately NOT the same thing as `rotated`.
			//
			// tryReRegister reports that a NEW token was STORED, never that it
			// authenticates — it even answers true from its "someone else
			// already rotated it" fallback. So in the rotate-and-reject shape
			// (ReRegister keeps succeeding while bosso keeps rejecting the
			// stream) `rotated` is true on every single iteration. Clearing the
			// wedge clock there — which is what this code used to do — re-stamps
			// authFailingSince forever: the duration never accumulates, the
			// suppression budget is never reached, and GetAuthState answers
			// `boss daemon doctor` with "signed in" for a daemon that has not
			// authenticated in hours. That is the BOS-942 blind spot this whole
			// change exists to close, reproduced one layer up from markConnected.
			//
			// So a rotation buys exactly one hot retry per failure run, and
			// nothing else: the clock keeps running until something PROVES the
			// credentials work (noteUpstreamAccepted), or a login resets it.
			hotRetry := false
			var note authFailureNote
			if authFailed {
				// Order matters. tryReRegister takes the "suppress the
				// failure warn" flag, and the log gate below is eleven lines
				// further on, so the escalation decision has to be made HERE,
				// before the call. Deciding at the log gate would hand
				// tryReRegister the previous iteration's answer and put its
				// warning one iteration out of step with the loop's own.
				note = c.noteAuthFailure()
				rotated = c.tryReRegister(ctx, !note.Escalate)
				hotRetry = rotated && note.First
			}
			authWedged = authFailed && !hotRetry
			// Reduce log spam on a sustained auth loop while refusing to go
			// permanently silent. The first failure of a run warns; the
			// repeats drop to debug; every authWedgeWarnBudget suppressed
			// repeats the wedge is re-stated at warn with how long it has been
			// going on. Before BOS-944 the suppression was unbounded, so a
			// daemon that had been unable to re-register for six hours looked
			// in the log exactly like one that recovered after the first
			// warning.
			//
			// The escalated line carries auth_failing_since / auth_failing_for
			// from the same c.mu-guarded state the GetAuthState RPC reads, so
			// `boss daemon doctor` and the log cannot disagree about the
			// duration.
			//
			// Note: terminal_stream.go has a structurally similar per-terminal
			// retry loop, and it is deliberately left alone. Its failures are
			// per-terminal and already bounded by terminalReadyTimeoutBudget;
			// this gate is about a process-wide upstream credential wedge,
			// which is a different fault with a different audience. Please
			// don't "unify" them.
			switch {
			case authFailed && !note.Escalate:
				c.logger.Debug().Err(err).Dur("backoff", backoff).Msg("stream closed, reconnecting (auth still failing)")
			case authFailed:
				// EVERY escalated auth line carries the same fields — the
				// first failure of a run as well as each budget re-statement.
				// The first one used to fall through to the generic arm
				// below, which meant the opening line of a wedge, the one an
				// operator reads first when they go looking, was the only one
				// that could not tell them which wedge it was or when it
				// started.
				event := c.logger.Warn().Err(err).
					Dur("backoff", backoff).
					Time("auth_failing_since", note.Since).
					Dur("auth_failing_for", c.clock.Now().Sub(note.Since))
				// The enumerated reason is the one field that says WHICH
				// wedge this is; it is empty for providers that cannot carry
				// one, so only attach it when there is something to say.
				if reason := providerReloginReason(c.tokenProvider); reason != "" {
					event = event.Str("relogin_reason", reason)
				}
				// The wording still separates the two, because they are
				// different claims: one rejected open is ordinary and often
				// self-heals on the next dial, while a re-statement after a
				// full budget of suppressed repeats is evidence of a wedge
				// that needs a human. Only the latter tells anyone to log in.
				if note.First {
					event.Msg(streamAuthRejectedMsg)
				} else {
					event.Msg(streamAuthWedgedMsg)
				}
			default:
				c.logger.Warn().Err(err).Dur("backoff", backoff).Msg("stream closed, reconnecting")
			}
			// Reset backoff only when we actually progressed:
			//   - non-auth error after a real handshake (one-off flake)
			//   - the FIRST auth error of a run where ReRegister got us a
			//     fresh token (plausible recovery; retry now while the token
			//     is hot). Only the first: a rotation that keeps happening on
			//     every iteration is a rotate-and-reject loop, not progress,
			//     and pinning the backoff at 1s there tight-loops against
			//     bosso and blows the log budget this change exists to bound.
			// A bare CodeUnauthenticated where ReRegister also failed
			// means we're stuck on bad credentials. wasConnected() is
			// misleading there because Send(snapshot) succeeds locally
			// before bosso's header-only auth rejection arrives, so
			// every attempt looks "connected" and would reset the
			// backoff to 1s — that's what was filling the log.
			switch {
			case hotRetry:
				backoff = streamInitialBackoff
			case !authFailed && c.wasConnected():
				backoff = streamInitialBackoff
				// Same evidence, second conclusion: this attempt reached
				// snapshot-accepted and then died of something that was NOT
				// an auth rejection, so whatever wedge was running has ended.
				// markConnected can no longer draw that conclusion for us —
				// it fires before the rejection is observable — so it is
				// drawn here, where `authFailed` is actually known.
				c.clearAuthFailure()
			}
		}

		if ctx.Err() != nil {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-c.reconnectCh:
			// A proactive re-register (login) asked us to re-open now.
			// Reset the backoff and skip the exponential ramp below so the
			// reconnect happens immediately with the fresh token.
			backoff = streamInitialBackoff
			continue
		case <-c.clock.After(backoff):
		}

		// Exponential ramp, capped at streamMaxBackoff. Grow when the last
		// attempt was a dead-on-arrival failure (no handshake) — and also when
		// it died on credentials we could not repair, even though it DID reach
		// the handshake. That second clause is the same trap the backoff-reset
		// switch above already documents, arriving one branch later: bosso
		// checks auth on the headers and reports it via Receive, so the
		// snapshot Send of a wedged daemon succeeds and wasConnected() reads
		// true on every attempt. Ramping on wasConnected() alone therefore
		// pins a wedged daemon at the 1s initial backoff forever — the tight
		// reconnect loop this cap exists to prevent, and 24x the log volume
		// the suppression budget is sized against, since that budget counts
		// loop iterations rather than wall-clock time.
		if !c.wasConnected() || authWedged {
			backoff *= 2
			if backoff > streamMaxBackoff {
				backoff = streamMaxBackoff
			}
			c.metrics.IncReconnect()
		}
	}
}

// openStream runs a single stream attempt end-to-end: build snapshot,
// open the bidi stream, send snapshot, fan out forwarders, block on
// Receive() for commands. Returns when the stream dies for any reason.
func (c *StreamClient) openStream(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	authWatchDone := cancelOnNeedsLogin(streamCtx, c.authState, cancel)
	defer func() {
		cancel()
		<-authWatchDone
	}()

	stream := c.opener.DaemonStream(streamCtx)

	// 1. Build + send the snapshot. Any error here is fatal for this
	//    attempt — bosso rejects streams whose first event isn't a
	//    snapshot, so there's no point forwarding deltas afterwards.
	snap, err := c.buildSnapshot(streamCtx)
	if err != nil {
		return fmt.Errorf("build snapshot: %w", err)
	}
	if err := stream.Send(&pb.DaemonEvent{
		Event: &pb.DaemonEvent_Snapshot{Snapshot: snap},
	}); err != nil {
		return fmt.Errorf("send snapshot: %w", err)
	}

	// Mark connected only after a successful handshake. The outer loop
	// uses this to decide whether to reset backoff on the next error, so the
	// latch has to outlive this function — only the live-connectivity bit is
	// dropped on the way out.
	c.markConnected()
	defer c.markStreamClosed()

	// Every successful handshake corresponds to a fresh DaemonState on
	// bosso; let sibling streams (TerminalStream) rebind to it.
	if c.onHandshake != nil {
		c.onHandshake()
	}

	// 2. Spin up the outbound writer. A single goroutine owns the
	//    stream.Send side so snapshots/deltas/results don't race one
	//    another inside ConnectRPC's framer.
	outbound := make(chan *pb.DaemonEvent, 64)
	writerDone := safego.Go(c.logger, func() {
		c.runWriter(streamCtx, stream, outbound)
	})

	// 3. Delta forwarder.
	forwarderDone := safego.Go(c.logger, func() {
		c.subscribeDeltas(streamCtx, outbound)
	})

	// 4. Token refresher. Drives outbound on refresh; returning an error
	//    here closes the stream so the outer loop reconnects with a
	//    fresh token — matches decision #2 ("refresh failure → close
	//    stream → reconnect").
	refreshErrCh := make(chan error, 1)
	refresherDone := safego.Go(c.logger, func() {
		if err := c.runTokenRefresher(streamCtx, outbound); err != nil {
			refreshErrCh <- err
			cancel()
		}
		close(refreshErrCh)
	})

	// 5. Command reader — runs on this goroutine. Receive is blocking
	//    and must be owned by exactly one caller per connect semantics.
	readErr := c.runCommandReader(streamCtx, stream, outbound)
	readCtxErr := streamCtx.Err()

	// Tear down in the reverse order we started. Close the outbound
	// channel so the writer exits, then wait for all children so a
	// subsequent reconnect attempt sees a clean slate.
	cancel()
	c.cancelAndWaitAsyncCommands()

	// Drain the refresh error channel so its error takes precedence
	// over the generic EOF from Receive when a refresh forced the
	// close. Matches decision #2's "close stream so outer loop
	// reconnects" semantics.
	var refreshErr error
	select {
	case refreshErr = <-refreshErrCh:
	default:
	}

	close(outbound)
	<-writerDone
	<-forwarderDone
	<-refresherDone

	if refreshErr != nil {
		return refreshErr
	}
	if readErr == nil && readCtxErr != nil {
		return readCtxErr
	}
	return readErr
}

// runWriter is the single-writer goroutine that drains the outbound
// channel onto the stream. Exits when outbound is closed or Send fails
// (the failure propagates up via the command reader's next Receive).
func (c *StreamClient) runWriter(ctx context.Context, stream bidirectionalStream, outbound <-chan *pb.DaemonEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-outbound:
			if !ok {
				return
			}
			if err := stream.Send(ev); err != nil {
				c.logger.Debug().Err(err).Msg("stream send failed, writer exiting")
				return
			}
		}
	}
}

// runCommandReader blocks on stream.Receive and dispatches each inbound
// command. Returns when Receive returns an error (EOF, reset, ctx) so
// the outer loop can decide whether to reconnect.
func (c *StreamClient) runCommandReader(ctx context.Context, stream bidirectionalStream, outbound chan<- *pb.DaemonEvent) error {
	// accepted keeps the wedge-clearing call to once per stream without a
	// second field or a lock acquisition per inbound command.
	accepted := false
	for {
		cmd, err := stream.Receive()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive: %w", err)
		}
		if !accepted {
			accepted = true
			c.noteUpstreamAccepted()
		}
		c.handleCommand(ctx, cmd, outbound)
	}
}

// handleCommand dispatches a single inbound command. Kept as a separate
// method (rather than inlined into runCommandReader) so command_dispatcher.go
// can hang the per-oneof logic off it without widening the reader.
func (c *StreamClient) handleCommand(ctx context.Context, cmd *pb.OrchestratorCommand, outbound chan<- *pb.DaemonEvent) {
	result := c.dispatchCommand(ctx, cmd, outbound)
	if result == nil {
		// Attach + unknown commands return nil because they either stream
		// chunks asynchronously (attach) or emit nothing (unknown).
		return
	}
	select {
	case outbound <- result:
	case <-ctx.Done():
	}
}

// Reconnect wakes the Run loop out of its backoff sleep so it re-opens the
// stream immediately instead of waiting out the remaining delay. It is
// non-blocking and coalesced: the signal lands in a single-slot buffer, so
// repeated calls collapse into one pending wake and a call made while the
// loop is not currently sleeping is remembered until it next reaches the
// backoff select. Nil-safe — a nil receiver (local-only mode never builds a
// StreamClient) is a no-op so callers need not nil-check.
//
// The login path calls this after a proactive re-register obtains a fresh
// session token, so the stream re-authenticates without a restart.
func (c *StreamClient) Reconnect() {
	if c == nil {
		return
	}
	select {
	case c.reconnectCh <- struct{}{}:
	default:
	}
}

// tryReRegister invokes the configured ReRegister callback (if any) to
// obtain a fresh daemon session_token and, on success, rotates the
// opener's token so the next DaemonStream open authenticates with it.
// Returns true iff the opener was updated. Safe to call when
// ReRegister is nil (returns false without work) or when the opener
// does not implement sessionTokenHolder (logs + returns false).
func (c *StreamClient) tryReRegister(ctx context.Context, suppressFailureWarn bool) bool {
	if c.reRegister == nil {
		return false
	}
	holder, ok := c.opener.(sessionTokenHolder)
	if !ok {
		c.logger.Warn().Msg("stream: opener does not support session token rotation; skipping re-register")
		return false
	}
	failedToken := holder.SessionToken()
	tok, err := c.reRegister(ctx)
	if err != nil {
		if suppressFailureWarn {
			c.logger.Debug().Err(err).Msg("stream: re-register still failing after auth rejection")
		} else {
			c.logger.Warn().Err(err).Msg("stream: re-register failed after auth rejection")
		}
		return false
	}
	if tok == "" {
		c.logger.Warn().Msg("stream: re-register returned empty session token; skipping rotation")
		return false
	}
	if holder.CompareAndSwapSessionToken(failedToken, tok) {
		c.logger.Info().Msg("stream: rotated session_token after auth rejection")
		return true
	}
	if holder.SessionToken() != "" {
		c.logger.Info().Msg("stream: session_token already rotated after auth rejection")
		return true
	}
	holder.SetSessionToken(tok)
	c.logger.Info().Msg("stream: rotated session_token after auth rejection")
	return true
}

// markConnected / markDisconnected / wasConnected are the tiny state
// machine that tells Run whether the last attempt reached the "snapshot
// accepted" point. Backoff resets only when this is true.
//
// It deliberately does NOT clear the auth-wedge state. Reaching this point
// means our own Send(snapshot) returned nil, which is a purely LOCAL fact:
// Run's own comment on the backoff switch records that bosso's header-only
// auth rejection arrives later, from Receive, so "every attempt looks
// connected". Clearing the wedge here would therefore re-stamp
// authFailingSince on every reconnect of a genuinely wedged daemon — the
// duration would never accumulate, the suppression budget would never be
// reached (so the WARN would fire on every single iteration), and
// GetAuthState would answer `boss daemon doctor` with a wedge that looks
// seconds old or absent. That is the BOS-942 blind spot this whole change
// exists to remove. See noteUpstreamAccepted for what does clear it.
func (c *StreamClient) markConnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
	c.streamOpen = true
	c.streamOpenedAt = c.clock.Now()
}

func (c *StreamClient) markDisconnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
	c.streamOpen = false
	c.streamOpenedAt = time.Time{}
}

// markStreamClosed drops only the live-connectivity bit, leaving `connected`
// latched for the backoff decision Run makes right after openStream returns.
//
// A stream that lasted at least streamAuthSustainedFor also clears the wedge
// state on the way out, and it does so BEFORE Run classifies the error that
// ended it: a daemon whose credentials were accepted for hours and is only now
// being rejected is starting a NEW failure run, so the next failure must warn
// as a first and its duration must count from now, not from the wedge that
// ended when this stream was accepted.
func (c *StreamClient) markStreamClosed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streamSustainedLocked() {
		c.resetAuthFailureLocked()
	}
	c.streamOpen = false
	c.streamOpenedAt = time.Time{}
}

func (c *StreamClient) wasConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}
