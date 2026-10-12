package session

import (
	"sync"
	"time"
)

// deliveryReceiptBook records "the agent submitted a prompt" receipts for the
// chats bossd is currently confirming. A receipt is kept only while a launch
// expects one for that id, so a POST for any other id (a finished chat, a
// sibling chat's prompt, a forged request) is dropped and the book never grows
// past the number of in-flight confirmations. The zero value is ready to use.
type deliveryReceiptBook struct {
	mu       sync.Mutex
	awaiting map[string]*receiptSlot
}

type receiptSlot struct {
	// expectations counts overlapping expect calls for one id, so one
	// launch's release cannot drop a slot another still waits on.
	expectations int
	// lastAt is the arrival time of the newest receipt, zero when none.
	lastAt time.Time
}

// expect opens a slot for key and returns its release. It must be called before
// the agent can submit, i.e. before the pane is launched, or a fast receipt
// arrives while nobody is waiting and is dropped.
func (b *deliveryReceiptBook) expect(key string) (release func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.awaiting == nil {
		b.awaiting = make(map[string]*receiptSlot)
	}
	slot := b.awaiting[key]
	if slot == nil {
		slot = &receiptSlot{}
		b.awaiting[key] = slot
	}
	slot.expectations++
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if cur := b.awaiting[key]; cur == slot {
				slot.expectations--
				if slot.expectations <= 0 {
					delete(b.awaiting, key)
				}
			}
		})
	}
}

// record stores a receipt for key that arrived at at, reporting whether anyone
// was waiting for it.
func (b *deliveryReceiptBook) record(key string, at time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	slot := b.awaiting[key]
	if slot == nil {
		return false
	}
	if at.After(slot.lastAt) {
		slot.lastAt = at
	}
	return true
}

// receivedSince reports whether a receipt for key arrived at or after since.
func (b *deliveryReceiptBook) receivedSince(key string, since time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	slot := b.awaiting[key]
	return slot != nil && !slot.lastAt.IsZero() && !slot.lastAt.Before(since)
}

// RecordPromptSubmitted records that the agent for agentSessionID submitted a
// prompt at at. The hook server calls it for each
// /hooks/prompt-submitted/{agent_session_id} POST. It reports whether a launch
// was waiting on that id; a receipt nobody awaits is dropped.
func (l *Lifecycle) RecordPromptSubmitted(agentSessionID string, at time.Time) bool {
	if agentSessionID == "" {
		return false
	}
	return l.deliveryReceipts.record(agentSessionID, at)
}

// ExpectPromptSubmitted opens a receipt slot for a send into an established
// chat, keyed like the launch slot (the session id the chat's runner was
// launched with), and returns its release. Call it before the payload can be
// submitted: a receipt that arrives while no slot is open is dropped. It is
// the established-send counterpart of the slot a launch opens (BOS-1506).
func (l *Lifecycle) ExpectPromptSubmitted(key string) (release func()) {
	if key == "" {
		return func() {}
	}
	return l.deliveryReceipts.expect(key)
}

// PromptSubmittedSince reports whether a prompt-submitted receipt for key
// arrived at or after since, while a slot for key was open.
func (l *Lifecycle) PromptSubmittedSince(key string, since time.Time) bool {
	if key == "" {
		return false
	}
	return l.deliveryReceipts.receivedSince(key, since)
}
