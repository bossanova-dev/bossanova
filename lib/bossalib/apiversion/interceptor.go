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
//   - resolves parseable versions at/above the registry min to the newest
//     supported version not newer than the client version
//   - returns CodeInvalidArgument for malformed or older-than-min versions
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

// resolve validates raw and returns the resolved Version. Empty → Default. A
// parsed date at/above the registry minimum resolves to the newest supported
// version not newer than the client date, so locally newer clients get the
// latest behavior this server can provide while in-range gaps stay conservative.
func (vi *versionInterceptor) resolve(raw string) (Version, error) {
	if raw == "" {
		return vi.reg.Default(), nil
	}
	all := vi.reg.All()
	v, err := Parse(raw)
	if err != nil {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported API version %q: must be YYYY-MM-DD; supported range: %s to %s",
				raw, all[0], all[len(all)-1]))
	}
	if string(v) < string(all[0]) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("unsupported API version %q; supported range: %s to %s",
				raw, all[0], all[len(all)-1]))
	}
	resolved := all[0]
	for _, supported := range all[1:] {
		if string(supported) > string(v) {
			break
		}
		resolved = supported
	}
	return resolved, nil
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
			// Error-path transforms for older-version clients. A change opts in
			// by implementing ErrorTransform; ApplyError skips the rest, so this
			// returns err untouched for every procedure no change targets. Uses
			// the same resolved version v as the success path below, so a client
			// cannot see a response down-converted to one version and an error
			// down-converted to another.
			//
			// This is the ONLY ApplyError call site. WrapStreamingHandler below
			// returns its handler's error raw, so the error-path seam is
			// unary-only by construction — see the SCOPE note on ErrorTransform
			// in transform.go, and the test that pins it.
			if vi.changes != nil {
				err = vi.changes.ApplyError(req.Spec().Procedure, err, v)
			}
			return nil, err
		}
		// Apply response transforms for older-version clients (newest→resolved order).
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
		// NOTE: the handler's error is returned raw below — no ApplyError call.
		// The error-path transform seam is unary-only; see the SCOPE note on
		// ErrorTransform in transform.go. Wiring it here is mechanically small
		// — the application error is just next(ctx, conn)'s return value — but
		// it would also need a decision about Send-side errors, which are
		// transport failures rather than application errors and have no
		// business being version-transformed. It is a deliberate future change
		// rather than an omission.
		if vi.changes != nil {
			conn = &transformingStreamingHandlerConn{
				StreamingHandlerConn: conn,
				changes:              vi.changes,
				procedure:            conn.Spec().Procedure,
				resolved:             v,
			}
		}
		return next(ctx, conn)
	}
}

type transformingStreamingHandlerConn struct {
	connect.StreamingHandlerConn
	changes   *Changes
	procedure string
	resolved  Version
}

type streamingHandlerCloser interface {
	Close(error) error
}

func (c *transformingStreamingHandlerConn) Send(msg any) error {
	c.changes.Apply(c.procedure, msg, c.resolved)
	return c.StreamingHandlerConn.Send(msg)
}

func (c *transformingStreamingHandlerConn) Close(err error) error {
	closer, ok := c.StreamingHandlerConn.(streamingHandlerCloser)
	if !ok {
		return fmt.Errorf("streaming handler connection does not support Close")
	}
	return closer.Close(err)
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
