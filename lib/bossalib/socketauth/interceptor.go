package socketauth

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"strings"

	"connectrpc.com/connect"
)

// AuthHeader is the request header carrying the Bearer token.
const AuthHeader = "Authorization"

const bearerPrefix = "Bearer "

// errUnauthenticated is intentionally generic: it never reveals whether a
// token file exists, its expected length, or its path. Operators get specifics
// from local daemon logs, not from the RPC error.
var errUnauthenticated = connect.NewError(
	connect.CodeUnauthenticated,
	errors.New("unauthenticated: daemon requires a valid socket token; restart the daemon or upgrade boss"),
)

// validHeader parses "Bearer <hex>" and constant-time-compares the raw decoded
// bytes against the expected token. Malformed scheme/hex/length are rejected
// before the compare. wantRaw is decoded once at interceptor construction.
func validHeader(header string, wantRaw []byte) bool {
	// Fail closed if no valid token was configured: an empty wantRaw would
	// otherwise accept an empty "Bearer " credential, because a zero-length
	// gotRaw matches the zero length and subtle.ConstantTimeCompare of two empty
	// slices returns 1. Unreachable via LoadOrCreateToken (always 32 bytes), but
	// this keeps the primitive fail-closed against future misuse.
	if len(wantRaw) == 0 {
		return false
	}
	if !strings.HasPrefix(header, bearerPrefix) {
		return false
	}
	got := strings.TrimPrefix(header, bearerPrefix)
	gotRaw, err := hex.DecodeString(got)
	if err != nil || len(gotRaw) != len(wantRaw) {
		return false
	}
	return subtle.ConstantTimeCompare(gotRaw, wantRaw) == 1
}

// NewServerInterceptor validates the Bearer token on every unary and streaming
// RPC. token is the canonical hex string from LoadOrCreateToken. An invalid or
// empty token yields a fail-closed interceptor (validHeader rejects everything
// when wantRaw is empty), never a fail-open one.
func NewServerInterceptor(token string) connect.Interceptor {
	wantRaw, err := hex.DecodeString(token) // token is validated upstream
	if err != nil {
		wantRaw = nil // fail closed rather than compare against a partial decode
	}
	return serverInterceptor{wantRaw: wantRaw}
}

type serverInterceptor struct{ wantRaw []byte }

func (s serverInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if !validHeader(req.Header().Get(AuthHeader), s.wantRaw) {
			return nil, errUnauthenticated
		}
		return next(ctx, req)
	}
}

func (s serverInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next // server interceptor: no client side
}

func (s serverInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		// Validate on stream open, before reading any request messages.
		if !validHeader(conn.RequestHeader().Get(AuthHeader), s.wantRaw) {
			return errUnauthenticated
		}
		return next(ctx, conn)
	}
}

// NewClientInterceptor attaches the Bearer token to outgoing unary and
// streaming requests.
func NewClientInterceptor(token string) connect.Interceptor {
	return clientInterceptor{header: bearerPrefix + token}
}

type clientInterceptor struct{ header string }

func (c clientInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set(AuthHeader, c.header)
		return next(ctx, req)
	}
}

func (c clientInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set(AuthHeader, c.header)
		return conn
	}
}

func (c clientInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next // client interceptor: no server side
}
