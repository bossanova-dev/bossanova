// Package safego provides panic-recovering goroutine launchers.
package safego

import (
	"runtime/debug"

	"github.com/rs/zerolog"
)

// Go launches fn in a new goroutine with panic recovery.
// If fn panics, the panic is logged and the goroutine returns
// instead of crashing the process.
func Go(logger zerolog.Logger, fn func()) {
	go func() {
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
}
