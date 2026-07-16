package socketauth

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

const testTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func unaryReq() connect.AnyRequest {
	return connect.NewRequest(&struct{}{})
}

func TestServerInterceptor_RejectsMissingHeader(t *testing.T) {
	called := false
	next := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		called = true
		return connect.NewResponse(&struct{}{}), nil
	}
	_, err := NewServerInterceptor(testTok).WrapUnary(next)(context.Background(), unaryReq())
	if called {
		t.Fatal("handler ran without auth")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
	if strings.Contains(err.Error(), testTok) {
		t.Fatal("error message leaked the token")
	}
}

func TestServerInterceptor_RejectsWrongToken(t *testing.T) {
	req := unaryReq()
	req.Header().Set(AuthHeader, "Bearer "+strings.Repeat("f", 64))
	_, err := NewServerInterceptor(testTok).WrapUnary(
		func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			return connect.NewResponse(&struct{}{}), nil
		})(context.Background(), req)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestServerInterceptor_RejectsMalformedScheme(t *testing.T) {
	for _, h := range []string{testTok, "Basic " + testTok, "Bearer not-hex", "Bearer "} {
		req := unaryReq()
		req.Header().Set(AuthHeader, h)
		_, err := NewServerInterceptor(testTok).WrapUnary(
			func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
				return connect.NewResponse(&struct{}{}), nil
			})(context.Background(), req)
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("header %q: code = %v, want Unauthenticated", h, connect.CodeOf(err))
		}
	}
}

func TestServerInterceptor_AcceptsValidToken(t *testing.T) {
	req := unaryReq()
	req.Header().Set(AuthHeader, "Bearer "+testTok)
	called := false
	_, err := NewServerInterceptor(testTok).WrapUnary(
		func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
			called = true
			return connect.NewResponse(&struct{}{}), nil
		})(context.Background(), req)
	if err != nil || !called {
		t.Fatalf("valid token rejected: called=%v err=%v", called, err)
	}
}

// TestServerInterceptor_EmptyOrInvalidTokenFailsClosed proves a server built
// from an empty or malformed token rejects EVERY request — including an empty
// "Bearer " credential — rather than failing open. Guards against a future
// caller that constructs the interceptor with an unvalidated token.
func TestServerInterceptor_EmptyOrInvalidTokenFailsClosed(t *testing.T) {
	for _, badTok := range []string{"", "not-hex", "abcd"} {
		for _, hdr := range []string{"", "Bearer ", "Bearer " + testTok} {
			req := unaryReq()
			if hdr != "" {
				req.Header().Set(AuthHeader, hdr)
			}
			called := false
			_, err := NewServerInterceptor(badTok).WrapUnary(
				func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
					called = true
					return connect.NewResponse(&struct{}{}), nil
				})(context.Background(), req)
			if called {
				t.Fatalf("token=%q header=%q: handler ran (fail-open)", badTok, hdr)
			}
			if connect.CodeOf(err) != connect.CodeUnauthenticated {
				t.Fatalf("token=%q header=%q: code = %v, want Unauthenticated", badTok, hdr, connect.CodeOf(err))
			}
		}
	}
}

func TestClientInterceptor_AttachesBearer(t *testing.T) {
	req := unaryReq()
	_, _ = NewClientInterceptor(testTok).WrapUnary(
		func(_ context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			if got := r.Header().Get(AuthHeader); got != "Bearer "+testTok {
				t.Fatalf("header = %q, want Bearer+token", got)
			}
			return connect.NewResponse(&struct{}{}), nil
		})(context.Background(), req)
}
