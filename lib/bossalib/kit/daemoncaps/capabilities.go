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

// CreateSessionPolicyRef identifies support for a trial verify policy ref on
// create. A daemon without it drops the ref and creates a verify-only session
// that judges under the base policy and may merge.
const CreateSessionPolicyRef = "create_session.policy_ref"

// requiredFeatures names each capability's feature and the call a refusal skips.
var requiredFeatures = map[string][2]string{
	MergeExpectedHeadSHA:   {"the merge head pin", "merge"},
	CreateSessionPolicyRef: {"trial verify policy refs", "session create"},
}

// Prober is the capability RPC implemented by a daemon client.
type Prober interface {
	GetDaemonCapabilities(context.Context, *connect.Request[pb.GetDaemonCapabilitiesRequest]) (*connect.Response[pb.GetDaemonCapabilitiesResponse], error)
}

// Require refuses a feature unless the running daemon explicitly advertises it.
func Require(ctx context.Context, probe Prober, capability string) error {
	feature := requiredFeatures[capability]
	resp, err := probe.GetDaemonCapabilities(ctx, connect.NewRequest(&pb.GetDaemonCapabilitiesRequest{}))
	if err != nil && connect.CodeOf(err) != connect.CodeUnimplemented {
		return fmt.Errorf("probe bossd capabilities; %s was not attempted: %w", feature[1], err)
	}
	if err == nil && slices.Contains(resp.Msg.GetCapabilities(), capability) {
		return nil
	}
	return connect.NewError(connect.CodeUnimplemented, fmt.Errorf("running bossd does not support %s (%s); %s was not attempted; restart or upgrade bossd to match this client", feature[0], capability, feature[1]))
}
