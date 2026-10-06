package upstream

import (
	"strings"
	"sync"
	"time"

	"github.com/recurser/bossalib/vcs"
)

// webhookDeliveryWindow is how long one delivered webhook vouches for a repo.
// A repo with live PRs sees check_run / check_suite events on every push, so
// this only lapses for a repo whose PRs have gone quiet — and a quiet repo
// costs little to poll at the normal interval again.
const webhookDeliveryWindow = 6 * time.Hour

// WebhookHealth answers "are GitHub webhooks reaching this daemon for this
// repo right now?", so GitHub pollers can drop to a slow safety-net interval
// for repos where webhooks already deliver every change.
//
// A repo counts as healthy only while the bosso stream is open AND a webhook
// for that repo arrived after the stream opened. Requiring a delivery on the
// CURRENT stream means a reconnect re-earns trust per repo: webhooks sent while
// the daemon was disconnected were never delivered, so the pollers must run at
// full speed until the first post-reconnect delivery proves the pipe.
type WebhookHealth struct {
	// link reports when the current upstream stream opened, and whether it is
	// open at all. Nil means never connected.
	link   func() (openedAt time.Time, open bool)
	now    func() time.Time
	window time.Duration

	mu       sync.Mutex
	lastSeen map[string]time.Time
}

// NewWebhookHealth builds a tracker over link, typically
// StreamClient.StreamOpenSince.
func NewWebhookHealth(link func() (time.Time, bool)) *WebhookHealth {
	return &WebhookHealth{
		link:     link,
		now:      time.Now,
		window:   webhookDeliveryWindow,
		lastSeen: make(map[string]time.Time),
	}
}

// webhookRepoKey normalises the repo spellings callers hold — an origin URL,
// an "owner/name", any case — onto one key.
func webhookRepoKey(repo string) string {
	if nwo := vcs.GitHubNWO(repo); nwo != "" {
		return strings.ToLower(nwo)
	}
	return strings.ToLower(strings.TrimSpace(repo))
}

// RecordDelivery notes that a webhook for repo just arrived.
func (h *WebhookHealth) RecordDelivery(repo string) {
	if h == nil || repo == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastSeen[webhookRepoKey(repo)] = h.now()
}

// WebhookDeliveryHealthy reports whether webhooks are currently reaching this
// daemon for repo (see WebhookHealth). Nil-safe: a nil tracker is never
// healthy, so an unwired daemon polls at full speed.
func (h *WebhookHealth) WebhookDeliveryHealthy(repo string) bool {
	if h == nil || h.link == nil || repo == "" {
		return false
	}
	openedAt, open := h.link()
	if !open {
		return false
	}
	h.mu.Lock()
	seen, ok := h.lastSeen[webhookRepoKey(repo)]
	h.mu.Unlock()
	return ok && !seen.Before(openedAt) && h.now().Sub(seen) < h.window
}
