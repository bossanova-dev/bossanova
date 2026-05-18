package errortrack

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHTTPHandler_CapturesPanic verifies the outermost Sentry HTTP recover
// reports panics that escape pre-handler middleware. This is the safety net
// for routes whose handler doesn't have its own capture (CORS, auth,
// webhooks, /metrics, /ws/attach). Connect RPC handlers convert their
// panics into CodeInternal errors inside the interceptor and never re-panic,
// so the outer Sentry wrapper sees nothing for them.
func TestHTTPHandler_CapturesPanic(t *testing.T) {
	transport := newCaptureTransport()
	closeFn, err := Init(Opts{
		DSN:       "https://k@o0.ingest.sentry.io/0",
		App:       "test",
		Transport: transport,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	t.Cleanup(closeFn)

	inner := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic(errors.New("webhook boom"))
	})
	wrapped := HTTPHandler(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", nil)
	func() {
		defer func() { _ = recover() }()
		wrapped.ServeHTTP(rr, req)
	}()

	events := waitForEvent(t, transport)
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
}
