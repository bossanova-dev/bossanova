package errortrack

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/getsentry/sentry-go"
)

type fakeRequest struct {
	connect.AnyRequest
	spec connect.Spec
}

func (r fakeRequest) Spec() connect.Spec {
	return r.spec
}

type fakeStreamingConn struct {
	connect.StreamingHandlerConn
	spec connect.Spec
}

func (c fakeStreamingConn) Spec() connect.Spec {
	return c.spec
}

func TestInterceptor_UnaryPanic_CapturedAndReturnedAsInternal(t *testing.T) {
	tr := newCaptureTransport()
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:       "https://k@o0.ingest.sentry.io/0",
		Transport: tr,
	}); err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	t.Cleanup(func() {
		sentry.Flush(time.Second)
		if client := sentry.CurrentHub().Client(); client != nil {
			client.Close()
		}
		sentry.CurrentHub().BindClient(nil)
	})

	interceptor := Interceptor()
	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		panic(errors.New("handler boom"))
	})
	req := fakeRequest{spec: connect.Spec{Procedure: "/test.Service/Method"}}

	// The interceptor must not re-panic — re-panicking would propagate the
	// panic up through CORS/auth and the outer Sentry HTTP recover, which
	// would then duplicate the report.
	resp, err := func() (resp connect.AnyResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("interceptor re-panicked with %v; want CodeInternal error", r)
			}
		}()
		return wrapped(context.Background(), req)
	}()
	if resp != nil {
		t.Fatalf("resp = %v, want nil", resp)
	}
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("err = %v (%T), want *connect.Error", err, err)
	}
	if connectErr.Code() != connect.CodeInternal {
		t.Fatalf("err code = %v, want CodeInternal", connectErr.Code())
	}

	events := waitForEvent(t, tr)
	if events[0].Tags["rpc.procedure"] != "/test.Service/Method" {
		t.Fatalf("rpc.procedure tag = %q, want /test.Service/Method", events[0].Tags["rpc.procedure"])
	}
}

func TestInterceptor_StreamingPanic_CapturedAndReturnedAsInternal(t *testing.T) {
	tr := newCaptureTransport()
	if err := sentry.Init(sentry.ClientOptions{
		Dsn:       "https://k@o0.ingest.sentry.io/0",
		Transport: tr,
	}); err != nil {
		t.Fatalf("sentry.Init: %v", err)
	}
	t.Cleanup(func() {
		sentry.Flush(time.Second)
		if client := sentry.CurrentHub().Client(); client != nil {
			client.Close()
		}
		sentry.CurrentHub().BindClient(nil)
	})

	interceptor := Interceptor()
	wrapped := interceptor.WrapStreamingHandler(func(context.Context, connect.StreamingHandlerConn) error {
		panic(errors.New("stream boom"))
	})
	conn := fakeStreamingConn{spec: connect.Spec{Procedure: "/test.Service/Stream"}}

	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("streaming interceptor re-panicked with %v; want CodeInternal error", r)
			}
		}()
		return wrapped(context.Background(), conn)
	}()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) {
		t.Fatalf("err = %v (%T), want *connect.Error", err, err)
	}
	if connectErr.Code() != connect.CodeInternal {
		t.Fatalf("err code = %v, want CodeInternal", connectErr.Code())
	}

	events := waitForEvent(t, tr)
	if events[0].Tags["rpc.procedure"] != "/test.Service/Stream" {
		t.Fatalf("rpc.procedure tag = %q, want /test.Service/Stream", events[0].Tags["rpc.procedure"])
	}
}

func TestInterceptor_UnaryNoPanic_PassThrough(t *testing.T) {
	interceptor := Interceptor()
	called := false
	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return nil, nil
	})
	req := fakeRequest{spec: connect.Spec{Procedure: "/test.Service/Method"}}

	_, err := wrapped(context.Background(), req)
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if !called {
		t.Fatal("handler was not called")
	}
}
