package client

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// wantBroadcastLocalOnlySubstring is the actionable half of every broadcast
// local-only error: asserted once here rather than repeated per table case.
const wantBroadcastLocalOnlySubstring = "not yet routed through the orchestrator"

// TestRemoteClient_BroadcastRPCsAreLocalOnly pins the BOS-560 contract that
// none of the six broadcast RPCs have an orchestrator proxy yet: RemoteClient
// must refuse every one with CodeUnimplemented rather than silently no-op or
// panic reaching for a proxy method that does not exist.
func TestRemoteClient_BroadcastRPCsAreLocalOnly(t *testing.T) {
	t.Parallel()
	c := &RemoteClient{}
	ctx := context.Background()

	cases := []struct {
		name string
		call func() error
	}{
		{
			name: "SendBroadcast",
			call: func() error {
				_, err := c.SendBroadcast(ctx, &pb.SendBroadcastRequest{})
				return err
			},
		},
		{
			name: "ListBroadcasts",
			call: func() error {
				_, err := c.ListBroadcasts(ctx, &pb.ListBroadcastsRequest{})
				return err
			},
		},
		{
			name: "DeleteBroadcast",
			call: func() error {
				return c.DeleteBroadcast(ctx, "bc-1")
			},
		},
		{
			name: "CreateBroadcastSubscription",
			call: func() error {
				_, err := c.CreateBroadcastSubscription(ctx, &pb.CreateBroadcastSubscriptionRequest{})
				return err
			},
		},
		{
			name: "ListBroadcastSubscriptions",
			call: func() error {
				_, err := c.ListBroadcastSubscriptions(ctx, &pb.ListBroadcastSubscriptionsRequest{})
				return err
			},
		},
		{
			name: "DeleteBroadcastSubscription",
			call: func() error {
				return c.DeleteBroadcastSubscription(ctx, "sub-1")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.call()
			if err == nil {
				t.Fatalf("%s: expected an error, got nil", tc.name)
			}
			if connect.CodeOf(err) != connect.CodeUnimplemented {
				t.Fatalf("%s: CodeOf(err) = %v, want CodeUnimplemented", tc.name, connect.CodeOf(err))
			}
			if !strings.Contains(err.Error(), tc.name) {
				t.Fatalf("%s: error %q does not name the failing method", tc.name, err.Error())
			}
			if !strings.Contains(err.Error(), wantBroadcastLocalOnlySubstring) {
				t.Fatalf("%s: error %q does not contain %q", tc.name, err.Error(), wantBroadcastLocalOnlySubstring)
			}
		})
	}
}
