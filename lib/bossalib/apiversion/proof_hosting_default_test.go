package apiversion

import (
	"testing"

	pb "github.com/recurser/bossalib/gen/bossanova/v1"
	"github.com/recurser/bossalib/gen/bossanova/v1/bossanovav1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// TestProofHostingDefault_NoTransformAtAnyVersion pins V20261108's
// on-by-default proof hosting (BOS-2783): every supported version, Baseline
// included, reads the current is_enabled, is_available and disabled_* values
// and the accepted StartProofUpload response unchanged.
//
// StartProofUpload (ENTITLEMENT_PROOF_HOSTING) admits any repository-resolved
// organization whose owner has not turned hosting off, with or without cloud
// access, and SetOrganizationProofHosting needs no cloud access either way.
// No transform or gate is registered: each change turns a refusal into an
// acceptance (the daemon, StartProofUpload's only caller, treats acceptance as
// success), and down-converting is_enabled or is_available for an older pin
// would misreport what the upload path now does. The co-deployed web settings
// page reads is_enabled and is_available, which keep their wire shape.
func TestProofHostingDefault_NoTransformAtAnyVersion(t *testing.T) {
	if !DefaultRegistry().IsSupported(V20261108) {
		t.Fatalf("V20261108 is not registered")
	}
	off := &pb.OrganizationProofHosting{IsEnabled: false, IsAvailable: true, DisabledAt: timestamppb.Now(), DisabledByUserId: "user-owner"}
	on := &pb.OrganizationProofHosting{IsEnabled: true, IsAvailable: true}
	cases := []struct {
		name      string
		procedure string
		build     func() proto.Message
	}{
		{"get on", bossanovav1connect.OrchestratorServiceGetOrganizationProofHostingProcedure, func() proto.Message {
			return &pb.GetOrganizationProofHostingResponse{ProofHosting: proto.CloneOf(on)}
		}},
		{"get off", bossanovav1connect.OrchestratorServiceGetOrganizationProofHostingProcedure, func() proto.Message {
			return &pb.GetOrganizationProofHostingResponse{ProofHosting: proto.CloneOf(off)}
		}},
		{"set off", bossanovav1connect.OrchestratorServiceSetOrganizationProofHostingProcedure, func() proto.Message {
			return &pb.SetOrganizationProofHostingResponse{ProofHosting: proto.CloneOf(off)}
		}},
		{"start upload", bossanovav1connect.OrchestratorServiceStartProofUploadProcedure, func() proto.Message {
			return &pb.StartProofUploadResponse{UploadId: "upload", OrgLabel: "org-label", PageToken: "0f0e0d0c0b0a09080706050403020100", Limits: &pb.ProofUploadLimits{MaxFileCount: 10}}
		}},
	}
	for _, tc := range cases {
		for _, version := range DefaultRegistry().All() {
			t.Run(tc.name+"/"+string(version), func(t *testing.T) {
				msg, want := tc.build(), tc.build()
				ProductionChanges().Apply(tc.procedure, msg, version)
				if !proto.Equal(msg, want) {
					t.Fatalf("response at %s = %v, want unchanged %v", version, msg, want)
				}
			})
		}
	}
}
