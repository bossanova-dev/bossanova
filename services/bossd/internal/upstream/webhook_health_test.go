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

// missFixture is a tracker whose stream opened at openedAt and stays open,
// with one delivery from another repo so the pipe is proven.
func missFixture(t *testing.T) (h *WebhookHealth, openedAt time.Time, now *time.Time, open *bool) {
	t.Helper()
	openedAt = time.Unix(1_800_000_000, 0)
	clock := openedAt.Add(time.Hour)
	isOpen := true
	h = NewWebhookHealth(func() (time.Time, bool) { return openedAt, isOpen })
	h.now = func() time.Time { return clock }
	h.RecordDelivery("https://github.com/acme/has-app.git")
	return h, openedAt, &clock, &isOpen
}

const noApp = "git@github.com:acme/no-app.git"

func TestWebhooksMissingNeedsAPolledChange(t *testing.T) {
	h, _, now, _ := missFixture(t)
	*now = now.Add(24 * time.Hour)
	if h.WebhooksMissing(noApp) {
		t.Fatal("flagged a repo no poll ever saw change, such as one whose open PR is just quiet")
	}
}

func TestWebhooksMissingAfterAnUnannouncedChange(t *testing.T) {
	h, openedAt, now, open := missFixture(t)
	h.NotePolledChange(noApp, openedAt.Add(polledReadStaleness))

	*now = now.Add(missedDeliveryGrace - time.Second)
	if h.WebhooksMissing(noApp) {
		t.Fatal("flagged before a slow delivery had time to arrive")
	}

	*now = now.Add(time.Second)
	// Bosso keys deliveries by the https origin; the local origin is SSH.
	for _, spelling := range []string{noApp, "https://github.com/ACME/no-app"} {
		if !h.WebhooksMissing(spelling) {
			t.Fatalf("%q not flagged after an unannounced change outlived the grace", spelling)
		}
	}
	if h.WebhooksMissing("https://github.com/acme/has-app.git") {
		t.Fatal("flagged a repo that delivers")
	}

	*open = false
	if h.WebhooksMissing(noApp) {
		t.Fatal("flagged while the stream is closed")
	}
	*open = true

	h.RecordDelivery("https://github.com/acme/no-app.git")
	if h.WebhooksMissing(noApp) {
		t.Fatal("still flagged after a webhook arrived for the repo")
	}
	h.NotePolledChange(noApp, openedAt.Add(time.Hour))
	*now = now.Add(missedDeliveryGrace)
	if h.WebhooksMissing(noApp) {
		t.Fatal("a repo that has delivered was flagged by a later polled change")
	}
}

func TestWebhooksMissingIgnoresChangesAcrossAReconnect(t *testing.T) {
	h, openedAt, now, _ := missFixture(t)
	// The earlier read predates the current stream (plus the read cache's
	// staleness), so the change may have happened while webhooks could not be
	// delivered.
	h.NotePolledChange(noApp, openedAt.Add(polledReadStaleness-time.Second))
	*now = now.Add(missedDeliveryGrace)
	if h.WebhooksMissing(noApp) {
		t.Fatal("flagged a change that may have happened before the stream opened")
	}
}

func TestWebhooksMissingForgetsAMissNotedBeforeAReconnect(t *testing.T) {
	openedAt := time.Unix(1_800_000_000, 0)
	now := openedAt.Add(time.Hour)
	h := NewWebhookHealth(func() (time.Time, bool) { return openedAt, true })
	h.now = func() time.Time { return now }
	h.RecordDelivery("https://github.com/acme/has-app.git")
	h.NotePolledChange(noApp, openedAt.Add(polledReadStaleness))

	// The stream drops before the webhook announcing the change lands, so that
	// delivery may never reach this daemon.
	now = now.Add(time.Minute)
	openedAt = now
	now = now.Add(missedDeliveryGrace)
	if h.WebhooksMissing(noApp) {
		t.Fatal("flagged a change whose delivery may have been lost to a reconnect")
	}
}

func TestWebhooksMissingFlagsAFreshMissAfterAReconnect(t *testing.T) {
	openedAt := time.Unix(1_800_000_000, 0)
	now := openedAt.Add(time.Hour)
	h := NewWebhookHealth(func() (time.Time, bool) { return openedAt, true })
	h.now = func() time.Time { return now }
	h.RecordDelivery("https://github.com/acme/has-app.git")
	h.NotePolledChange(noApp, openedAt.Add(polledReadStaleness))

	now = now.Add(time.Minute)
	openedAt = now
	now = now.Add(time.Hour)
	h.NotePolledChange(noApp, openedAt.Add(polledReadStaleness))
	now = now.Add(missedDeliveryGrace)
	if !h.WebhooksMissing(noApp) {
		t.Fatal("a miss noted before a reconnect hid a fresh miss on the new stream")
	}
}

func TestWebhooksMissingNeedsAnyDelivery(t *testing.T) {
	openedAt := time.Unix(1_800_000_000, 0)
	now := openedAt.Add(time.Hour)
	h := NewWebhookHealth(func() (time.Time, bool) { return openedAt, true })
	h.now = func() time.Time { return now }
	h.NotePolledChange(noApp, openedAt.Add(polledReadStaleness))
	now = now.Add(missedDeliveryGrace)
	if h.WebhooksMissing(noApp) {
		t.Fatal("flagged although no repo has ever delivered, so the pipe itself is unproven")
	}
}

func TestWebhooksMissingNeverFlagsAnUnnormalisableOrigin(t *testing.T) {
	h, openedAt, now, _ := missFixture(t)
	for _, origin := range []string{"git@github-work:acme/no-app.git", "git@github.com-work:acme/no-app.git"} {
		h.NotePolledChange(origin, openedAt.Add(polledReadStaleness))
		*now = now.Add(missedDeliveryGrace)
		if h.WebhooksMissing(origin) {
			t.Fatalf("flagged %q, whose key can never match a delivery", origin)
		}
	}

	var nilHealth *WebhookHealth
	nilHealth.NotePolledChange(noApp, openedAt)
	if nilHealth.WebhooksMissing(noApp) {
		t.Fatal("a nil tracker reported missing")
	}
}
