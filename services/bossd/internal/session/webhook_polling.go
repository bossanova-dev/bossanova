package session

import "time"

// webhookSafetyNetInterval is how often the GitHub pollers still poll a PR in
// a repo whose webhooks are known to reach this daemon (WebhookHealth). Every
// change on such a PR arrives as a webhook that refreshes it immediately — the
// display refresh, the callback evaluation and the state-machine PollPR — so
// the scheduled poll only has to catch a delivery bosso dropped.
const webhookSafetyNetInterval = 10 * time.Minute

// WebhookHealth reports whether GitHub webhooks are currently reaching this
// daemon for a repo (identified by origin URL or "owner/name").
// upstream.WebhookHealth implements it.
type WebhookHealth interface {
	WebhookDeliveryHealthy(repo string) bool
}
