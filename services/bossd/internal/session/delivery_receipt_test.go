package session

import (
	"testing"
	"time"
)

func TestDeliveryReceiptBook_KeepsOnlyAwaitedReceipts(t *testing.T) {
	var b deliveryReceiptBook
	launch := time.Now()

	if b.record("agent-1", launch.Add(time.Second)) {
		t.Fatal("a receipt nobody awaits must be dropped")
	}
	release := b.expect("agent-1")
	if b.receivedSince("agent-1", launch) {
		t.Fatal("the dropped receipt must not count once a launch starts waiting")
	}
	if !b.record("agent-1", launch.Add(time.Second)) {
		t.Fatal("an awaited receipt must be recorded")
	}
	if !b.receivedSince("agent-1", launch) {
		t.Fatal("receipt after launch must confirm")
	}
	if b.receivedSince("agent-1", launch.Add(2*time.Second)) {
		t.Fatal("a receipt older than the launch must not confirm it")
	}
	release()
	release() // idempotent
	if b.receivedSince("agent-1", launch) || len(b.awaiting) != 0 {
		t.Fatal("release must drop the slot")
	}
}

func TestDeliveryReceiptBook_OverlappingExpectationsShareASlot(t *testing.T) {
	var b deliveryReceiptBook
	first := b.expect("agent-1")
	second := b.expect("agent-1")
	first()
	if !b.record("agent-1", time.Now()) {
		t.Fatal("the second expectation must keep the slot open after the first releases")
	}
	second()
	if b.record("agent-1", time.Now()) {
		t.Fatal("slot must close after the last release")
	}
}

func TestLifecycle_RecordPromptSubmitted(t *testing.T) {
	l := &Lifecycle{}
	if l.RecordPromptSubmitted("", time.Now()) {
		t.Fatal("empty id must be refused")
	}
	release := l.deliveryReceipts.expect("agent-1")
	defer release()
	if !l.RecordPromptSubmitted("agent-1", time.Now()) {
		t.Fatal("awaited id must be recorded")
	}
}

func TestLifecycle_EstablishedSendReceipt(t *testing.T) {
	l := &Lifecycle{}
	sent := time.Now()

	l.ExpectPromptSubmitted("")() // an empty key opens nothing and releases cleanly
	if l.PromptSubmittedSince("", sent) {
		t.Fatal("an empty key must never confirm")
	}

	release := l.ExpectPromptSubmitted("agent-1")
	if l.PromptSubmittedSince("agent-1", sent) {
		t.Fatal("no receipt has arrived yet")
	}
	if !l.RecordPromptSubmitted("agent-1", sent.Add(time.Millisecond)) {
		t.Fatal("the hook's receipt must be recorded while the send waits")
	}
	if !l.PromptSubmittedSince("agent-1", sent) {
		t.Fatal("a receipt after the send must confirm it")
	}
	release()
	if l.RecordPromptSubmitted("agent-1", time.Now()) {
		t.Fatal("the slot must close once the send releases it")
	}
}

func TestLifecycle_PromptReceiptToken(t *testing.T) {
	const sockTok = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	unkeyed := &Lifecycle{}
	if tok := unkeyed.PromptReceiptToken("agent-1"); tok != "" {
		t.Fatal("without a key no token may be minted")
	}
	if unkeyed.ValidPromptReceiptToken("agent-1", "") {
		t.Fatal("without a key every token must be refused, including the empty one")
	}

	l := &Lifecycle{}
	l.SetPromptReceiptKey(sockTok)
	tok := l.PromptReceiptToken("agent-1")
	if len(tok) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(tok))
	}
	if tok == sockTok {
		t.Fatal("the receipt token must not be the socket token")
	}
	if !l.ValidPromptReceiptToken("agent-1", tok) {
		t.Fatal("the derived token must validate for its own chat")
	}
	if l.ValidPromptReceiptToken("agent-2", tok) {
		t.Fatal("a token must not validate for a different chat")
	}
	for _, bad := range []string{"", "wrong", tok[:63], tok + "0"} {
		if l.ValidPromptReceiptToken("agent-1", bad) {
			t.Fatalf("token %q must be refused", bad)
		}
	}
	if l.PromptReceiptToken("") != "" || l.ValidPromptReceiptToken("", tok) {
		t.Fatal("an empty chat id must never mint or validate")
	}

	// Derived from the persistent socket token, so a restarted daemon (a new
	// Lifecycle with the same key) still accepts a pane launched before it.
	restarted := &Lifecycle{}
	restarted.SetPromptReceiptKey(sockTok)
	if !restarted.ValidPromptReceiptToken("agent-1", tok) {
		t.Fatal("the token must survive a daemon restart with the same socket token")
	}
	rotated := &Lifecycle{}
	rotated.SetPromptReceiptKey("f" + sockTok[1:])
	if rotated.ValidPromptReceiptToken("agent-1", tok) {
		t.Fatal("a rotated socket token must invalidate earlier receipt tokens")
	}
}
