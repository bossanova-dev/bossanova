package daemonrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/recurser/boss/internal/domain/rpc"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/domain/rpcerr"
)

func TestLocalClientClassifiesReturnedTransportError(t *testing.T) {
	original := connect.NewError(connect.CodeNotFound, errors.New("session gone"))
	c := &LocalClient{rpc: &fakeDaemonRPC{err: original}}
	_, err := c.GetSession(context.Background(), "gone", rpc.SessionReadOptions{})
	if rpcerr.KindOf(err) != rpcerr.NotFound || connect.CodeOf(err) != connect.CodeNotFound || rpcerr.Message(err) != "session gone" {
		t.Fatalf("unclassified client error: %v", err)
	}
}
