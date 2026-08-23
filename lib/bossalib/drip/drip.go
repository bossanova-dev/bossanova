// Package drip provides a small abstraction for enrolling contacts in lifecycle
// email sequences. Implementations use the standard library only.
package drip

import "context"

// Drip sends a lifecycle event for a contact. Send implementations must be
// safe for concurrent use.
type Drip interface {
	Send(ctx context.Context, event Event) error
}

// Event identifies a contact and the lifecycle event to send. ContactProperties
// are encoded as top-level Loops contact properties; EventProperties belong only
// to the emitted event.
type Event struct {
	Email             string
	UserID            string
	EventName         string
	IdempotencyKey    string
	ContactProperties map[string]any
	MailingLists      map[string]bool
	EventProperties   map[string]any
}
