package upstream

import (
	"testing"
	"time"
)

func TestWebhookHealth(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	openedAt := now.Add(-time.Hour)
	open := true
	h := NewWebhookHealth(func() (time.Time, bool) { return openedAt, open })
	h.now = func() time.Time { return now }

	const repo = "https://github.com/Acme/Widgets.git"
	if h.WebhookDeliveryHealthy(repo) {
		t.Fatal("healthy before any delivery")
	}

	h.RecordDelivery(repo)
	// Every spelling of the same repo shares the evidence.
	for _, spelling := range []string{repo, "acme/widgets", "git@github.com:acme/widgets.git"} {
		if !h.WebhookDeliveryHealthy(spelling) {
			t.Fatalf("%q not healthy after a delivery", spelling)
		}
	}
	if h.WebhookDeliveryHealthy("acme/other") {
		t.Fatal("a delivery for one repo vouched for another")
	}

	// A closed stream delivers nothing.
	open = false
	if h.WebhookDeliveryHealthy(repo) {
		t.Fatal("healthy while the stream is closed")
	}

	// A reconnect must re-earn trust: the old delivery predates the new stream.
	open = true
	openedAt = now.Add(time.Second)
	now = now.Add(2 * time.Second)
	if h.WebhookDeliveryHealthy(repo) {
		t.Fatal("a delivery from the previous stream vouched for the current one")
	}
	h.RecordDelivery(repo)
	if !h.WebhookDeliveryHealthy(repo) {
		t.Fatal("not healthy after a delivery on the current stream")
	}

	// Evidence lapses.
	now = now.Add(webhookDeliveryWindow)
	if h.WebhookDeliveryHealthy(repo) {
		t.Fatal("healthy after the delivery window lapsed")
	}

	var nilHealth *WebhookHealth
	nilHealth.RecordDelivery(repo)
	if nilHealth.WebhookDeliveryHealthy(repo) {
		t.Fatal("a nil tracker reported healthy")
	}
}
