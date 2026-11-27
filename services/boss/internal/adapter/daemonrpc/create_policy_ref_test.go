package daemonrpc

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"github.com/recurser/bossalib/kit/daemoncaps"
)

// errCreateReached marks a CreateSession call that reached the daemon.
var errCreateReached = errors.New("create reached the daemon")

// capabilityRPC is a daemon client whose capability probe answers with
// capabilities or probeErr, and which counts CreateSession calls.
type capabilityRPC struct {
	bossanovav1connect.DaemonServiceClient
	capabilities []string
	probeErr     error
	probes       int
	creates      int
}

func (f *capabilityRPC) GetDaemonCapabilities(context.Context, *connect.Request[pb.GetDaemonCapabilitiesRequest]) (*connect.Response[pb.GetDaemonCapabilitiesResponse], error) {
	f.probes++
	if f.probeErr != nil {
		return nil, f.probeErr
	}
	return connect.NewResponse(&pb.GetDaemonCapabilitiesResponse{Capabilities: f.capabilities}), nil
}

func (f *capabilityRPC) CreateSession(context.Context, *connect.Request[pb.CreateSessionRequest]) (*connect.ServerStreamForClient[pb.CreateSessionResponse], error) {
	f.creates++
	return nil, errCreateReached
}

func TestLocalClient_CreateSessionRefusesPolicyRefTheDaemonCannotCarry(t *testing.T) {
	trial := &pb.CreateSessionRequest{IsVerifyOnly: true, PolicyRef: "config/trial"}
	for _, tc := range []struct {
		name string
		rpc  *capabilityRPC
	}{
		{"legacy", &capabilityRPC{probeErr: connect.NewError(connect.CodeUnimplemented, errors.New("unknown method"))}},
		{"missing", &capabilityRPC{capabilities: []string{daemoncaps.MergeExpectedHeadSHA}}},
		{"unavailable", &capabilityRPC{probeErr: connect.NewError(connect.CodeUnavailable, errors.New("offline"))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &LocalClient{rpc: tc.rpc}
			if _, err := c.CreateSession(context.Background(), trial); err == nil || errors.Is(err, errCreateReached) {
				t.Fatalf("CreateSession err = %v, want a refusal before create", err)
			}
			if tc.rpc.creates != 0 {
				t.Fatalf("session created %d times on a daemon that cannot carry policy_ref", tc.rpc.creates)
			}
		})
	}

	capable := &capabilityRPC{capabilities: []string{daemoncaps.CreateSessionPolicyRef}}
	if _, err := (&LocalClient{rpc: capable}).CreateSession(context.Background(), trial); !errors.Is(err, errCreateReached) {
		t.Fatalf("capable daemon: err = %v, want the create to reach the daemon", err)
	}

	legacy := &capabilityRPC{probeErr: connect.NewError(connect.CodeUnimplemented, errors.New("unknown method"))}
	if _, err := (&LocalClient{rpc: legacy}).CreateSession(context.Background(), &pb.CreateSessionRequest{}); !errors.Is(err, errCreateReached) {
		t.Fatalf("ordinary create: err = %v, want the create to reach the daemon", err)
	}
	if legacy.probes != 0 {
		t.Fatalf("ordinary create probed capabilities %d times", legacy.probes)
	}
}
