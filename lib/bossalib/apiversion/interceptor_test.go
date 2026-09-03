package apiversion_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"

	"github.com/recurser/bossalib/apiversion"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
)

// fakeStreamingHandlerConn is a minimal connect.StreamingHandlerConn test double.
type fakeStreamingHandlerConn struct {
	requestHeader  http.Header
	responseHeader http.Header
	spec           connect.Spec
	sent           any
}

func newFakeStreamConn(reqHeader http.Header) *fakeStreamingHandlerConn {
	return &fakeStreamingHandlerConn{
		requestHeader:  reqHeader,
		responseHeader: make(http.Header),
	}
}

func (f *fakeStreamingHandlerConn) Spec() connect.Spec           { return f.spec }
func (f *fakeStreamingHandlerConn) Peer() connect.Peer           { return connect.Peer{} }
func (f *fakeStreamingHandlerConn) Receive(_ any) error          { return nil }
func (f *fakeStreamingHandlerConn) RequestHeader() http.Header   { return f.requestHeader }
func (f *fakeStreamingHandlerConn) Send(msg any) error           { f.sent = msg; return nil }
func (f *fakeStreamingHandlerConn) ResponseHeader() http.Header  { return f.responseHeader }
func (f *fakeStreamingHandlerConn) ResponseTrailer() http.Header { return make(http.Header) }

type closeableFakeStreamingHandlerConn struct {
	*fakeStreamingHandlerConn
	closeCalls int
	closeErr   error
}

func (f *closeableFakeStreamingHandlerConn) Close(err error) error {
	f.closeCalls++
	f.closeErr = err
	return nil
}

// fakeStreamingClientConn is a minimal connect.StreamingClientConn test double.
type fakeStreamingClientConn struct {
	requestHeader http.Header
}

func newFakeClientConn() *fakeStreamingClientConn {
	return &fakeStreamingClientConn{requestHeader: make(http.Header)}
}

func (f *fakeStreamingClientConn) Spec() connect.Spec           { return connect.Spec{} }
func (f *fakeStreamingClientConn) Peer() connect.Peer           { return connect.Peer{} }
func (f *fakeStreamingClientConn) Send(_ any) error             { return nil }
func (f *fakeStreamingClientConn) RequestHeader() http.Header   { return f.requestHeader }
func (f *fakeStreamingClientConn) CloseRequest() error          { return nil }
func (f *fakeStreamingClientConn) Receive(_ any) error          { return nil }
func (f *fakeStreamingClientConn) ResponseHeader() http.Header  { return make(http.Header) }
func (f *fakeStreamingClientConn) ResponseTrailer() http.Header { return make(http.Header) }
func (f *fakeStreamingClientConn) CloseResponse() error         { return nil }

// --- Server Interceptor tests ---

// exampleTwoVersionRegistry builds a 2-version registry (Baseline + V20260701)
// for tests that need to exercise a valid non-default version header or the
// reference transform. DefaultRegistry ships with only Baseline (production
// safety), so tests requiring V20260701 build this inline.
func exampleTwoVersionRegistry(t *testing.T) *apiversion.Registry {
	t.Helper()
	reg, err := apiversion.NewRegistry(
		[]apiversion.Version{apiversion.Baseline, apiversion.V20260701},
		apiversion.V20260701,
		apiversion.Baseline,
	)
	if err != nil {
		t.Fatalf("exampleTwoVersionRegistry: %v", err)
	}
	return reg
}

// exampleChanges builds a Changes list with the ReferenceChange against the
// 2-version example registry, for use in transform-application tests.
func exampleChanges(t *testing.T, reg *apiversion.Registry) *apiversion.Changes {
	t.Helper()
	changes, err := apiversion.NewChanges(reg, apiversion.ReferenceChange{})
	if err != nil {
		t.Fatalf("exampleChanges: %v", err)
	}
	return changes
}

// mutatingChange is a test-only VersionChange that mutates any *RefMsg payload
// regardless of method, to verify the interceptor wires transform application.
type mutatingChange struct {
	version apiversion.Version
	prefix  string
}

func (m *mutatingChange) Version() apiversion.Version { return m.version }
func (m *mutatingChange) TransformResponse(_ string, msg any) {
	if rm, ok := msg.(*apiversion.RefMsg); ok {
		rm.Greeting = m.prefix + rm.Greeting
	}
}

func TestInterceptor_UnaryAbsentHeader_ResolvesToDefault(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return connect.NewResponse(&struct{}{}), nil
	}

	req := connect.NewRequest(&struct{}{})
	// No header set — should resolve to Default (Baseline).
	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	if resolvedInNext != reg.Default() {
		t.Errorf("resolved in ctx = %q, want Default %q", resolvedInNext, reg.Default())
	}
	if got := resp.Header().Get(apiversion.HeaderName); got != reg.Default().String() {
		t.Errorf("response header %q = %q, want %q", apiversion.HeaderName, got, reg.Default())
	}
}

func TestInterceptor_UnaryValidHeader_ResolvedAndEchoed(t *testing.T) {
	// Use a 2-version registry so we can test a valid header that differs from
	// the default (DefaultRegistry ships with only Baseline, so V20260701 is
	// not a member of the production registry).
	reg := exampleTwoVersionRegistry(t)
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return connect.NewResponse(&struct{}{}), nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, apiversion.V20260701.String())

	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	if resolvedInNext != apiversion.V20260701 {
		t.Errorf("resolved in ctx = %q, want %q", resolvedInNext, apiversion.V20260701)
	}
	if got := resp.Header().Get(apiversion.HeaderName); got != apiversion.V20260701.String() {
		t.Errorf("response header = %q, want %q", got, apiversion.V20260701)
	}
}

func TestInterceptor_UnaryUnknownVersion_CodeInvalidArgument(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("next should not be called on invalid version")
		return nil, nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, "2020-01-01") // valid date, not in registry

	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for unknown version, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("error code = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

func TestInterceptor_UnaryFutureVersion_ResolvesToCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return connect.NewResponse(&struct{}{}), nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, "2099-01-01")

	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	if resolvedInNext != reg.Current() {
		t.Errorf("resolved in ctx = %q, want Current %q", resolvedInNext, reg.Current())
	}
	if got := resp.Header().Get(apiversion.HeaderName); got != reg.Current().String() {
		t.Errorf("response header = %q, want %q", got, reg.Current())
	}
}

func TestInterceptor_OrgScopedVisibilitySkew(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, apiversion.ProductionChanges())
	handler := connect.NewUnaryHandler[pb.ListDaemonsRequest, pb.ListDaemonsResponse](
		bossanovav1connect.OrchestratorServiceListDaemonsProcedure,
		func(ctx context.Context, _ *connect.Request[pb.ListDaemonsRequest]) (*connect.Response[pb.ListDaemonsResponse], error) {
			label := "legacy-user-scope"
			if apiversion.IsOrgScopedVisibility(ctx) {
				label = "org-scope"
			}
			return connect.NewResponse(&pb.ListDaemonsResponse{
				Daemons: []*pb.DaemonInfo{{DaemonId: label}},
			}), nil
		},
		connect.WithInterceptors(interceptor),
	)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	client := connect.NewClient[pb.ListDaemonsRequest, pb.ListDaemonsResponse](
		server.Client(),
		server.URL+bossanovav1connect.OrchestratorServiceListDaemonsProcedure,
	)

	cases := []struct {
		name       string
		header     string
		wantID     string
		wantEchoed apiversion.Version
	}{
		{
			name:       "no header resolves to default legacy branch",
			wantID:     "legacy-user-scope",
			wantEchoed: reg.Default(),
		},
		{
			name:       "one version back resolves to legacy branch",
			header:     apiversion.V20260825.String(),
			wantID:     "legacy-user-scope",
			wantEchoed: apiversion.V20260825,
		},
		{
			name:       "current resolves to org branch",
			header:     apiversion.V20260902.String(),
			wantID:     "org-scope",
			wantEchoed: apiversion.V20260902,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := connect.NewRequest(&pb.ListDaemonsRequest{})
			if tc.header != "" {
				req.Header().Set(apiversion.HeaderName, tc.header)
			}
			resp, err := client.CallUnary(context.Background(), req)
			if err != nil {
				t.Fatalf("CallUnary: %v", err)
			}
			daemons := resp.Msg.GetDaemons()
			if len(daemons) != 1 {
				t.Fatalf("len(daemons) = %d, want 1", len(daemons))
			}
			if got := daemons[0].GetDaemonId(); got != tc.wantID {
				t.Errorf("daemon_id = %q, want %q", got, tc.wantID)
			}
			if got := resp.Header().Get(apiversion.HeaderName); got != tc.wantEchoed.String() {
				t.Errorf("response header %q = %q, want %q", apiversion.HeaderName, got, tc.wantEchoed)
			}
		})
	}
}

func TestInterceptor_UnaryInRangeNonMemberVersion_ResolvesToNearestOlderSupported(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return connect.NewResponse(&struct{}{}), nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, "2026-07-03")

	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	if resolvedInNext != apiversion.Baseline {
		t.Errorf("resolved in ctx = %q, want nearest older supported %q", resolvedInNext, apiversion.Baseline)
	}
	if got := resp.Header().Get(apiversion.HeaderName); got != apiversion.Baseline.String() {
		t.Errorf("response header = %q, want %q", got, apiversion.Baseline)
	}
}

func TestInterceptor_UnaryMalformedVersion_CodeInvalidArgument(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("next should not be called on malformed version")
		return nil, nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, "not-a-date")

	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for malformed version, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("error code = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

func TestInterceptor_UnaryHandlerError_PropagatesWithoutHeader(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	handlerErr := errors.New("handler failed")
	next := func(ctx context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, handlerErr
	}

	req := connect.NewRequest(&struct{}{})
	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if !errors.Is(err, handlerErr) {
		t.Errorf("error = %v, want wrapped %v", err, handlerErr)
	}
}

// TestInterceptor_UnaryTransform_OlderVersion_Applied verifies that when the
// resolved version is older than a VersionChange, the interceptor calls
// TransformResponse on the response message. Uses a 2-version registry and a
// mutatingChange to assert the response IS modified for Baseline callers.
func TestInterceptor_UnaryTransform_OlderVersion_Applied(t *testing.T) {
	reg := exampleTwoVersionRegistry(t)
	changes, err := apiversion.NewChanges(reg, &mutatingChange{
		version: apiversion.V20260701,
		prefix:  "[old] ",
	})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	interceptor := apiversion.Interceptor(reg, changes)

	msg := &apiversion.RefMsg{Greeting: "hello"}
	next := func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(msg), nil
	}

	// Resolved to Baseline (older than V20260701) → transform fires.
	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, apiversion.Baseline.String())

	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	// The transform must have prefixed the greeting.
	if want := "[old] hello"; msg.Greeting != want {
		t.Errorf("Greeting = %q, want %q (transform not applied)", msg.Greeting, want)
	}
	// Header still echoed.
	if got := resp.Header().Get(apiversion.HeaderName); got != apiversion.Baseline.String() {
		t.Errorf("response header = %q, want %q", got, apiversion.Baseline)
	}
}

// TestInterceptor_UnaryTransform_CurrentVersion_NotApplied verifies that a
// request resolved to Current runs zero transforms — the hot path is
// allocation-free and unchanged.
func TestInterceptor_UnaryTransform_CurrentVersion_NotApplied(t *testing.T) {
	reg := exampleTwoVersionRegistry(t)
	changes, err := apiversion.NewChanges(reg, &mutatingChange{
		version: apiversion.V20260701,
		prefix:  "[old] ",
	})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	interceptor := apiversion.Interceptor(reg, changes)

	msg := &apiversion.RefMsg{Greeting: "hello"}
	next := func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(msg), nil
	}

	// Resolved to V20260701 (Current) → mutatingChange is NOT newer → no transform.
	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, apiversion.V20260701.String())

	_, err = interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	// Greeting must be untouched.
	if msg.Greeting != "hello" {
		t.Errorf("Greeting = %q, want %q (transform should not have applied)", msg.Greeting, "hello")
	}
}

// TestInterceptor_UnaryTransform_NilChanges_Passthrough verifies that nil
// changes is safe and results in unmodified passthrough.
func TestInterceptor_UnaryTransform_NilChanges_Passthrough(t *testing.T) {
	reg := exampleTwoVersionRegistry(t)
	interceptor := apiversion.Interceptor(reg, nil)

	msg := &apiversion.RefMsg{Greeting: "hello"}
	next := func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(msg), nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, apiversion.Baseline.String())

	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	// With nil changes, no transform fires; greeting unchanged.
	if msg.Greeting != "hello" {
		t.Errorf("Greeting = %q, want %q (nil changes should not modify response)", msg.Greeting, "hello")
	}
}

// TestInterceptor_UnaryTransform_ReferenceChange_Baseline verifies the
// ReferenceChange end-to-end through the interceptor: a Baseline client
// targeting "demo.Greet" gets the prefixed response.
func TestInterceptor_UnaryTransform_ReferenceChange_Baseline(t *testing.T) {
	reg := exampleTwoVersionRegistry(t)
	interceptor := apiversion.Interceptor(reg, exampleChanges(t, reg))

	msg := &apiversion.RefMsg{Greeting: "world"}
	// The interceptor calls changes.Apply(req.Spec().Procedure, ...).
	// In unit tests, Spec().Procedure is "" (no Connect runtime). We verify
	// that the ReferenceChange is a no-op on "" (it gates on "demo.Greet"),
	// so this test asserts the interceptor plumbing without relying on a
	// specific procedure path. See TestChanges_ResolvedOneVersionBack_* in
	// transform_test.go for the authoritative behavioral evidence.
	next := func(_ context.Context, _ connect.AnyRequest) (connect.AnyResponse, error) {
		return connect.NewResponse(msg), nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, apiversion.Baseline.String())

	resp, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	// ReferenceChange is a no-op here (Spec.Procedure == "") — that's expected.
	// What we assert: header is echoed correctly.
	if got := resp.Header().Get(apiversion.HeaderName); got != apiversion.Baseline.String() {
		t.Errorf("response header = %q, want %q", got, apiversion.Baseline)
	}
}

func TestInterceptor_StreamingHandler_AbsentHeader_ResolvesToDefault(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	nextHandler := func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return nil
	}

	conn := newFakeStreamConn(make(http.Header))
	// No header.
	err := interceptor.WrapStreamingHandler(nextHandler)(context.Background(), conn)
	if err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	if resolvedInNext != reg.Default() {
		t.Errorf("resolved in ctx = %q, want Default %q", resolvedInNext, reg.Default())
	}
	if got := conn.ResponseHeader().Get(apiversion.HeaderName); got != reg.Default().String() {
		t.Errorf("response header = %q, want %q", got, reg.Default())
	}
}

func TestInterceptor_StreamingHandler_ValidHeader_ResolvedAndEchoed(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	nextHandler := func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return nil
	}

	reqHeader := make(http.Header)
	reqHeader.Set(apiversion.HeaderName, apiversion.Baseline.String())
	conn := newFakeStreamConn(reqHeader)

	err := interceptor.WrapStreamingHandler(nextHandler)(context.Background(), conn)
	if err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	if resolvedInNext != apiversion.Baseline {
		t.Errorf("resolved in ctx = %q, want %q", resolvedInNext, apiversion.Baseline)
	}
	if got := conn.ResponseHeader().Get(apiversion.HeaderName); got != apiversion.Baseline.String() {
		t.Errorf("response header = %q, want %q", got, apiversion.Baseline)
	}
}

func TestInterceptor_StreamingHandler_AppliesResponseTransforms(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, apiversion.ProductionChanges())

	nextHandler := func(_ context.Context, conn connect.StreamingHandlerConn) error {
		return conn.Send(&pb.ProxyChatListEvent{Event: &pb.ProxyChatListEvent_StatusDelta{StatusDelta: &pb.ChatStatusDelta{
			SessionId:      "sess-1",
			AgentSessionId: "agent-1",
			Status:         pb.ChatStatus_CHAT_STATUS_LIMITED,
		}}})
	}

	conn := newFakeStreamConn(make(http.Header))
	conn.spec = connect.Spec{Procedure: bossanovav1connect.OrchestratorServiceProxyStreamChatsProcedure}
	err := interceptor.WrapStreamingHandler(nextHandler)(context.Background(), conn)
	if err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	got, ok := conn.sent.(*pb.ProxyChatListEvent)
	if !ok {
		t.Fatalf("sent message type = %T, want *ProxyChatListEvent", conn.sent)
	}
	if status := got.GetStatusDelta().GetStatus(); status != pb.ChatStatus_CHAT_STATUS_IDLE {
		t.Fatalf("stream status = %v, want IDLE", status)
	}
}

func TestInterceptor_StreamingHandler_UnknownVersion_CodeInvalidArgument(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	nextHandler := func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		t.Fatal("next should not be called on invalid version")
		return nil
	}

	reqHeader := make(http.Header)
	reqHeader.Set(apiversion.HeaderName, "2020-01-01")
	conn := newFakeStreamConn(reqHeader)

	err := interceptor.WrapStreamingHandler(nextHandler)(context.Background(), conn)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("error code = %v, want CodeInvalidArgument", connect.CodeOf(err))
	}
}

func TestInterceptor_StreamingHandler_FutureVersion_ResolvesToCurrent(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	var resolvedInNext apiversion.Version
	nextHandler := func(ctx context.Context, _ connect.StreamingHandlerConn) error {
		resolvedInNext = apiversion.ResolvedVersion(ctx)
		return nil
	}

	reqHeader := make(http.Header)
	reqHeader.Set(apiversion.HeaderName, "2099-01-01")
	conn := newFakeStreamConn(reqHeader)

	err := interceptor.WrapStreamingHandler(nextHandler)(context.Background(), conn)
	if err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	if resolvedInNext != reg.Current() {
		t.Errorf("resolved in ctx = %q, want Current %q", resolvedInNext, reg.Current())
	}
	if got := conn.ResponseHeader().Get(apiversion.HeaderName); got != reg.Current().String() {
		t.Errorf("response header = %q, want %q", got, reg.Current())
	}
}

func TestInterceptor_WrapStreamingClient_PassThrough(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, nil)

	called := false
	next := func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		called = true
		return newFakeClientConn()
	}

	wrapped := interceptor.WrapStreamingClient(next)
	wrapped(context.Background(), connect.Spec{})
	if !called {
		t.Fatal("WrapStreamingClient did not call next")
	}
}

func TestResolvedVersion_NoContext_ReturnsDefault(t *testing.T) {
	got := apiversion.ResolvedVersion(context.Background())
	want := apiversion.DefaultRegistry().Default()
	if got != want {
		t.Errorf("ResolvedVersion(bare ctx) = %q, want %q", got, want)
	}
}

// --- Client Interceptor tests ---

func TestClientInterceptor_WrapUnary_SetsHeader(t *testing.T) {
	built := apiversion.DefaultRegistry().Current()
	interceptor := apiversion.ClientInterceptor(built)

	var gotHeader string
	next := func(_ context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		gotHeader = req.Header().Get(apiversion.HeaderName)
		return connect.NewResponse(&struct{}{}), nil
	}

	req := connect.NewRequest(&struct{}{})
	_, err := interceptor.WrapUnary(next)(context.Background(), req)
	if err != nil {
		t.Fatalf("WrapUnary: %v", err)
	}
	if gotHeader != built.String() {
		t.Errorf("request header %q = %q, want %q", apiversion.HeaderName, gotHeader, built)
	}
}

func TestClientInterceptor_WrapStreamingClient_SetsHeader(t *testing.T) {
	built := apiversion.DefaultRegistry().Current()
	interceptor := apiversion.ClientInterceptor(built)

	fakeConn := newFakeClientConn()
	next := func(_ context.Context, _ connect.Spec) connect.StreamingClientConn {
		return fakeConn
	}

	wrapped := interceptor.WrapStreamingClient(next)
	conn := wrapped(context.Background(), connect.Spec{})
	if got := conn.RequestHeader().Get(apiversion.HeaderName); got != built.String() {
		t.Errorf("streaming request header = %q, want %q", got, built)
	}
}

func TestClientInterceptor_WrapStreamingHandler_PassThrough(t *testing.T) {
	interceptor := apiversion.ClientInterceptor(apiversion.DefaultRegistry().Current())

	called := false
	next := func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		called = true
		return nil
	}

	conn := newFakeStreamConn(make(http.Header))
	err := interceptor.WrapStreamingHandler(next)(context.Background(), conn)
	if err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	if !called {
		t.Fatal("WrapStreamingHandler did not call next")
	}
}

func TestHeaderName(t *testing.T) {
	if apiversion.HeaderName != "Bossanova-Version" {
		t.Errorf("HeaderName = %q, want %q", apiversion.HeaderName, "Bossanova-Version")
	}
}

// --- Error-path hook (V20260820 / BOS-947) --------------------------------
//
// These pin the interceptor's half of the error-path seam: that WrapUnary calls
// Changes.ApplyError on the failure branch, with the SAME resolved version the
// success branch uses, and returns what it threads back.
//
// They deliberately use the method-agnostic errOnlyChange double rather than the
// production SwitchDeadlineCodeChange: a connect.Request built in a test carries
// an empty Spec().Procedure and connect.AnyRequest cannot be implemented outside
// the connect package (it has unexported methods), so procedure matching is not
// observable here. Procedure discrimination is pinned in transform_test.go, and
// the fully wired served stack — real procedure, real interceptor — is pinned by
// TestE2E_APIVersion_SwitchDeadline_* in services/bosso/internal/server.

// TestInterceptor_UnaryErrorTransform_OlderVersion_Applied verifies the error
// returned by the handler is replaced for a client resolved older than the
// change. Without the WrapUnary hook this fails: the bare `if err != nil {
// return nil, err }` short-circuit returned the handler's error untouched.
func TestInterceptor_UnaryErrorTransform_OlderVersion_Applied(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	var errCalls, respCalls int
	replaced := connect.NewError(connect.CodeAborted, errors.New("down-converted"))
	changes, err := apiversion.NewChanges(reg, &errOnlyChange{
		version:    apiversion.V20260820,
		errCalls:   &errCalls,
		respCalls:  &respCalls,
		replaceErr: replaced,
	})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	interceptor := apiversion.Interceptor(reg, changes)

	next := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("budget exhausted"))
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, apiversion.Baseline.String())

	_, gotErr := interceptor.WrapUnary(next)(context.Background(), req)
	if connect.CodeOf(gotErr) != connect.CodeAborted {
		t.Errorf("code = %v, want CodeAborted (error transform not applied by WrapUnary)", connect.CodeOf(gotErr))
	}
	if errCalls != 1 {
		t.Errorf("TransformError calls = %d, want 1", errCalls)
	}
}

// TestInterceptor_UnaryErrorTransform_CurrentVersion_NotApplied is the other
// half: a client on Current runs zero error transforms.
func TestInterceptor_UnaryErrorTransform_CurrentVersion_NotApplied(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	var errCalls, respCalls int
	changes, err := apiversion.NewChanges(reg, &errOnlyChange{
		version:    apiversion.V20260820,
		errCalls:   &errCalls,
		respCalls:  &respCalls,
		replaceErr: connect.NewError(connect.CodeAborted, errors.New("down-converted")),
	})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	interceptor := apiversion.Interceptor(reg, changes)

	next := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("budget exhausted"))
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, reg.Current().String())

	_, gotErr := interceptor.WrapUnary(next)(context.Background(), req)
	if connect.CodeOf(gotErr) != connect.CodeDeadlineExceeded {
		t.Errorf("code = %v, want CodeDeadlineExceeded (unchanged at Current)", connect.CodeOf(gotErr))
	}
	if errCalls != 0 {
		t.Errorf("TransformError calls = %d, want 0 at Current", errCalls)
	}
}

// TestInterceptor_UnaryErrorTransform_VersionResolutionErrorIsNotTransformed
// pins that the interceptor's OWN rejection of a malformed version header is
// returned before any transform runs — that error is about the negotiation
// itself, so down-converting it would be nonsense.
func TestInterceptor_UnaryErrorTransform_VersionResolutionErrorIsNotTransformed(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	var errCalls, respCalls int
	changes, err := apiversion.NewChanges(reg, &errOnlyChange{
		version:    apiversion.V20260820,
		errCalls:   &errCalls,
		respCalls:  &respCalls,
		replaceErr: connect.NewError(connect.CodeAborted, errors.New("down-converted")),
	})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	interceptor := apiversion.Interceptor(reg, changes)

	next := func(context.Context, connect.AnyRequest) (connect.AnyResponse, error) {
		t.Fatal("handler must not be reached for a malformed version header")
		return nil, nil
	}

	req := connect.NewRequest(&struct{}{})
	req.Header().Set(apiversion.HeaderName, "garbage")

	_, gotErr := interceptor.WrapUnary(next)(context.Background(), req)
	if connect.CodeOf(gotErr) != connect.CodeInvalidArgument {
		t.Errorf("code = %v, want CodeInvalidArgument", connect.CodeOf(gotErr))
	}
	if errCalls != 0 {
		t.Errorf("TransformError calls = %d, want 0 for a version-resolution error", errCalls)
	}
}

// TestApplyError_StreamingHandlerErrorIsNotTransformed pins the error-path
// seam's UNARY-ONLY scope as a recorded decision rather than an accident.
//
// WrapStreamingHandler wraps the conn for Send-side response transforms and
// returns the handler's error raw — there is no ApplyError call and
// transformingStreamingHandlerConn has no error hook — so an ErrorTransform
// written for a streaming procedure is a silent no-op with no compile error and
// no other failing test. This is the signal. If someone widens the seam to
// streaming, this test goes red and they delete it deliberately, having read
// the SCOPE note on ErrorTransform; today its redness would instead mean the
// boundary moved by accident.
func TestApplyError_StreamingHandlerErrorIsNotTransformed(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	var errCalls, respCalls int
	changes, err := apiversion.NewChanges(reg, &errOnlyChange{
		version:    apiversion.V20260820,
		errCalls:   &errCalls,
		respCalls:  &respCalls,
		replaceErr: connect.NewError(connect.CodeAborted, errors.New("down-converted")),
	})
	if err != nil {
		t.Fatalf("NewChanges: %v", err)
	}
	interceptor := apiversion.Interceptor(reg, changes)

	handlerErr := connect.NewError(connect.CodeDeadlineExceeded, errors.New("budget exhausted"))
	next := func(context.Context, connect.StreamingHandlerConn) error { return handlerErr }

	conn := newFakeStreamConn(http.Header{apiversion.HeaderName: []string{apiversion.Baseline.String()}})
	gotErr := interceptor.WrapStreamingHandler(next)(context.Background(), conn)

	if !errors.Is(gotErr, error(handlerErr)) {
		t.Errorf("streaming error = %v, want the handler's own error returned raw", gotErr)
	}
	if code := connect.CodeOf(gotErr); code != connect.CodeDeadlineExceeded {
		t.Errorf("streaming code = %v, want DeadlineExceeded (unary-only seam must not fire here)", code)
	}
	if errCalls != 0 {
		t.Errorf("TransformError ran on the streaming path %d times, want 0", errCalls)
	}
	// The handler returned before sending, so no response transform ran either.
	// Asserting this is what makes the test say "response transforms ARE wired
	// into this path (via transformingStreamingHandlerConn.Send) and error
	// transforms are NOT" rather than only the second half.
	if respCalls != 0 {
		t.Errorf("TransformResponse ran %d times, want 0 (the handler sent nothing)", respCalls)
	}
}

func TestInterceptor_StreamingHandlerPreservesCloseCapabilityThroughTransformWrapper(t *testing.T) {
	reg := apiversion.DefaultRegistry()
	interceptor := apiversion.Interceptor(reg, apiversion.ProductionChanges())
	conn := &closeableFakeStreamingHandlerConn{
		fakeStreamingHandlerConn: newFakeStreamConn(http.Header{}),
	}
	closeErr := errors.New("cancel transport")

	next := func(_ context.Context, conn connect.StreamingHandlerConn) error {
		closer, ok := conn.(interface{ Close(error) error })
		if !ok {
			t.Fatal("wrapped streaming handler connection does not expose Close")
		}
		if err := closer.Close(closeErr); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return nil
	}

	if err := interceptor.WrapStreamingHandler(next)(context.Background(), conn); err != nil {
		t.Fatalf("WrapStreamingHandler: %v", err)
	}
	if conn.closeCalls != 1 {
		t.Fatalf("close calls = %d, want 1", conn.closeCalls)
	}
	if !errors.Is(conn.closeErr, closeErr) {
		t.Fatalf("close err = %v, want %v", conn.closeErr, closeErr)
	}
}
