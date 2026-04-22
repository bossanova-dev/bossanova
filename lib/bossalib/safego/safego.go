// Package safego provides panic-recovering goroutine launchers.
package safego

import (
	"runtime/debug"

	"github.com/rs/zerolog"
)

// Go launches fn in a new goroutine with panic recovery.
// If fn panics, the panic is logged and the goroutine returns
// instead of crashing the process. The returned channel is closed
// once the goroutine has exited — including after panic recovery
// and its log write — so callers (typically tests) can synchronize
// on completion without racing the recover path.
func Go(logger zerolog.Logger, fn func()) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				logger.Error().
					Interface("panic", r).
					Str("stack", string(debug.Stack())).
					Msg("recovered from panic in goroutine")
			}
		}()
		fn()
	}()
	return done
}
