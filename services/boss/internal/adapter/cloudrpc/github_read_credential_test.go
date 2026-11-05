package cloudrpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/recurser/bossalib/gen/bossanova/v1"
)

func TestRemoteGitHubReadCredentialLocalOnly(t *testing.T) {
	response, err := (&RemoteClient{}).GetGitHubReadCredential(context.Background(), &pb.GetGitHubReadCredentialRequest{GhArgs: []string{"pr", "view", "1"}})
	if response != nil || connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("response=%v error=%v", response, err)
	}
}
