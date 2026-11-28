package plugin

import (
	"context"
	"reflect"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	sharedplugin "github.com/recurser/bossalib/contract/plugin/plugin"
	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/kit/config"
)

func TestAgentRunner_StartAgentRun_RoutesToCorrectMethod(t *testing.T) {
	t.Skip("integration-style — verified end-to-end in Task 25")
	// Placeholder for now; the full smoke test happens in the integration
	// test that spins up a real plugin process. The agentRunnerGRPCClient
	// method-routing is exercised in plugin_runner_test.go via the
	// AgentRunnerClient interface.
	_ = bossanovav1.StartAgentRunRequest{}
}

func TestDegradeRotationCapability_UnimplementedIsNonSupporting(t *testing.T) {
	t.Run("Unimplemented degrades to non-supporting, no error", func(t *testing.T) {
		err := grpcstatus.Error(codes.Unimplemented, "RotationCapability not implemented")

		resp, gotErr := degradeRotationCapability(nil, err)

		if gotErr != nil {
			t.Fatalf("expected nil error, got %v", gotErr)
		}
		if resp == nil {
			t.Fatal("expected non-nil response")
		}
		if resp.SupportsRotation {
			t.Errorf("expected SupportsRotation=false, got true")
		}
	})

	t.Run("success passes response through unchanged", func(t *testing.T) {
		want := &bossanovav1.RotationCapabilityResponse{
			SupportsRotation: true,
			AuthKind:         bossanovav1.AuthKind_AUTH_KIND_ENV_TOKEN,
		}

		resp, gotErr := degradeRotationCapability(want, nil)

		if gotErr != nil {
			t.Fatalf("expected nil error, got %v", gotErr)
		}
		if resp != want {
			t.Fatalf("expected response to be returned unchanged, got %+v", resp)
		}
		if !resp.SupportsRotation {
			t.Errorf("expected SupportsRotation=true, got false")
		}
		if resp.AuthKind != bossanovav1.AuthKind_AUTH_KIND_ENV_TOKEN {
			t.Errorf("expected AuthKind=AUTH_KIND_ENV_TOKEN, got %v", resp.AuthKind)
		}
	})

	t.Run("non-Unimplemented error propagates unchanged", func(t *testing.T) {
		wantErr := grpcstatus.Error(codes.Internal, "boom")

		resp, gotErr := degradeRotationCapability(nil, wantErr)

		if gotErr == nil {
			t.Fatal("expected non-nil error")
		}
		if grpcstatus.Code(gotErr) != codes.Internal {
			t.Errorf("expected code Internal, got %v", grpcstatus.Code(gotErr))
		}
		if resp != nil {
			t.Errorf("expected nil response, got %+v", resp)
		}
	})
}

// deadlineRecordingConn observes the actual context handed to gRPC without
// waiting for the production deadlines to expire.
type deadlineRecordingConn struct {
	deadline time.Time
	method   string
}

func (c *deadlineRecordingConn) Invoke(ctx context.Context, method string, _, _ any, _ ...grpc.CallOption) error {
	c.deadline, _ = ctx.Deadline()
	c.method = method
	return nil
}

func (*deadlineRecordingConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	panic("unexpected streaming RPC")
}

func TestAgentRunnerRPCDeadlineBudgets(t *testing.T) {
	runnerType := reflect.TypeFor[AgentRunner]()
	for i := range runnerType.NumMethod() {
		method := runnerType.Method(i)
		t.Run(method.Name, func(t *testing.T) {
			budget := defaultPluginRPCTimeout
			if method.Name == "PreflightHeadlessRun" || method.Name == "StartAgentRun" {
				budget = sharedplugin.HeadlessCapabilityPreflightRPCTimeout
			}
			for _, shorterCaller := range []bool{false, true} {
				name := "plugin ceiling"
				if shorterCaller {
					name = "shorter caller"
				}
				t.Run(name, func(t *testing.T) {
					conn := &deadlineRecordingConn{}
					runner := NewAgentRunnerClient(bossanovav1.NewAgentRunnerServiceClient(conn))
					ctx := context.Background()
					var callerDeadline time.Time
					if shorterCaller {
						callerDeadline = time.Now().Add(time.Second)
						var cancel context.CancelFunc
						ctx, cancel = context.WithDeadline(ctx, callerDeadline)
						defer cancel()
					}
					args := []reflect.Value{reflect.ValueOf(ctx)}
					for j := 1; j < method.Type.NumIn(); j++ {
						args = append(args, reflect.New(method.Type.In(j).Elem()))
					}
					before := time.Now()
					results := reflect.ValueOf(runner).MethodByName(method.Name).Call(args)
					after := time.Now()
					if err := results[len(results)-1]; !err.IsNil() {
						t.Fatalf("RPC returned error: %v", err.Interface())
					}
					if shorterCaller {
						if !conn.deadline.Equal(callerDeadline) {
							t.Fatalf("deadline = %s, want caller deadline %s", conn.deadline, callerDeadline)
						}
					} else if conn.deadline.Before(before.Add(budget)) || conn.deadline.After(after.Add(budget)) {
						t.Fatalf("deadline = %s, want %s budget between %s and %s", conn.deadline, budget, before, after)
					}
				})
			}
		})
	}
}

func TestAgentRunnerGRPCClientCheckLoginDeadline(t *testing.T) {
	budget := sharedplugin.AgentLoginProbeTimeout + 5*time.Second
	for _, shorterCaller := range []bool{false, true} {
		name := "plugin ceiling"
		if shorterCaller {
			name = "shorter caller"
		}
		t.Run(name, func(t *testing.T) {
			conn := &deadlineRecordingConn{}
			runner := NewAgentRunnerClient(bossanovav1.NewAgentRunnerServiceClient(conn))
			checker, ok := runner.(LoginChecker)
			if !ok {
				t.Fatalf("NewAgentRunnerClient(%T) does not implement LoginChecker", runner)
			}

			ctx := context.Background()
			var callerDeadline time.Time
			if shorterCaller {
				callerDeadline = time.Now().Add(time.Second)
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, callerDeadline)
				defer cancel()
			}

			before := time.Now()
			if _, err := checker.CheckLogin(ctx, &bossanovav1.CheckLoginRequest{}); err != nil {
				t.Fatalf("CheckLogin returned error: %v", err)
			}
			after := time.Now()

			if conn.method != bossanovav1.AgentRunnerService_CheckLogin_FullMethodName {
				t.Errorf("method = %q, want %q", conn.method, bossanovav1.AgentRunnerService_CheckLogin_FullMethodName)
			}
			if shorterCaller {
				if !conn.deadline.Equal(callerDeadline) {
					t.Fatalf("deadline = %s, want caller deadline %s", conn.deadline, callerDeadline)
				}
			} else if conn.deadline.Before(before.Add(budget)) || conn.deadline.After(after.Add(budget)) {
				t.Fatalf("deadline = %s, want %s budget between %s and %s", conn.deadline, budget, before, after)
			}
		})
	}
}

type loginCheckerRunnerStub struct {
	*stubAgentRunner
	calls int
	resp  *bossanovav1.CheckLoginResponse
	err   error
}

func (s *loginCheckerRunnerStub) CheckLogin(_ context.Context, _ *bossanovav1.CheckLoginRequest) (*bossanovav1.CheckLoginResponse, error) {
	s.calls++
	return s.resp, s.err
}

func TestAgentRunnerProxyCheckLogin(t *testing.T) {
	ctx := context.Background()

	t.Run("forwards CheckLogin to runner implementing LoginChecker", func(t *testing.T) {
		want := &bossanovav1.CheckLoginResponse{
			Status: bossanovav1.AgentLoginStatus_AGENT_LOGIN_STATUS_LOGGED_OUT,
			Remedy: "run `codex login`",
		}
		live := &loginCheckerRunnerStub{
			stubAgentRunner: &stubAgentRunner{name: "codex"},
			resp:            want,
		}
		h := newHostForTest([]managedPlugin{
			{cfg: config.PluginConfig{Name: "codex"}, agentRunner: live},
		})

		proxy, ok := h.AgentRunners()["codex"].(LoginChecker)
		if !ok {
			t.Fatal("AgentRunners proxy does not implement LoginChecker")
		}

		got, err := proxy.CheckLogin(ctx, &bossanovav1.CheckLoginRequest{})
		if err != nil {
			t.Fatalf("CheckLogin returned unexpected error: %v", err)
		}
		if live.calls != 1 {
			t.Errorf("underlying CheckLogin calls = %d, want 1", live.calls)
		}
		if got != want {
			t.Fatalf("CheckLogin response = %+v, want %+v", got, want)
		}
	})

	t.Run("propagates error from underlying LoginChecker", func(t *testing.T) {
		wantErr := grpcstatus.Error(codes.Internal, "probe failed")
		live := &loginCheckerRunnerStub{
			stubAgentRunner: &stubAgentRunner{name: "codex"},
			err:             wantErr,
		}
		h := newHostForTest([]managedPlugin{
			{cfg: config.PluginConfig{Name: "codex"}, agentRunner: live},
		})

		proxy := h.AgentRunners()["codex"].(LoginChecker)
		got, err := proxy.CheckLogin(ctx, &bossanovav1.CheckLoginRequest{})
		if err == nil {
			t.Fatal("expected error from CheckLogin, got nil")
		}
		if grpcstatus.Code(err) != codes.Internal {
			t.Errorf("error code = %v, want codes.Internal", grpcstatus.Code(err))
		}
		if got != nil {
			t.Errorf("expected nil response on error, got %+v", got)
		}
	})

	t.Run("returns Unimplemented when runner lacks LoginChecker", func(t *testing.T) {
		h := newHostForTest([]managedPlugin{
			{cfg: config.PluginConfig{Name: "legacy"}, agentRunner: &stubAgentRunner{name: "legacy"}},
		})

		proxy := h.AgentRunners()["legacy"].(LoginChecker)
		got, err := proxy.CheckLogin(ctx, &bossanovav1.CheckLoginRequest{})
		if err == nil {
			t.Fatal("expected Unimplemented error, got nil")
		}
		if grpcstatus.Code(err) != codes.Unimplemented {
			t.Errorf("error code = %v, want codes.Unimplemented", grpcstatus.Code(err))
		}
		if got != nil {
			t.Errorf("expected nil response, got %+v", got)
		}
	})

	t.Run("returns Unavailable when no live runner is present", func(t *testing.T) {
		h := newHostForTest([]managedPlugin{
			{cfg: config.PluginConfig{Name: "claude"}, agentRunner: &stubAgentRunner{name: "claude"}},
		})
		proxy := h.AgentRunners()["claude"].(LoginChecker)

		h.mu.Lock()
		h.plugins = nil
		h.mu.Unlock()

		got, err := proxy.CheckLogin(ctx, &bossanovav1.CheckLoginRequest{})
		if err == nil {
			t.Fatal("expected Unavailable error, got nil")
		}
		if grpcstatus.Code(err) != codes.Unavailable {
			t.Errorf("error code = %v, want codes.Unavailable", grpcstatus.Code(err))
		}
		if got != nil {
			t.Errorf("expected nil response, got %+v", got)
		}
	})
}
