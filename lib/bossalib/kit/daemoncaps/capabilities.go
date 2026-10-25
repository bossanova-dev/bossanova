// Package daemoncaps guards optional daemon features across client/daemon version skew.
package daemoncaps

import (
	"context"
	"fmt"
	"slices"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

// MergeExpectedHeadSHA identifies support for enforcing a merge head pin.
const MergeExpectedHeadSHA = "merge_session.expected_head_sha"

// Prober is the capability RPC implemented by a daemon client.
type Prober interface {
	GetDaemonCapabilities(context.Context, *connect.Request[pb.GetDaemonCapabilitiesRequest]) (*connect.Response[pb.GetDaemonCapabilitiesResponse], error)
}

// Require refuses a feature unless the running daemon explicitly advertises it.
func Require(ctx context.Context, probe Prober, capability string) error {
	resp, err := probe.GetDaemonCapabilities(ctx, connect.NewRequest(&pb.GetDaemonCapabilitiesRequest{}))
	if err != nil && connect.CodeOf(err) != connect.CodeUnimplemented {
		return fmt.Errorf("probe bossd capabilities; merge was not attempted: %w", err)
	}
	if err == nil && slices.Contains(resp.Msg.GetCapabilities(), capability) {
		return nil
	}
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("running bossd does not support the merge head pin (%s); merge was not attempted; restart or upgrade bossd to match this client", capability))
}
