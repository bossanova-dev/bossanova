package cloudrpc

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
)

func TestRemoteClientCheckAgentLoginsIsLocalOnly(t *testing.T) {
	t.Parallel()

	got, err := (&RemoteClient{}).CheckAgentLogins(context.Background())
	if got != nil {
		t.Fatalf("CheckAgentLogins = %+v, want nil", got)
	}
	if connect.CodeOf(err) != connect.CodeUnimplemented || !strings.Contains(err.Error(), "CheckAgentLogins is only available on a local daemon") {
		t.Fatalf("err = %v, want Unimplemented local-only error", err)
	}
}
