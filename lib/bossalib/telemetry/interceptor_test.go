package telemetry

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
)

// interceptorRequest embeds a nil connect.AnyRequest and overrides only the
// methods the interceptor calls. Any() must be one of them: the interceptor
// hands the decoded request message to the classifier, so without this override
// the embedded nil interface panics on every classified call.
type interceptorRequest struct {
	connect.AnyRequest
	spec connect.Spec
	msg  any
}

func (r interceptorRequest) Spec() connect.Spec { return r.spec }
func (r interceptorRequest) Any() any           { return r.msg }

type interceptorCapture struct {
	event      Event
	distinctID string
	properties map[string]any
}

type interceptorRecorder struct{ captures []interceptorCapture }

func (r *interceptorRecorder) Capture(_ context.Context, event Event, distinctID string, properties map[string]any) {
	copy := make(map[string]any, len(properties))
	for key, value := range properties {
		copy[key] = value
	}
	r.captures = append(r.captures, interceptorCapture{event: event, distinctID: distinctID, properties: copy})
}

func (r *interceptorRecorder) Identify(context.Context, string, map[string]any) {}
func (r *interceptorRecorder) Alias(context.Context, string, string)            {}
func (r *interceptorRecorder) Close()                                           {}

func TestInterceptor_MutatingSuccessCapturesAllowedProperties(t *testing.T) {
	recorder := &interceptorRecorder{}
	interceptor := Interceptor(recorder, func(procedure string, _ any) (string, bool) {
		return "sessions", procedure == "/bossanova.v1.OrchestratorService/ProxyStopSession"
	}, func(context.Context) string { return "user:test" })

	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	})
	_, err := wrapped(context.Background(), interceptorRequest{spec: connect.Spec{Procedure: "/bossanova.v1.OrchestratorService/ProxyStopSession"}})
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}

	if len(recorder.captures) != 1 {
		t.Fatalf("captures = %d, want 1", len(recorder.captures))
	}
	capture := recorder.captures[0]
	if capture.event != EventCloudActionInvoked {
		t.Fatalf("event = %q, want %q", capture.event, EventCloudActionInvoked)
	}
	t.Logf("captured %s", capture.event)
	if capture.distinctID != "user:test" {
		t.Fatalf("distinct ID = %q, want user:test", capture.distinctID)
	}
	want := map[string]any{
		"command":      "ProxyStopSession",
		"status":       "success",
		"product_area": "sessions",
		"source":       "cloud",
	}
	if len(capture.properties) != len(want) {
		t.Fatalf("property count = %d, want %d: %#v", len(capture.properties), len(want), capture.properties)
	}
	for key, wantValue := range want {
		if got := capture.properties[key]; got != wantValue {
			t.Errorf("property %q = %#v, want %#v", key, got, wantValue)
		}
	}
	for key := range capture.properties {
		if !IsAllowedProperty(EventCloudActionInvoked, key) {
			t.Errorf("property %q is not registry-authorized", key)
		}
	}
}

func TestInterceptor_ExcludedProcedureDoesNotCapture(t *testing.T) {
	recorder := &interceptorRecorder{}
	interceptor := Interceptor(recorder, func(string, any) (string, bool) { return "", false }, func(context.Context) string { return "user:test" })

	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	})
	_, err := wrapped(context.Background(), interceptorRequest{spec: connect.Spec{Procedure: "/bossanova.v1.OrchestratorService/ProxyListSessions"}})
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if len(recorder.captures) != 0 {
		t.Fatalf("captures = %d, want 0", len(recorder.captures))
	}
}

func TestInterceptor_ErrorCapturesStatusAndConnectCode(t *testing.T) {
	recorder := &interceptorRecorder{}
	interceptor := Interceptor(recorder, func(string, any) (string, bool) { return "sessions", true }, func(context.Context) string { return "user:test" })

	wantErr := connect.NewError(connect.CodeInvalidArgument, errors.New("bad request"))
	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, wantErr
	})
	_, err := wrapped(context.Background(), interceptorRequest{spec: connect.Spec{Procedure: "/bossanova.v1.OrchestratorService/ProxyStopSession"}})
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}

	if len(recorder.captures) != 1 {
		t.Fatalf("captures = %d, want 1", len(recorder.captures))
	}
	properties := recorder.captures[0].properties
	if got := properties["status"]; got != "error" {
		t.Errorf("status = %#v, want error", got)
	}
	if got := properties["error_code"]; got != "invalid_argument" {
		t.Errorf("error_code = %#v, want invalid_argument", got)
	}
}

func TestInterceptor_PanicIsTransparent(t *testing.T) {
	recorder := &interceptorRecorder{}
	interceptor := Interceptor(recorder, func(string, any) (string, bool) { return "sessions", true }, func(context.Context) string { return "user:test" })
	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		panic("handler panic")
	})

	defer func() {
		if got := recover(); got != "handler panic" {
			t.Fatalf("panic = %#v, want handler panic", got)
		}
		if len(recorder.captures) != 0 {
			t.Fatalf("captures = %d, want 0", len(recorder.captures))
		}
	}()
	_, _ = wrapped(context.Background(), interceptorRequest{spec: connect.Spec{Procedure: "/bossanova.v1.OrchestratorService/ProxyStopSession"}})
}

func TestInterceptor_MissingAuthenticationDoesNotCapture(t *testing.T) {
	recorder := &interceptorRecorder{}
	interceptor := Interceptor(recorder, func(string, any) (string, bool) { return "sessions", true }, func(context.Context) string { return "" })

	wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(&struct{}{}), nil
	})
	_, err := wrapped(context.Background(), interceptorRequest{spec: connect.Spec{Procedure: "/bossanova.v1.OrchestratorService/ProxyStopSession"}})
	if err != nil {
		t.Fatalf("wrapped: %v", err)
	}
	if len(recorder.captures) != 0 {
		t.Fatalf("captures = %d, want 0", len(recorder.captures))
	}
}

func TestProcedureCommand(t *testing.T) {
	tests := []struct {
		name      string
		procedure string
		want      string
		wantOK    bool
	}{
		{name: "connect procedure", procedure: "/bossanova.v1.OrchestratorService/ProxyStopSession", want: "ProxyStopSession", wantOK: true},
		{name: "bare procedure", procedure: "ProxyStopSession", want: "ProxyStopSession", wantOK: true},
		{name: "surrounding whitespace", procedure: "  /bossanova.v1.OrchestratorService/ProxyStopSession  ", want: "ProxyStopSession", wantOK: true},
		{name: "empty", procedure: "", want: "", wantOK: false},
		{name: "whitespace", procedure: "   ", want: "", wantOK: false},
		{name: "missing method", procedure: "/bossanova.v1.OrchestratorService/", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := procedureCommand(tt.procedure)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("procedureCommand(%q) = (%q, %v), want (%q, %v)", tt.procedure, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

// TestInterceptor_ClassifierReceivesDecodedRequest pins the widened
// ProcedureClassifier contract: the interceptor hands the decoded request
// message to the classifier, and honours a classifier that rejects on the
// message alone even when the procedure is otherwise recordable.
func TestInterceptor_ClassifierReceivesDecodedRequest(t *testing.T) {
	type listRequest struct{ refresh bool }

	tests := []struct {
		name        string
		message     any
		wantCapture bool
	}{
		{name: "message accepted", message: &listRequest{refresh: true}, wantCapture: true},
		{name: "message rejected", message: &listRequest{refresh: false}},
		{name: "nil message rejected", message: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &interceptorRecorder{}
			var seen []any
			interceptor := Interceptor(recorder, func(_ string, request any) (string, bool) {
				seen = append(seen, request)
				list, ok := request.(*listRequest)
				if !ok || !list.refresh {
					return "", false
				}
				return "accounts", true
			}, func(context.Context) string { return "user:test" })

			wrapped := interceptor.WrapUnary(func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
				return connect.NewResponse(&struct{}{}), nil
			})
			_, err := wrapped(context.Background(), interceptorRequest{
				spec: connect.Spec{Procedure: "/bossanova.v1.OrchestratorService/ProxyManageListAccounts"},
				msg:  tt.message,
			})
			if err != nil {
				t.Fatalf("wrapped: %v", err)
			}
			if len(seen) != 1 {
				t.Fatalf("classifier calls = %d, want 1", len(seen))
			}
			if seen[0] != tt.message {
				t.Fatalf("classifier request = %#v, want %#v", seen[0], tt.message)
			}
			wantCaptures := 0
			if tt.wantCapture {
				wantCaptures = 1
			}
			if len(recorder.captures) != wantCaptures {
				t.Fatalf("captures = %d, want %d", len(recorder.captures), wantCaptures)
			}
		})
	}
}
