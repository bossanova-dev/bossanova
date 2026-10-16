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

// polledReadStaleness bounds how old a poll's view of a PR can be: pollers
// read through the GitHub read cache, so a change a poll reports may have
// happened up to a cache TTL before the poll ran.
const polledReadStaleness = 2 * time.Minute

// missedDeliveryGrace is how long a poll-observed change must go unannounced
// before it counts as a missed webhook, so a poll that merely beat a slow
// delivery is never reported.
const missedDeliveryGrace = 5 * time.Minute

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
	// latestDelivery is when the most recent webhook for any repo arrived.
	latestDelivery time.Time
	// missedAt maps a repo key to when a poll first found a PR change in it
	// that no webhook announced (NotePolledChange). A delivery clears it.
	missedAt map[string]time.Time
}

// NewWebhookHealth builds a tracker over link, typically
// StreamClient.StreamOpenSince.
func NewWebhookHealth(link func() (time.Time, bool)) *WebhookHealth {
	return &WebhookHealth{
		link:     link,
		now:      time.Now,
		window:   webhookDeliveryWindow,
		lastSeen: make(map[string]time.Time),
		missedAt: make(map[string]time.Time),
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

// githubRepoKey is webhookRepoKey restricted to origins it can name as a
// GitHub owner/name. Bosso keys deliveries by the https origin, so a repo whose
// origin does not normalise (an SSH host alias such as git@github-work:o/r.git)
// can never be matched against them, and reports nothing about missed webhooks.
func githubRepoKey(repo string) (string, bool) {
	nwo := vcs.GitHubNWO(repo)
	return strings.ToLower(nwo), nwo != ""
}

// RecordDelivery notes that a webhook for repo just arrived.
func (h *WebhookHealth) RecordDelivery(repo string) {
	if h == nil || repo == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	key := webhookRepoKey(repo)
	now := h.now()
	h.lastSeen[key] = now
	h.latestDelivery = now
	delete(h.missedAt, key)
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

// NotePolledChange records that a poll found a PR in repo changed (a new head,
// or the PR ending) since it last read that PR at since: a change GitHub
// announces by webhook. It counts as a missed webhook only when the stream was
// open across that whole window, so no delivery could have been lost to a
// disconnect, and no webhook for repo has reached this daemon since it started.
// The first such change is kept; a later delivery for repo clears it.
func (h *WebhookHealth) NotePolledChange(repo string, since time.Time) {
	if h == nil || h.link == nil {
		return
	}
	key, ok := githubRepoKey(repo)
	if !ok {
		return
	}
	// Stamped before link() so a reconnect right after it returns leaves the
	// stamp before the new stream opened, where WebhooksMissing ignores it.
	noted := h.now()
	openedAt, open := h.link()
	if !open || since.Before(openedAt.Add(polledReadStaleness)) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, delivered := h.lastSeen[key]; delivered {
		return
	}
	// A miss from before the current stream opened no longer counts
	// (WebhooksMissing), so a fresh one replaces it.
	if prior, ok := h.missedAt[key]; !ok || prior.Before(openedAt) {
		h.missedAt[key] = noted
	}
}

// WebhooksMissing reports that GitHub webhooks are evidently not reaching this
// daemon for repo: a poll found a change in it that no webhook announced within
// missedDeliveryGrace (NotePolledChange) and none has arrived for it since,
// the stream is open, and some repo has had a delivery on the current stream,
// so the pipe itself works. Nil-safe: a nil tracker (no upstream) never reports
// missing.
func (h *WebhookHealth) WebhooksMissing(repo string) bool {
	if h == nil || h.link == nil {
		return false
	}
	key, ok := githubRepoKey(repo)
	if !ok {
		return false
	}
	openedAt, open := h.link()
	if !open {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	missed, ok := h.missedAt[key]
	// A miss noted before the current stream opened proves nothing: the
	// delivery that would have cleared it may have been lost in the disconnect.
	// Only a delivery on this stream proves it forwards anything; otherwise a
	// reconnect that stopped forwarding for every repo would flag each one.
	pipeWorks := !h.latestDelivery.Before(openedAt)
	return ok && !missed.Before(openedAt) && pipeWorks && h.now().Sub(missed) >= missedDeliveryGrace
}
