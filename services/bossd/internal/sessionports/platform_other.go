//go:build !darwin && !linux

package sessionports

import (
	"context"
	"errors"
)

// errUnsupportedPlatform is returned by the fallback adapters on platforms
// without a process/socket implementation. It keeps the package building
// everywhere (CI cross-compiles) while making the lack of evidence explicit:
// callers see transient failures that degrade to incomplete, never a panic.
var errUnsupportedPlatform = errors.New("sessionports: unsupported platform")

// readProcessIdentity has no implementation off macOS/Linux; report transient
// unavailability so sessions degrade to incomplete rather than crashing.
func readProcessIdentity(_ context.Context, _ int) (Identity, error) {
	return Identity{}, errUnsupportedPlatform
}

// newPlatformSocketSource returns a SocketSource that reports every batch as
// globally incomplete on unsupported platforms.
func newPlatformSocketSource() SocketSource {
	return unsupportedSocketSource{}
}

type unsupportedSocketSource struct{}

func (unsupportedSocketSource) Listeners(_ context.Context, _ []int) (SocketScan, error) {
	return SocketScan{GlobalIncomplete: true}, nil
}
