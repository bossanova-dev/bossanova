// Package safego provides panic-recovering goroutine launchers.
package safego

import (
	"runtime/debug"
	"sync/atomic"

	"github.com/rs/zerolog"
)

var recoverHook atomic.Pointer[func(any, []byte)]

// RegisterRecoverHook registers fn to run after Go recovers a panic.
// Passing nil clears the hook.
func RegisterRecoverHook(fn func(any, []byte)) {
	if fn == nil {
		recoverHook.Store(nil)
		return
	}
	recoverHook.Store(&fn)
}

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
				stack := debug.Stack()
				logger.Error().
					Interface("panic", r).
					Str("stack", string(stack)).
					Msg("recovered from panic in goroutine")
				if hook := recoverHook.Load(); hook != nil {
					func() {
						defer func() {
							if hookPanic := recover(); hookPanic != nil {
								logger.Error().
									Interface("panic", hookPanic).
									Str("stack", string(debug.Stack())).
									Msg("recovered from panic in safego recover hook")
							}
						}()
						(*hook)(r, stack)
					}()
				}
			}
		}()
		fn()
	}()
	return done
}
