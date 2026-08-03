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
}

// NewSessionTokenHolder constructs a holder seeded with the given token.
// Callers typically pass the SessionToken returned by upstream.Register.
func NewSessionTokenHolder(tok string) *SessionTokenHolder {
	return &SessionTokenHolder{tok: tok}
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
		// — the typical reconnect path. The 60s window matches the
		// refreshThreshold lower bound and gives bosso a comfortable
		// validity window to complete the register handshake.
		if exp := o.tokens.ExpiresAt(); !exp.IsZero() && time.Until(exp) < 60*time.Second {
			if _, err := o.tokens.Refresh(ctx); err != nil {
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
		refreshInterval = 60 * time.Second
	}
	refreshThreshold := cfg.RefreshThreshold
	if refreshThreshold == 0 {
		refreshThreshold = 60 * time.Second
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
	// authStuck tracks "we're in a sustained CodeUnauthenticated loop where
	// ReRegister also can't recover" so the next retry's log line drops to
	// debug. Cleared as soon as the error class changes or a re-register
	// succeeds.
	authStuck := false
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
			// Reset backoff and authStuck so the post-login dial doesn't
			// inherit a long backoff from the paused era.
			backoff = streamInitialBackoff
			authStuck = false
			continue
		}

		c.markDisconnected()
		err := c.openStream(ctx)
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
			authStuck = false
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
			if authFailed {
				rotated = c.tryReRegister(ctx, authStuck)
			}
			// Reduce log spam on a sustained auth loop: log the first
			// failure (and any change in error code or rotation outcome)
			// at warn, then drop to debug while the same condition
			// repeats. The state-change branch surfaces real progress
			// (e.g. JWT became valid mid-loop) without burying it.
			//
			// Without this: every retry comes back "missing or invalid
			// Authorization header" at warn and fills the log, even
			// though there's nothing actionable beyond the first one.
			suppressLog := authStuck && authFailed && !rotated
			if suppressLog {
				c.logger.Debug().Err(err).Dur("backoff", backoff).Msg("stream closed, reconnecting (auth still failing)")
			} else {
				c.logger.Warn().Err(err).Dur("backoff", backoff).Msg("stream closed, reconnecting")
			}
			authStuck = authFailed && !rotated
			// Reset backoff only when we actually progressed:
			//   - non-auth error after a real handshake (one-off flake)
			//   - auth error but ReRegister got us a fresh token (real
			//     recovery; retry now while the token is hot)
			// A bare CodeUnauthenticated where ReRegister also failed
			// means we're stuck on bad credentials. wasConnected() is
			// misleading there because Send(snapshot) succeeds locally
			// before bosso's header-only auth rejection arrives, so
			// every attempt looks "connected" and would reset the
			// backoff to 1s — that's what was filling the log.
			switch {
			case rotated:
				backoff = streamInitialBackoff
			case !authFailed && c.wasConnected():
				backoff = streamInitialBackoff
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

		// Exponential ramp, capped at streamMaxBackoff. Only grow when
		// the last attempt was a dead-on-arrival failure (no handshake).
		if !c.wasConnected() {
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
	// uses this to decide whether to reset backoff on the next error.
	c.markConnected()

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
func (c *StreamClient) markConnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = true
}

func (c *StreamClient) markDisconnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connected = false
}

func (c *StreamClient) wasConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}
