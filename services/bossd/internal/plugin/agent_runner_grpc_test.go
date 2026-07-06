package plugin

import (
	"testing"

	bossanovav1 "github.com/recurser/bossalib/gen/bossanova/v1"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestAgentRunner_StartRun_RoutesToCorrectMethod(t *testing.T) {
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
