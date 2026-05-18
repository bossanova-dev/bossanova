package errortrack

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"github.com/getsentry/sentry-go"
)

// Interceptor returns a ConnectRPC interceptor that captures handler panics,
// reports them to Sentry, and converts them into a CodeInternal response. The
// panic is intentionally not re-panicked: doing so would leak past every
// outer HTTP middleware (CORS, auth, the Sentry HTTP recover) and the
// outermost Sentry HTTP layer would double-report the same panic that this
// interceptor already captured.
func Interceptor() connect.Interceptor {
	return sentryInterceptor{}
}

type sentryInterceptor struct{}

func (sentryInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (resp connect.AnyResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = capturePanicAsError(req.Spec().Procedure, r)
			}
		}()
		return next(ctx, req)
	}
}

func (sentryInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (sentryInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = capturePanicAsError(conn.Spec().Procedure, r)
			}
		}()
		return next(ctx, conn)
	}
}

func capturePanicAsError(procedure string, r any) error {
	hub := sentry.CurrentHub().Clone()
	hub.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("rpc.procedure", procedure)
	})
	hub.Recover(r)
	return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
}
