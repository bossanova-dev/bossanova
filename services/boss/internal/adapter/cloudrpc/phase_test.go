package cloudrpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
)

func TestRemoteSetChatPhaseLocalOnly(t *testing.T) {
	err := (&RemoteClient{}).SetChatPhase(context.Background(), "session", "chat", "reviewing")
	if connect.CodeOf(err) != connect.CodeUnimplemented || err.Error() != errLocalOnly("set chat phase").Error() {
		t.Fatalf("error=%v, want local-only unimplemented", err)
	}
}
