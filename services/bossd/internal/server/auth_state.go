package server

import (
	"context"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
)

// GetAuthState reports the running daemon's live upstream auth state.
//
// This exists because a locally readable credential record says nothing about
// whether THIS daemon can talk to the orchestrator. Throughout the BOS-942
// incident the `workos-tokens-v1` record was present and parseable, so every
// local check the CLI could make came back clean while the daemon's
// re-registration failed every 30 seconds. Only the daemon itself is an
// authority on that, which is why the answer has to come over the wire.
//
// The response carries scalar, enumerated, non-secret facts only — never an
// access token, a refresh token, an Authorization header value, or an upstream
// response body — because `boss daemon doctor` renders these fields verbatim
// to an operator's terminal, and terminals get pasted into issues.
func (s *Server) GetAuthState(ctx context.Context, _ *connect.Request[bossanovav1.GetAuthStateRequest]) (*connect.Response[bossanovav1.GetAuthStateResponse], error) {
	// A nil reporter is local-only mode: the daemon was started with no
	// upstream at all. That is a real, correct answer to "what is your auth
	// state", not a failure to produce one, so report it as data and return no
	// error. Returning an error here would make an ordinary local-only daemon
	// look broken in the doctor, and a doctor that cries wolf on a healthy
	// configuration stops being read.
	if s.authStateReporter == nil {
		return connect.NewResponse(&bossanovav1.GetAuthStateResponse{
			UpstreamConfigured: false,
		}), nil
	}

	state := s.authStateReporter.AuthState(ctx)
	resp := &bossanovav1.GetAuthStateResponse{
		UpstreamConfigured: true,
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
	return connect.NewResponse(resp), nil
}
