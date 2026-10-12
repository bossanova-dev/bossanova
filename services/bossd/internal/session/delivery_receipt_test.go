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
