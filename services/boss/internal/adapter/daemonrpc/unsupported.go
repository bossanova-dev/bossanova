package daemonrpc

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
)

// errLocalOnly is returned for operations that only work on a local daemon.
func errLocalOnly(op string) error {
	return rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available on a local daemon", op)))
}

// errCloudOnly is the mirror of errLocalOnly for operations a local daemon can
// never serve because the data lives in bosso: organizations and the
// repo-to-organization mapping. It is deliberately not errLocalOnly — that
// helper's message says the operation "is only available on a local daemon",
// which is exactly backwards when a LocalClient is the one refusing, and would
// tell a signed-out user to do the thing they are already doing.
func errCloudOnly(op string) error {
	return rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available when signed in to Bossanova Cloud", op)))
}
