package cloudrpc

import (
	"fmt"

	"connectrpc.com/connect"
	"github.com/recurser/boss/internal/adapter/rpcerror"
)

// errLocalOnly is returned for operations that only work on a local daemon.
func errLocalOnly(op string) error {
	return rpcerror.WrapError(connect.NewError(connect.CodeUnimplemented, fmt.Errorf("%s is only available on a local daemon", op)))
}
