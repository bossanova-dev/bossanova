package server

import (
	"context"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/safego"
	"github.com/recurser/bossd/internal/ghtoken"
)

// AuthStateReporter is the read-only counterpart to AuthNotifier: it reports
// what the RUNNING daemon currently knows about its own upstream auth, which
// the GetAuthState RPC renders for `boss daemon doctor`.
//
// It is deliberately a second, separate interface rather than two more methods
// on AuthNotifier. AuthNotifier is a command surface — a caller that holds it
// can change the daemon's auth behaviour — and a diagnostic has no business
// requiring that authority. Keeping them apart also means a build with no
// reporter wired still compiles against every existing AuthNotifier
// implementation.
//
// Like AuthNotifier it is nil in local-only mode, where there is no upstream to
// report on. Implementations must be safe for concurrent use: the RPC handler
// calls this from a request goroutine while the stream loop mutates the same
// state.
type AuthStateReporter interface {
	// AuthState returns a snapshot of the daemon's live upstream auth state.
	// It must never return token material: the caller renders these fields
	// verbatim to an operator's terminal.
	AuthState(ctx context.Context) DaemonAuthState
}

// DaemonAuthState is the scalar, non-secret snapshot AuthStateReporter returns.
// It mirrors the wire message field for field, but is declared here so the
// upstream package's adapter does not have to import the generated protobuf
// types just to answer a diagnostic.
type DaemonAuthState struct {
	// UpstreamOrigin identifies this daemon's Cloud environment without secrets.
	UpstreamOrigin string
	// NeedsLogin is the daemon's own "I have parked until someone runs
	// `boss login`" flag. It is NOT a fault signal on its own — it is the
	// correct, expected state right after a deliberate `boss logout`.
	NeedsLogin bool
	// ReloginReason is the enumerated, non-secret marker persisted alongside
	// the credentials ("refresh_outcome_unknown", "refresh_token_rejected"),
	// or "" when none. Treat an unrecognised value as forward-compatible:
	// render it rather than mapping it to "unknown".
	ReloginReason string
	// LastRegisteredAt is when this daemon process last completed a successful
	// upstream registration. Zero when it never has. It is context, not a
	// liveness floor: a long-lived healthy daemon legitimately registers once
	// and never again.
	LastRegisteredAt time.Time
	// Connected is the coarse "the last handshake reached snapshot-accepted"
	// signal. False during any ordinary reconnect gap, so it is never a
	// verdict by itself.
	Connected bool
	// AuthFailingSince is the discriminating BOS-942 fact: the start of the
	// current unbroken run of unrecoverable auth failures, zero when auth is
	// not currently failing. A daemon with NeedsLogin false and a non-zero
	// AuthFailingSince is wedged, and nothing else in the system says so.
	AuthFailingSince time.Time
}

// GitHubCredentialReporter answers the per-repo GitHub credential part of
// GetAuthState. *ghtoken.Source satisfies it. Report must make no network
// call and return no token material.
//
// The door imports ghtoken for the value type rather than mirroring it, as
// DaemonAuthState does for upstream: ghtoken is a plain internal package with
// no protobuf types, so a mirror would be a second copy of the same fields for
// no boundary gain. Nil leaves the response's credential report unset.
type GitHubCredentialReporter interface {
	Report(ctx context.Context) ([]ghtoken.RepoCredentialState, error)
}

// GetAuthState reports the running daemon's live upstream auth state.
//
// This exists because a locally readable credential record says nothing about
// whether THIS daemon can talk to the orchestrator. Throughout the BOS-942
// incident the `workos-tokens-v1` record was present and parseable, so every
// local check the CLI could make came back clean while the daemon's
// re-registration failed every 30 seconds. Only the daemon itself is an
// authority on that, which is why the answer has to come over the wire.
//
// The response carries enumerated, non-secret facts only — the upstream auth
// state and, per repo, which GitHub credential it uses — never an access
// token, a refresh token, an installation token, an Authorization header
// value, or an upstream response body, because `boss daemon doctor` renders
// these fields verbatim to an operator's terminal, and terminals get pasted
// into issues. The credential report never fails the RPC: the auth half is the
// BOS-942 wedge detector and must still answer when the repo listing fails.
func (s *Server) GetAuthState(ctx context.Context, _ *connect.Request[bossanovav1.GetAuthStateRequest]) (*connect.Response[bossanovav1.GetAuthStateResponse], error) {
	// A nil reporter is local-only mode: the daemon was started with no
	// upstream at all. That is a real, correct answer to "what is your auth
	// state", not a failure to produce one, so report it as data and return no
	// error. Returning an error here would make an ordinary local-only daemon
	// look broken in the doctor, and a doctor that cries wolf on a healthy
	// configuration stops being read.
	resp := &bossanovav1.GetAuthStateResponse{UpstreamConfigured: false}
	if s.authStateReporter != nil {
		resp = s.upstreamAuthState(ctx)
	}
	resp.GithubCredentialReport = s.githubCredentialReport(ctx)
	return connect.NewResponse(resp), nil
}

// upstreamAuthState maps the signed-in daemon's auth state onto the wire.
func (s *Server) upstreamAuthState(ctx context.Context) *bossanovav1.GetAuthStateResponse {
	state := s.authStateReporter.AuthState(ctx)
	resp := &bossanovav1.GetAuthStateResponse{
		UpstreamConfigured: true,
		UpstreamOrigin:     state.UpstreamOrigin,
		NeedsLogin:         state.NeedsLogin,
		ReloginReason:      state.ReloginReason,
		UpstreamConnected:  state.Connected,
	}
	// Distinguish "never happened" from "happened at the zero instant" by
	// leaving the field unset rather than encoding a zero timestamp. The CLI
	// renders an unset last_registered_at as `never`, which is a different
	// sentence from a date in 1970.
	if !state.LastRegisteredAt.IsZero() {
		resp.LastRegisteredAt = timestamppb.New(state.LastRegisteredAt)
	}
	if !state.AuthFailingSince.IsZero() {
		resp.AuthFailingSince = timestamppb.New(state.AuthFailingSince)
	}
	return resp
}

// githubCredentialReport maps the credential source's per-repo report onto the
// wire. Nil (field unset) means "not reported": no source is wired, or Report
// failed. A failure logs one Warn carrying only the error text, which holds no
// token because Report holds none. An empty report is non-nil with no repos.
func (s *Server) githubCredentialReport(ctx context.Context) *bossanovav1.GitHubCredentialReport {
	if s.githubCredentials == nil {
		return nil
	}
	states, ok := s.boundedGitHubCredentialReport(ctx)
	if !ok {
		return nil
	}
	report := &bossanovav1.GitHubCredentialReport{
		Repos: make([]*bossanovav1.GitHubCredentialState, 0, len(states)),
	}
	for _, st := range states {
		report.Repos = append(report.Repos, &bossanovav1.GitHubCredentialState{
			RepoId:         st.RepoID,
			RepoOriginUrl:  st.Origin,
			Identity:       githubCredentialIdentityToProto(st.Identity),
			InstallationId: st.InstallationID,
			AccountLogin:   st.AccountLogin,
			Reason:         string(st.Reason),
			ExpiresAt:      timestampOrNil(st.ExpiresAt),
			NextAttemptAt:  timestampOrNil(st.NextAttemptAt),
		})
	}
	return report
}

// githubCredentialReportTimeout bounds the credential report well inside the
// doctor's 5s GetAuthState probe. Report lists repos, and that listing reads
// SQLite and hydrates keyring secrets, either of which can stall; the auth half
// of the response must still reach the doctor when it does.
const githubCredentialReportTimeout = 2 * time.Second

// boundedGitHubCredentialReport runs Report under githubCredentialReportTimeout
// and stops waiting at that bound even if Report ignores its context. ok is
// false (and one Warn is logged) on an error, a timeout or a recovered panic.
func (s *Server) boundedGitHubCredentialReport(ctx context.Context) ([]ghtoken.RepoCredentialState, bool) {
	ctx, cancel := context.WithTimeout(ctx, githubCredentialReportTimeout)
	defer cancel()
	type result struct {
		states []ghtoken.RepoCredentialState
		err    error
	}
	// Buffered so a Report that finishes after the timeout never blocks.
	results := make(chan result, 1)
	done := safego.Go(s.logger, func() {
		states, err := s.githubCredentials.Report(ctx)
		results <- result{states: states, err: err}
	})
	select {
	case <-done:
	case <-ctx.Done():
		s.logger.Warn().Err(ctx.Err()).Msg("github credential report did not finish in time; GetAuthState leaves it unset")
		return nil, false
	}
	select {
	case r := <-results:
		if r.err != nil {
			s.logger.Warn().Err(r.err).Msg("github credential report failed; GetAuthState leaves it unset")
			return nil, false
		}
		return r.states, true
	default:
		// Report panicked; safego logged it.
		return nil, false
	}
}

// githubCredentialIdentityToProto maps an identity to its wire enum; anything
// unrecognised is UNSPECIFIED, never guessed.
func githubCredentialIdentityToProto(id ghtoken.Identity) bossanovav1.GitHubCredentialIdentity {
	switch id {
	case ghtoken.IdentityInstallation:
		return bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_INSTALLATION
	case ghtoken.IdentityUser:
		return bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_USER
	default:
		return bossanovav1.GitHubCredentialIdentity_GIT_HUB_CREDENTIAL_IDENTITY_UNSPECIFIED
	}
}

// timestampOrNil leaves a zero instant unset rather than encoding 1970, the
// same rule GetAuthState applies to last_registered_at.
func timestampOrNil(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}
