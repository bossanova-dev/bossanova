package cloudrpc

import (
	"testing"

	"connectrpc.com/connect"
)

func TestRemoteWatchSessionSetupLocalOnly(t *testing.T) {
	stream, err := (&RemoteClient{}).WatchSessionSetup(t.Context(), "session")
	if stream != nil || connect.CodeOf(err) != connect.CodeUnimplemented || err.Error() != errLocalOnly("WatchSessionSetup").Error() {
		t.Fatalf("stream = %v, error = %v", stream, err)
	}
}
