package drip

import (
	"context"
	"log"
)

// NoopDrip satisfies Drip by logging and discarding lifecycle events. It is
// used when no drip provider is configured.
type NoopDrip struct{}

// NewNoopDrip returns a Drip that does not send events.
func NewNoopDrip() *NoopDrip {
	return &NoopDrip{}
}

// Send logs and discards the event.
func (*NoopDrip) Send(_ context.Context, event Event) error {
	log.Printf("drip: noop — event %q for %q not sent (no provider configured)", event.EventName, event.Email)
	return nil
}
