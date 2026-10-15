package server

import (
	"context"
	"slices"

	"connectrpc.com/connect"
	"github.com/recurser/bossalib/daemoncaps"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

var daemonCapabilities = []string{daemoncaps.MergeExpectedHeadSHA}

// GetDaemonCapabilities advertises optional request features this bossd honours.
// This is a local daemon RPC, outside the versioned orchestrator surface.
func (s *Server) GetDaemonCapabilities(context.Context, *connect.Request[pb.GetDaemonCapabilitiesRequest]) (*connect.Response[pb.GetDaemonCapabilitiesResponse], error) {
	return connect.NewResponse(&pb.GetDaemonCapabilitiesResponse{Capabilities: slices.Clone(daemonCapabilities)}), nil
}
