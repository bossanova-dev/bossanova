package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/rs/zerolog"

	server "github.com/recurser/bossd/api/daemon"
	"github.com/recurser/bossd/api/upstream"
	"github.com/recurser/bossd/internal/agent"
	"github.com/recurser/bossd/internal/db"
	"github.com/recurser/bossd/internal/notesync"
	"github.com/recurser/bossd/internal/session"
)

// sessionGetterAdapter wires db.SessionStore.Get into the
// upstream.SessionReader interface used by the command handler adapter.
type sessionGetterAdapter struct {
	sessions db.SessionStore
}

func (a sessionGetterAdapter) GetSession(ctx context.Context, id string) (*bossanovav1.Session, error) {
	sess, err := a.sessions.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return server.SessionToProto(sess), nil
}

// automationToggleAdapter exposes db.SessionStore.Update's
// IsAutomationEnabled field as a narrow interface so the pause/resume
// command path doesn't need the full update surface.
type automationToggleAdapter struct {
	sessions db.SessionStore
}

func (a automationToggleAdapter) SetIsAutomationEnabled(ctx context.Context, sessionID string, enabled bool) error {
	_, err := a.sessions.Update(ctx, sessionID, db.UpdateSessionParams{IsAutomationEnabled: &enabled})
	return err
}

// newDispatcherLookup builds the lookup closure used by agent.Dispatcher
// to resolve an ID to its AgentName. It accepts EITHER a bossd session ID
// (lifecycle paths) OR an agent session ID — the liveness checker and the
// interactive attach adapter both pass the latter, since they only ever
// know the agent's tracking key. The chats table indexes agent session IDs,
// so we fall through to it when sessions.Get misses. Returning an error on
// double-miss lets the dispatcher's existing fallback (defaultAgent /
// single-loaded-runner shortcut) kick in for genuinely unknown IDs.
func newDispatcherLookup(sessions db.SessionStore, chats db.AgentChatStore) func(string) (string, error) {
	return func(id string) (string, error) {
		if sess, err := sessions.Get(context.Background(), id); err == nil {
			return sess.AgentName, nil
		}
		if chats != nil {
			if chat, err := chats.GetByAgentSessionID(context.Background(), id); err == nil {
				return chat.AgentName, nil
			}
		}
		return "", fmt.Errorf("no session or chat found for id %q", id)
	}
}

// attachLookupAdapter resolves a session ID to its current claude
// session ID and state — the two bits the attacher needs to decide
// whether to tail or bounce straight to SessionEnded.
type attachLookupAdapter struct {
	sessions db.SessionStore
}

func (a attachLookupAdapter) LookupAttachTarget(ctx context.Context, sessionID string) (string, int32, error) {
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil {
		return "", 0, err
	}
	agentSessionID := ""
	if sess.AgentSessionID != nil {
		agentSessionID = *sess.AgentSessionID
	}
	return agentSessionID, clampInt32(int(sess.State)), nil
}

// claudeAttachAdapter converts claude.Runner's OutputLine channel into
// the upstream-package AttachOutputLine shape so the attacher's
// interface stays free of the claude package.
type claudeAttachAdapter struct {
	runner agent.Runner
}

func (a claudeAttachAdapter) IsRunning(claudeSessionID string) bool {
	return a.runner.IsRunning(claudeSessionID)
}

func (a claudeAttachAdapter) History(claudeSessionID string) []upstream.AttachOutputLine {
	lines := a.runner.History(claudeSessionID)
	out := make([]upstream.AttachOutputLine, len(lines))
	for i, l := range lines {
		out[i] = upstream.AttachOutputLine{Text: l.Text, Timestamp: l.Timestamp}
	}
	return out
}

func (a claudeAttachAdapter) Subscribe(ctx context.Context, claudeSessionID string) (<-chan upstream.AttachOutputLine, error) {
	ch, err := a.runner.Subscribe(ctx, claudeSessionID)
	if err != nil {
		return nil, err
	}
	out := make(chan upstream.AttachOutputLine, 64)
	go func() {
		defer close(out)
		for line := range ch {
			select {
			case out <- upstream.AttachOutputLine{Text: line.Text, Timestamp: line.Timestamp}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

func runSnapshotPublisher(
	ctx context.Context,
	client bossanovav1connect.OrchestratorServiceClient,
	sessionToken *upstream.SessionTokenHolder,
	stores upstream.StreamStores,
	daemonID, hostname string,
	reRegister func(context.Context) (string, error),
	closeIdle func(),
	interval time.Duration,
	logger zerolog.Logger,
) {
	// attempt sends one snapshot with the given bearer token and returns the
	// raw PublishDaemonSnapshot error (nil on success).
	attempt := func(pubCtx context.Context, snap *bossanovav1.DaemonSnapshot, token string) error {
		req := connect.NewRequest(&bossanovav1.PublishDaemonSnapshotRequest{Snapshot: snap})
		req.Header().Set("Authorization", "Bearer "+token)
		_, err := client.PublishDaemonSnapshot(pubCtx, req)
		return err
	}

	publish := func() {
		if closeIdle != nil {
			defer closeIdle()
		}
		token := ""
		if sessionToken != nil {
			token = sessionToken.Get()
		}
		if token == "" {
			logger.Debug().Msg("snapshot publisher waiting for daemon session token")
			return
		}
		pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		snap, err := buildSnapshotForPublish(pubCtx, stores, daemonID, hostname)
		if err != nil {
			logger.Warn().Err(err).Msg("snapshot publisher: build snapshot")
			return
		}
		err = attempt(pubCtx, snap, token)
		// Self-heal a stale session_token. CodeUnauthenticated ("invalid
		// credentials") means bosso's daemons row for our token is gone —
		// bosso restarted, or another bossd with our daemon_id rotated it via
		// UPSERT. The bidi stream normally re-registers on its own auth
		// rejection, but if that loop is wedged (e.g. blocked in a half-open
		// Receive) the publisher is the only feed still running. Without a
		// re-register here it would fail every tick forever and the daemon
		// would stay invisible on the web. Rotate the shared holder (which
		// fans out to both stream openers) and retry once.
		if err != nil && connect.CodeOf(err) == connect.CodeUnauthenticated && reRegister != nil {
			newTok, regErr := reRegister(pubCtx)
			switch {
			case regErr != nil:
				logger.Warn().Err(regErr).Msg("snapshot publisher: re-register after auth rejection failed")
			case newTok == "":
				logger.Warn().Msg("snapshot publisher: re-register returned empty session token")
			default:
				retryToken := newTok
				if sessionToken != nil {
					var already bool
					retryToken, already = upstream.AdoptReRegisteredToken(sessionToken, token, newTok)
					if already {
						logger.Info().Msg("snapshot publisher: session_token already rotated after auth rejection")
					} else {
						logger.Info().Msg("snapshot publisher: rotated session_token after auth rejection")
					}
				} else {
					logger.Info().Msg("snapshot publisher: using re-registered session_token after auth rejection")
				}
				err = attempt(pubCtx, snap, retryToken)
			}
		}
		if err != nil {
			// CodeUnimplemented means the orchestrator has no read model
			// (single-instance / local dev) — there is nothing to reconcile,
			// so don't spam warnings on every steady-state tick.
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				logger.Debug().Msg("snapshot publisher: read model not configured")
			} else {
				logger.Warn().Err(err).Msg("snapshot publisher: publish failed")
			}
			return
		}
		logger.Debug().Int("sessions", len(snap.GetSessions())).Msg("snapshot publisher: published")
	}

	publish()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			publish()
		}
	}
}

// noteSyncNudger converts the optional worker to the server's nudge
// interface, keeping a nil worker a nil INTERFACE: a typed nil pointer would
// read as configured at the server's nil check.
func noteSyncNudger(w *notesync.Worker) server.NoteSyncNudger {
	if w == nil {
		return nil
	}
	return w
}

func buildSnapshotForPublish(ctx context.Context, stores upstream.StreamStores, daemonID, hostname string) (*bossanovav1.DaemonSnapshot, error) {
	snap := &bossanovav1.DaemonSnapshot{
		DaemonId: daemonID,
		Hostname: hostname,
	}
	if stores.Repos != nil {
		repos, err := stores.Repos.SnapshotRepoIDs(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot repos: %w", err)
		}
		snap.RepoIds = repos
	}
	if stores.Sessions != nil {
		sessions, err := stores.Sessions.SnapshotSessions(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot sessions: %w", err)
		}
		snap.Sessions = sessions
	}
	if stores.Chats != nil {
		chats, err := stores.Chats.SnapshotChats(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot chats: %w", err)
		}
		snap.Chats = chats
	}
	if stores.Statuses != nil {
		statuses, err := stores.Statuses.SnapshotStatuses(ctx)
		if err != nil {
			return nil, fmt.Errorf("snapshot statuses: %w", err)
		}
		snap.Statuses = statuses
	}
	return snap, nil
}

// streamReconnector is the subset of *upstream.StreamClient the auth
// adapter needs on login: wake the Run loop so it re-opens the stream
// immediately (with the freshly-registered session token). Narrowed to an
// interface so the login path is unit-testable without a live stream.
type streamReconnector interface {
	Reconnect()
}

// streamAuthSnapshotter is the read-only subset of *upstream.StreamClient the
// auth adapter needs to answer the GetAuthState diagnostic.
//
// Kept separate from streamReconnector rather than widening it: Reconnect is a
// command and AuthSnapshot is a query, and the existing login tests' fake
// implements only the former. Segregating them means a diagnostic addition
// does not force every command-side fake to grow a method it has no opinion
// about.
type streamAuthSnapshotter interface {
	AuthSnapshot() upstream.AuthSnapshot
}

// streamAuthAdapter implements server.AuthNotifier by reloading
// credentials from the keychain on login and signalling active streams on
// logout. The shared AuthState is wired into both DaemonStream and
// TerminalStream, so MarkNeedsLogin cancels any open bidi immediately and
// the outer Run loops pause until NotifyLogin marks auth OK again.
// authTokenReloader is the slice of *upstream.KeychainTokenProvider that
// NotifyLogin needs. It is an interface so a test can drive the post-reload
// log line — including the read-failure case, which is the one the
// instrumentation exists for and which a real keychain will not stage on
// demand.
type authTokenReloader interface {
	ReloadResult() upstream.ReloadOutcome
	Token() string
	ExpiresAt() time.Time
	ReloginReason() string
	// CredentialVerdict answers "are the reloaded credentials usable?" under a
	// single lock, so the gate decision below cannot be made from a token and
	// a marker that were never true at the same instant.
	CredentialVerdict() (usable bool, reloginReason string)
}

type streamAuthAdapter struct {
	streamClient  streamReconnector
	tokenProvider authTokenReloader
	authState     *upstream.AuthState
	logger        zerolog.Logger
	// streamAuth and sessionTokens back the read-only AuthState() reporter.
	// Both are nil-tolerant: a local-only daemon never builds this adapter at
	// all, and the login unit tests construct it without them.
	//
	// Know what that tolerance costs before you rely on it. A nil source
	// leaves its DaemonAuthState fields at their Go zero values, which is
	// byte-identical to a healthy reading — Connected=false,
	// AuthFailingSince=zero — while GetAuthState still answers
	// UpstreamConfigured=true. So an adapter built upstream-side WITHOUT
	// streamAuth reports "signed in" for a wedged daemon, the exact reading
	// this whole feature exists to make impossible. The single production
	// construction site (search streamAuthAdapter{ in this file) wires all
	// four sources together; keep it that way rather than adding a caller that
	// omits one.
	streamAuth    streamAuthSnapshotter
	sessionTokens *upstream.SessionTokenHolder
	// reRegister proactively (re-)registers the daemon with the
	// orchestrator on login. It shares the reRegisterMu + sessionTokenHolder
	// with the reactive tryReRegister path, so login and auth-failure
	// recovery never double-register. Nil in local-only mode.
	reRegister func(context.Context) (string, error)
}

// NotifyLogin reloads keychain credentials so the running stream picks
// up the freshly stored tokens from `boss login`. Calling Refresh here
// would use the in-memory refresh_token, which has been superseded by
// the new login — WorkOS rejects it with "Session has already ended".
// Reading the keychain instead picks up the access+refresh pair the CLI
// just wrote. The next reconnect (or the periodic refresher when the
// current JWT nears expiry) propagates the new token to bosso.
//
// It then proactively (re-)registers with the orchestrator, BEFORE clearing
// the auth gate. Because you cannot log in before the daemon exists, every
// first login lands on a running-daemon-with-no-registration state; without
// this the daemon stays unregistered until a manual restart (the reactive
// tryReRegister path can wedge on a never-populated session token). A single
// re-register fires per login: on success we wake the stream so it re-opens
// with the fresh token immediately. Ordering matters — a daemon parked on
// NeedsLogin is woken by MarkOK() itself (not only by Reconnect()), so if
// MarkOK ran first the Run loop could race into openStream and hit
// CodeUnauthenticated with the still-stale token before this register
// populated sessionTokenHolder, forcing a redundant reactive re-register.
// Registering first means both park paths (the NeedsLogin Wait() and the
// backoff select) re-open cleanly and the reactive path never sees
// CodeUnauthenticated — the primary anti-storm guarantee; the shared
// reRegisterMu + sessionTokenHolder keep login and the reactive path from
// double-registering. Re-register is best-effort — a failure is logged and
// MarkOK still runs so the reactive path remains the backstop. Note that this
// function DOES return that failure, as a RegisterFailed verdict plus the
// error; "login never fails on it" is now Server.NotifyAuthChange's doing,
// which deliberately reports the verdict without an RPC error. A second caller
// of NotifyLogin must make that same choice for itself.
//
// MarkOK is called last, and is no longer unconditional: it runs only when
// the reloaded record was judged usable, or when the reload could not read the
// record at all (unreadable is not evidence of a bad credential, and refusing
// the gate there would strand the daemon). It clears the "needs re-login" flag
// set by the opener when the previous refresh token could not be exchanged — WorkOS
// rejected it, or the exchange outcome was never confirmed. The Run
// loops are blocked on AuthState.Wait() in that case; clearing the flag
// wakes them so they reconnect with the freshly-loaded keychain credentials
// and the now-populated session token. While the re-register runs (bounded
// below), a NeedsLogin-parked loop simply stays parked — there is nothing to
// dial without a token yet.
func (a *streamAuthAdapter) NotifyLogin(ctx context.Context, _ []string) (server.LoginVerdict, error) {
	// reloadUnverified records that the reload could not read the record at
	// all, so the cache below describes the PRE-login state and cannot be used
	// to judge the credentials this login just wrote. See the gate below.
	reloadUnverified := false
	if a.tokenProvider != nil {
		// Log what the RELOAD observed, not merely what the cache holds
		// afterwards: a read failure leaves the previous cache in place, so
		// without reload_read_ok an empty token here would mean either "the
		// record really is still flagged" or "the reload could not read the
		// record at all". The backend is here because a boss CLI writing to a
		// different keyring backend than this daemon reads from produces a
		// perfectly successful read of the wrong store.
		outcome := a.tokenProvider.ReloadResult()
		// ONE verdict snapshot, taken here and reused by the gate below.
		// Token(), ReloginReason() and CredentialVerdict() each take their
		// own RLock, so deriving the log line and the gate from separate
		// calls lets a concurrent Refresh/applyTokens land between them and
		// print a line saying the credentials look fine directly above a
		// gate that closed on them — the log would misdescribe the very
		// decision it exists to explain. credentials_usable and
		// relogin_reason are therefore the gate's OWN values, so the two can
		// no longer disagree about the outcome. token_present and expires_at
		// stay as separate best-effort reads: BOS-942 wants them for
		// diagnosis, neither one feeds the gate, and a skew in either can at
		// worst age the detail beside a verdict that is still correct.
		credsUsable, reloginReason := a.tokenProvider.CredentialVerdict()
		a.logger.Info().
			Str("component", "auth-reload").
			Bool("reload_read_ok", outcome.ReadOK).
			Str("reload_error_class", outcome.ErrorClass).
			Str("keyring_backend", upstream.KeyringBackend()).
			Bool("credentials_usable", credsUsable).
			Str("relogin_reason", reloginReason).
			Bool("token_present", a.tokenProvider.Token() != "").
			Time("expires_at", a.tokenProvider.ExpiresAt()).
			Msg("reloaded credentials after login")

		// The gate below is the whole point of this function: MarkOK used to
		// fire unconditionally, which turned a wedged daemon into a wedged
		// daemon that LOOKED recovered — the Run loops woke, openStream found
		// no JWT, bosso rejected the handshake, and the daemon fell straight
		// back to MarkNeedsLogin with the only evidence in its own log.
		// ...but only where the reload actually observed the record. A
		// ReloadErrorReadFailed leaves the previous cache in place by design,
		// so a verdict drawn from it describes the credentials the daemon held
		// BEFORE this login — exactly the ones the user just replaced. Gating
		// on that would answer a transient keyring read error with a confident
		// "your credentials are still flagged", withhold MarkOK, and park both
		// Run loops with no way back: the reactive Reload() hatch lives inside
		// openStream, downstream of the NeedsLogin gate, so a parked loop can
		// never reach it. Unreadable is not the same as unusable, so this path
		// keeps the pre-BOS-945 behaviour (register, then MarkOK, letting the
		// reload hatch self-heal) and reports no verdict rather than a wrong
		// one. ReloadErrorRecordDeleted is authoritative and clears the cache,
		// so it is judged normally.
		reloadUnverified = !outcome.ReadOK && outcome.ErrorClass == upstream.ReloadErrorReadFailed
		if reloadUnverified {
			a.logger.Warn().
				Str("component", "auth-reload").
				Str("reload_error_class", outcome.ErrorClass).
				Msg("could not re-read credentials after login; clearing the auth gate unjudged so the stream can retry")
		}
		if !reloadUnverified && !credsUsable {
			// Nothing to dial with, so the proactive re-register is skipped
			// too: with no bearer token it sends no Authorization header and
			// bosso answers "missing or invalid Authorization header",
			// manufacturing a misleading log line for a call that could not
			// have worked. The auth gate stays closed, which is honest — the
			// Run loops have nothing to reconnect with either way.
			//
			// The reason is one of the enumerated persisted markers, never
			// token material, so it is safe to log and to return.
			verdict := server.LoginVerdict{
				Outcome:       server.LoginOutcomeCredentialsMissing,
				ReloginReason: reloginReason,
			}
			if reloginReason != "" {
				verdict.Outcome = server.LoginOutcomeCredentialsFlagged
			}
			a.logger.Warn().
				Str("component", "auth-reload").
				Str("relogin_reason", reloginReason).
				Msg("credentials still unusable after login; auth gate left closed")
			return verdict, fmt.Errorf("credentials still unusable after login (relogin_reason=%q)", reloginReason)
		}
	}
	var registerErr error
	if a.reRegister != nil {
		regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if _, err := a.reRegister(regCtx); err != nil {
			a.logger.Warn().Err(err).Msg("login re-register failed; reactive path will retry")
			registerErr = err
		} else if a.streamClient != nil {
			a.streamClient.Reconnect()
		}
	}
	// MarkOK still fires when only the re-register failed. Re-register is
	// documented best-effort and the reactive path is the backstop; withholding
	// the gate on a CLEAN record would park both Run loops behind
	// AuthState.Wait() with usable credentials on disk and nothing left to wake
	// them. Only the reporting changes: the failure now rides back in the
	// verdict instead of dying in the log.
	if a.authState != nil && a.authState.MarkOK() {
		// Gated on the transition so this line means "the daemon was actually
		// parked and is now released", not "a login happened" — the latter is
		// the steady state and would drown the signal.
		a.logger.Info().
			Str("component", "auth-reload").
			Msg("credentials usable after login; auth gate cleared")
	}
	if registerErr != nil {
		return server.LoginVerdict{Outcome: server.LoginOutcomeRegisterFailed}, registerErr
	}
	if reloadUnverified {
		// The gate was cleared, but nothing here verified the credentials, and
		// OUTCOME_OK is a claim. Unspecified is the value built for "no verdict
		// given", and it renders as silence rather than as reassurance.
		return server.LoginVerdict{Outcome: server.LoginOutcomeUnspecified}, nil
	}
	return server.LoginVerdict{Outcome: server.LoginOutcomeOK}, nil
}

// AuthState reports what this daemon currently knows about its own upstream
// auth. It implements server.AuthStateReporter, the read-only counterpart to
// the AuthNotifier commands above.
//
// Every field is read from a live, concurrently-mutated source rather than
// recomputed from disk — that is the entire point. Re-reading the keychain
// here would reproduce the BOS-942 blind spot exactly: the record was present
// and parseable the whole time the daemon could not register.
//
// Each source is independently nil-tolerant so a partially wired adapter
// degrades to "nothing known about that field" instead of panicking inside a
// diagnostic, which is the worst possible place to crash.
func (a *streamAuthAdapter) AuthState(_ context.Context) server.DaemonAuthState {
	var state server.DaemonAuthState
	// Nil-receiver tolerant, deliberately. A nil *streamAuthAdapter assigned
	// into the server's AuthStateReporter interface field reads as non-nil
	// there, so the handler's local-only guard would let the call through;
	// answering with a zero state degrades that to "nothing known" instead of
	// crashing the daemon inside the RPC an operator reaches for when things
	// are already going wrong. The assignment site guards it too — both layers
	// are wanted, neither is redundant.
	if a == nil {
		return state
	}
	if a.authState != nil {
		state.NeedsLogin = a.authState.NeedsLogin()
	}
	if a.tokenProvider != nil {
		state.ReloginReason = a.tokenProvider.ReloginReason()
	}
	if a.sessionTokens != nil {
		state.LastRegisteredAt = a.sessionTokens.LastSetAt()
	}
	if a.streamAuth != nil {
		snap := a.streamAuth.AuthSnapshot()
		state.Connected = snap.Connected
		state.AuthFailingSince = snap.AuthFailingSince
	}
	return state
}

// NotifyLogout marks the auth state as "needs login". Active streams watch
// this transition and cancel their contexts immediately, then the Run loops
// pause before reconnecting. Idempotent.
func (a *streamAuthAdapter) NotifyLogout() {
	if a.authState != nil {
		a.authState.MarkNeedsLogin()
	}
}

// decodeJWTClaimsForLog extracts iss/sub/aud/exp from an unverified JWT
// for diagnostic logging. It deliberately does not validate the signature
// — it's just pulling fields out of the base64url-encoded payload so the
// log line tells us whether the token is expired, for the wrong client,
// or malformed. Returns empty strings + err on any parse failure.
func decodeJWTClaimsForLog(token string) (iss, sub, aud, exp string, err error) {
	if token == "" {
		return "", "", "", "", fmt.Errorf("empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", "", "", fmt.Errorf("not a JWT (%d parts)", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", "", "", fmt.Errorf("base64 decode payload: %w", err)
	}
	var claims struct {
		Iss string          `json:"iss"`
		Sub string          `json:"sub"`
		Aud json.RawMessage `json:"aud"`
		Exp int64           `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", "", "", "", fmt.Errorf("unmarshal claims: %w", err)
	}
	expStr := ""
	if claims.Exp > 0 {
		t := time.Unix(claims.Exp, 0)
		expStr = fmt.Sprintf("%s (in %s)", t.Format(time.RFC3339), time.Until(t).Round(time.Second))
	}
	return claims.Iss, claims.Sub, string(claims.Aud), expStr, nil
}

func mergeSessionEvents(ctx context.Context, a, b <-chan session.Event) <-chan session.Event {
	out := make(chan session.Event, 64)
	go func() {
		defer close(out)
		for a != nil || b != nil {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-a:
				if !ok {
					a = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			case ev, ok := <-b:
				if !ok {
					b = nil
					continue
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
