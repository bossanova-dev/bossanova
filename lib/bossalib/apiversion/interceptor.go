package apiversion

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
)

// HeaderName is the HTTP request/response header used to negotiate the API
// version between clients and the server.
const HeaderName = "Bossanova-Version"

type contextKey struct{}

// ResolvedVersion returns the API version stored in ctx by the server
// Interceptor. If no version is present (e.g. the call did not pass through
// the interceptor), it falls back to DefaultRegistry().Default().
func ResolvedVersion(ctx context.Context) Version {
	if v, ok := ctx.Value(contextKey{}).(Version); ok {
		return v
	}
	return DefaultRegistry().Default()
}

func withResolvedVersion(ctx context.Context, v Version) context.Context {
	return context.WithValue(ctx, contextKey{}, v)
}

// Interceptor returns a server connect.Interceptor that:
//   - reads the Bossanova-Version request header
//   - resolves it against reg (absent/empty → reg.Default())
//   - returns CodeInvalidArgument for unknown or malformed versions
//   - stores the resolved version in the request context
//   - on a successful unary response, applies changes (if non-nil) to
//     down-convert the response for older-version clients
//   - echoes the resolved version as a Bossanova-Version response header
//
// Pass nil for changes to skip transform application (negotiation-only mode).
//
// The interceptor mirrors the shape of lib/bossalib/errortrack.Interceptor.
func Interceptor(reg *Registry, changes *Changes) connect.Interceptor {
	return &versionInterceptor{reg: reg, changes: changes}
}

type versionInterceptor struct {
	reg     *Registry
	changes *Changes
}

// resolve validates raw and returns the resolved Version. Empty → Default.
func (vi *versionInterceptor) resolve(raw string) (Version, error) {
	if raw == "" {
		return vi.reg.Default(), nil
	}
	v, err := Parse(raw)
	if err != nil {
		all := vi.reg.All()
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported API version %q: must be YYYY-MM-DD; supported range: %s to %s",
				raw, all[0], all[len(all)-1]))
	}
	if !vi.reg.IsSupported(v) {
		all := vi.reg.All()
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported API version %q; supported range: %s to %s",
				raw, all[0], all[len(all)-1]))
	}
	return v, nil
}

func (vi *versionInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		v, err := vi.resolve(req.Header().Get(HeaderName))
		if err != nil {
			return nil, err
		}
		ctx = withResolvedVersion(ctx, v)
		resp, err := next(ctx, req)
		if err != nil {
			return nil, err
		}
		// Apply response transforms for older-version clients (newest→resolved order).
		// Per-message streaming transforms are intentionally out of scope here:
		// bidi/streaming protocol evolution is handled by additive proto fields
		// (buf-enforced wire compatibility), not by this layer. See the streaming-risk
		// note in docs/plans/BOS-10-version-the-orchestrator-apis.md.
		if vi.changes != nil {
			vi.changes.Apply(req.Spec().Procedure, resp.Any(), v)
		}
		resp.Header().Set(HeaderName, v.String())
		return resp, nil
	}
}

func (vi *versionInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (vi *versionInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		v, err := vi.resolve(conn.RequestHeader().Get(HeaderName))
		if err != nil {
			return err
		}
		ctx = withResolvedVersion(ctx, v)
		conn.ResponseHeader().Set(HeaderName, v.String())
		return next(ctx, conn)
	}
}

// ClientInterceptor returns a connect.Interceptor for use in first-party
// clients. It stamps every outgoing unary request and streaming client
// connection with the Bossanova-Version header set to built, so a deployed
// client always negotiates against the API version it was compiled with.
//
// Typical usage at client construction:
//
//	connect.WithInterceptors(apiversion.ClientInterceptor(apiversion.DefaultRegistry().Current()))
func ClientInterceptor(built Version) connect.Interceptor {
	return &clientVersionInterceptor{built: built}
}

type clientVersionInterceptor struct {
	built Version
}

func (ci *clientVersionInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set(HeaderName, ci.built.String())
		return next(ctx, req)
	}
}

func (ci *clientVersionInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		conn.RequestHeader().Set(HeaderName, ci.built.String())
		return conn
	}
}

func (ci *clientVersionInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
