package cloudrpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
)

// verifyRPC is a daemon client that records the verify verb requests.
func TestRemoteClient_VerifyVerbsAreLocalOnly(t *testing.T) {
	t.Parallel()
	c, _ := newTestRemote(t)
	if _, err := c.ApproveSessionPR(context.Background(), "s1", false); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("ApproveSessionPR err = %v, want Unimplemented", err)
	}
	if _, err := c.OperatorMergeSession(context.Background(), "s1", "h", false, nil); connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("OperatorMergeSession err = %v, want Unimplemented", err)
	}
}
