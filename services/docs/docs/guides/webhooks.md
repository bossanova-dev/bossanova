---
title: Webhooks
description: 'Send signed HTTPS requests to your own server when a session changes state: endpoints, event types, payload, signatures, test events, retries and idempotency.'
---

import Tabs from '@theme/Tabs';
import TabItem from '@theme/TabItem';
import CommandTabs from '@site/src/components/CommandTabs';

# Webhooks

Bossanova allows you to register webhooks to respond to changes in realtime.
It sends your endpoint a signed `POST` request each time one of your
organization's sessions changes state: its pull request checks pass, it hits
a merge conflict, it is ready for review, it merges, and more. Use webhooks to
trigger follow-up actions like a chat message when something happens in a
session.

Session webhooks are **outbound**. To do the opposite and have Bossanova
respond to an event happening somewhere else, see
[Inbound Triggers](./inbound-triggers.md). To wake an agent chat when a pull
request changes, see [GitHub Callbacks](./github-callbacks.md).

## Plans and permissions

Session webhooks are a paid Bossanova Cloud feature, and each endpoint
belongs to an organization. Only organization owners can manage endpoints. An
organization can register up to 20 webhooks.

## Managing endpoints

Manage webhooks in the web app under **Settings → Webhooks**. The page lists
your organization's webhooks with their endpoint and the events each one
subscribes to. A disabled webhook is shown faded. Each step below also shows
the agent chat request, the `boss webhook` command and the MCP tool that do the
same thing. The `boss webhook` commands need `boss login`, and each takes
`--org`; leave it out to act on your active organization.

:::note
The session webhook MCP tools appear only on the hosted Bossanova Cloud MCP
endpoint. The local `bin/mcp` server does not list them. See
[Hosted-only tools](./mcp.md#hosted-only-tools-bossanova-cloud).
:::

List your organization's webhooks:

<CommandTabs
chat='"list our session webhooks"'
cli="boss webhook ls"
mcp="list_session_webhooks"
/>

List the event types available to a webhook:

<CommandTabs
chat='"which events can a session webhook subscribe to?"'
cli="boss webhook events"
mcp="list_session_webhook_event_types"
/>

### Registering an endpoint

1. Open **Settings → Webhooks** and click **+** (or **New webhook** when the
   list is empty).
2. Enter the **Endpoint URL**. It has to follow the rules below.
3. Add a **Description** to help you recognize the webhook later, up to 200
   characters.
4. Under **Events**, tick the events to subscribe to. Choose at least one. The
   [event table](#event-types) explains when each one fires, and **View
   payload** shows a sample request body.
5. Click **Create webhook**. Bossanova generates the signing secret, and the
   new webhook is enabled straight away.

To register one without the web app, pass the URL and repeat `--event` for
each event type. The webhook is enabled unless you pass `--disabled`:

<CommandTabs
chat='"register a session webhook for https://example.com/hooks/bossanova on session.passing and session.merged, described as Deploy bot"'
cli='boss webhook add https://example.com/hooks/bossanova --event session.passing --event session.merged --description "Deploy bot"'
mcp="save_session_webhook"
/>

The MCP tool creates a webhook when you pass no `id`. It needs `url` and a
non-empty `event_types`.

The endpoint URL must:

- use `https`;
- contain no user name, password or `#fragment`;
- be at most 2,048 bytes; and
- point at a public address.

A host that is, or resolves to, a private, loopback, link-local, multicast or
reserved address is refused. That includes the `169.254.169.254` cloud
metadata service and carrier-grade NAT space. Bossanova checks the URL when
you save it and checks the resolved address again on every send, so a DNS
change after registration does not get around the rule.

### The signing secret

After you create a webhook or rotate its secret, a dialog shows the signing
secret once. Click **Copy**, store it with your server's configuration, then
confirm you have saved it. Bossanova cannot show it again, so if you
lose it, rotate it. After a create, closing the dialog returns you to the list.

`boss webhook add`, `boss webhook rotate-secret` and the
`save_session_webhook` tool also print the secret once, when they create a
webhook or rotate its secret. To choose your own secret, pass
`boss webhook add --secret-file` a file holding it, or `-` to read it from
standard input. The web app and the MCP tool always generate the secret.

To rotate, open the webhook from the list, click **Rotate secret** at the
bottom left of the form and confirm, or:

<CommandTabs
chat='"rotate the signing secret of session webhook <webhook-id>"'
cli="boss webhook rotate-secret <webhook-id>"
mcp="save_session_webhook"
/>

With the MCP tool, rotating is `id` plus `rotate_secret: true` and no other
field. Bossanova signs each request when it sends it, so every event still
waiting to be delivered, retries included, is signed with the new secret from
then on. To rotate without dropping events, have your server accept either
secret for a short while, rotate, then remove the old one.

### Enabling, disabling and editing

Use the power button on a webhook's row to pause or resume it. Click the row,
or its pencil button, to open the webhook's page, where you change its URL,
description or events, then click **Save changes**. Disabling a webhook or
changing its URL cancels every delivery that has not finished, including one
mid-attempt.

<CommandTabs
chat='"disable session webhook <webhook-id>"'
cli="boss webhook edit <webhook-id> --disable"
mcp="save_session_webhook"
/>

`--enable` turns it back on. To change what it subscribes to:

<CommandTabs
chat='"change session webhook <webhook-id> to send only session.failing"'
cli="boss webhook edit <webhook-id> --event session.failing"
mcp="save_session_webhook"
/>

`--event` replaces the whole set of events, so repeat it for each one to
keep. `--url` and `--description` change the other fields. With the MCP
tool, pass the `id` and only the fields to change: `url`, `description`,
`event_types` or `is_enabled`. A non-empty `event_types` replaces the set.

The bin button on a webhook's row deletes the webhook and its delivery history
after you confirm. It cannot be undone.

<CommandTabs
chat='"delete session webhook <webhook-id>"'
cli="boss webhook rm <webhook-id> --yes"
mcp="delete_session_webhook"
/>

The command refuses to delete without `--yes`, and the MCP tool without
`confirm: true`.

### Using the API

The web app is built on the session webhook calls in the
`bossanova.v1.OrchestratorService` API, and so are the `boss webhook`
commands and MCP tools in the tabs on this page, including
[test events](#test-events) and delivery history. The
[API Reference](/reference/api) lists every request and response field. Each
call takes an `organization_id`. Leave it empty to act on your active
organization.

| RPC                            | What it does                                                                  |
| ------------------------------ | ----------------------------------------------------------------------------- |
| `ListSessionWebhookEventTypes` | Lists the event catalog, with a sample payload for each event.                |
| `ListSessionWebhooks`          | Lists the organization's endpoints, oldest first, without secrets.            |
| `CreateSessionWebhook`         | Registers an endpoint and returns its signing secret.                         |
| `UpdateSessionWebhook`         | Changes the URL, description, event types or enabled flag.                    |
| `RotateSessionWebhookSecret`   | Replaces the signing secret and returns the new one.                          |
| `DeleteSessionWebhook`         | Deletes the endpoint together with its delivery history.                      |
| `SendSessionWebhookTestEvent`  | Sends one test event and returns the result. See [Test events](#test-events). |
| `ListSessionWebhookDeliveries` | Pages through an endpoint's delivery history, newest first.                   |
| `GetSessionWebhookDelivery`    | Returns one delivery with its attempts, exact body and headers.               |

## Event types

Each event fires when a session moves into a new state.

| Event              | Label        | Fires when                                                                                                                                    |
| ------------------ | ------------ | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `session.working`  | `working`    | The session's agent started working (its status label became working from any other label).                                                   |
| `session.passing`  | `✓ passing`  | The session's pull request checks passed.                                                                                                     |
| `session.failing`  | `⨯ failing`  | The session's pull request checks failed.                                                                                                     |
| `session.conflict` | `⨯ conflict` | The session's pull request has a merge conflict with its base branch.                                                                         |
| `session.rejected` | `⨯ rejected` | A reviewer requested changes on the session's pull request.                                                                                   |
| `session.merged`   | `✓ merged`   | The session's pull request was merged.                                                                                                        |
| `session.closed`   | `closed`     | The session's pull request was closed without merging.                                                                                        |
| `session.ready`    | `✓ ready`    | The session reached its review hand-off: Ready outranks waiting and idle, and is cleared by any failure, conflict, rejection, merge or close. |

An event fires only on a change. A session that stays in the same state sends
nothing, however many other fields change. Leaving the ready state sends no
event of its own: the state the session moves into sends its own. When one
change produces two events, for example a session that passes and becomes
ready together, they are queued in table order. Archived sessions send
nothing.

## Payload

Each request body is one JSON object, the **envelope**. Here is the catalog
sample for `session.passing`. A test event sent without a payload uses this
body, with Bossanova's own `id`, `organization_id` and `created_at` in place of
the sample values:

```json
{
  "id": "evt_sample_passing",
  "type": "session.passing",
  "created_at": "2026-10-06T12:00:00Z",
  "is_test": true,
  "organization_id": "org_sample",
  "data": {
    "session": {
      "id": "sess_sample",
      "title": "Add rate limiting to the public API",
      "repo_display_name": "acme/api",
      "repo_origin_url": "https://github.com/acme/api.git",
      "branch_name": "feat/rate-limiting",
      "base_branch": "main",
      "pr_number": 42,
      "pr_url": "https://github.com/acme/api/pull/42",
      "state": "ready_for_review",
      "display_status": "passing",
      "display_label": "✓ passing",
      "is_ready": false,
      "tracker_id": "ENG-123",
      "tracker_url": "https://linear.app/acme/issue/ENG-123",
      "updated_at": "2026-10-06T12:00:00Z"
    },
    "previous": {
      "state": "awaiting_checks",
      "display_status": "checking",
      "display_label": "checking",
      "is_ready": false
    }
  }
}
```

| Field             | Meaning                                                                                        |
| ----------------- | ---------------------------------------------------------------------------------------------- |
| `id`              | The event id. Every retry and every endpoint receives the same value.                          |
| `type`            | The event type from the table above.                                                           |
| `created_at`      | When the event was created, RFC 3339 in UTC.                                                   |
| `is_test`         | `true` for a test event, `false` for a live one.                                               |
| `organization_id` | The organization that owns the session.                                                        |
| `data.session`    | The session after the change.                                                                  |
| `data.previous`   | The session's status before the change. Absent when Bossanova has no earlier status to report. |

`data.session` holds these fields:

| Field                             | Meaning                                                                               |
| --------------------------------- | ------------------------------------------------------------------------------------- |
| `id`, `title`                     | The session id and title.                                                             |
| `repo_display_name`               | The repository name, such as `acme/api`.                                              |
| `repo_origin_url`                 | The repository's clone URL.                                                           |
| `branch_name`, `base_branch`      | The session's branch and the branch it merges into.                                   |
| `pr_number`, `pr_url`             | The pull request. `0` and an empty string before a pull request exists.               |
| `state`                           | The session's lifecycle state, in lower case, such as `ready_for_review` or `merged`. |
| `display_status`, `display_label` | The status and label the web app and `boss` show.                                     |
| `is_ready`                        | `true` when the session is ready for review hand-off.                                 |
| `tracker_id`, `tracker_url`       | The linked tracker issue, when there is one.                                          |
| `updated_at`                      | When the session last changed, RFC 3339 in UTC. Use it to order events.               |

`data.previous` carries `state`, `display_status`, `display_label` and
`is_ready` as they were before the change.

Every `data.session` field is always present. A value Bossanova does not
have is sent as an empty string, `0` or `false`. The payload never includes the session's plan
text, its worktree path or terminal name, account details, chat contents or
raw error messages.

Do not depend on key order. Test events re-encode the body with its top-level
keys sorted.

## Headers

Every request carries these headers:

| Header                      | Value                                                                                    |
| --------------------------- | ---------------------------------------------------------------------------------------- |
| `Content-Type`              | `application/json`                                                                       |
| `User-Agent`                | `Bossanova-Webhooks/1`                                                                   |
| `X-Bossanova-Event`         | The event type, such as `session.passing`.                                               |
| `X-Bossanova-Event-Id`      | The event id. It matches the body's `id`.                                                |
| `X-Bossanova-Delivery`      | The delivery id. It is unique per event and endpoint, and stays the same across retries. |
| `X-Bossanova-Test`          | `true` for a test event, otherwise `false`.                                              |
| `X-Bossanova-Signature-256` | The signature, `sha256=` followed by 64 lower-case hex characters.                       |

## Verifying signatures

The signature is the HMAC-SHA256 of the raw request body, keyed with the
endpoint's secret, hex-encoded in lower case and prefixed with `sha256=`. It
uses the same format as GitHub's `X-Hub-Signature-256`.

To verify a request:

1. Read the raw body bytes before any JSON parsing. A re-serialized body
   produces a different signature.
2. Compute the HMAC-SHA256 of those bytes with your secret.
3. Compare `sha256=` plus the hex digest with the header, using a
   constant-time comparison.
4. Reject the request if they differ.

To test your code, use the secret `Jefe` and the body
`what do ya want for nothing?` with no trailing newline. The header must be
`sha256=5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843`.

<Tabs groupId="language">
<TabItem value="node" label="Node.js" default>

```js
import { createHmac, timingSafeEqual } from 'node:crypto'

export function verifySignature(secret, rawBody, header) {
  const expected = Buffer.from(
    'sha256=' + createHmac('sha256', secret).update(rawBody).digest('hex'),
  )
  const received = Buffer.from(header ?? '')
  return expected.length === received.length && timingSafeEqual(expected, received)
}
```

Pass the body as a `Buffer`. In Express, mount `express.raw({ type: 'application/json' })`
on the webhook route so `req.body` holds the raw bytes.

</TabItem>
<TabItem value="python" label="Python">

```python
import hashlib
import hmac


def verify_signature(secret: str, raw_body: bytes, header: str | None) -> bool:
    digest = hmac.new(secret.encode(), raw_body, hashlib.sha256).hexdigest()
    expected = "sha256=" + digest
    return hmac.compare_digest(expected.encode(), (header or "").encode())
```

In Flask, read the body with `request.get_data()`. In Django, use
`request.body`.

</TabItem>
<TabItem value="go" label="Go">

```go
package webhooks

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
)

// VerifySignature reports whether header is the signature of rawBody.
func VerifySignature(secret string, rawBody []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

func HandleWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("BOSSANOVA_WEBHOOK_SECRET")
	if secret == "" {
		// An empty key would accept requests signed by anyone.
		http.Error(w, "webhook secret not configured", http.StatusInternalServerError)
		return
	}
	// Cap the body before reading it, since the sender is not verified yet.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	if !VerifySignature(secret, body, r.Header.Get("X-Bossanova-Signature-256")) {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	// Parse body as JSON and queue the work here.
	w.WriteHeader(http.StatusNoContent)
}
```

</TabItem>
</Tabs>

## Test events

Send a test event to check your server before real events arrive:

1. Open the webhook from the list and click **Test** under **Activity**.
2. Pick an event, and edit the payload if you want to.
3. Click **Send test event**. The result shows the status code, how long the
   request took and the start of the response.

To send one without the web app:

<CommandTabs
chat='"send a session.passing test event to session webhook <webhook-id>"'
cli="boss webhook test <webhook-id> session.passing"
mcp="test_session_webhook"
/>

A test send delivers the event straight away and returns the delivery with its
single attempt. The API call behind it is `SendSessionWebhookTestEvent`.

Pick any event from the catalog. The endpoint does not need to subscribe to
it, and a disabled endpoint still accepts test sends. Leave `payload_json`
empty to send the catalog sample, or pass your own JSON object of at most
64 KiB (65,536 bytes). On the command line, `--payload-file` reads that
object from a file, or from standard input with `-`.

Whatever the payload says, Bossanova replaces `id`, `type`,
`organization_id` and `created_at` with its own values and sets `is_test` to
`true`. The `X-Bossanova-Test` header is `true` as well.

A test event gets one attempt. A failure that a live event would retry is
final here. Each organization gets 10 test sends per minute, and the call
fails with `RESOURCE_EXHAUSTED` past that. A payload rejected as invalid does
not count against the limit.

Test deliveries appear in the delivery history with `is_test` set, and
`ListSessionWebhookDeliveries` filters on it. To list them:

<CommandTabs
chat='"show the test deliveries of session webhook <webhook-id>"'
cli="boss webhook deliveries <webhook-id> --test"
mcp="list_session_webhook_deliveries"
/>

`--live` lists only live deliveries, and `--status` and `--event` narrow
the list further. The MCP tool takes `is_test`, `status` and `event_type`.
To see one delivery with every attempt, the request body and the headers:

<CommandTabs
chat='"show session webhook delivery <delivery-id> with its attempts"'
cli="boss webhook delivery <delivery-id>"
mcp="get_session_webhook_delivery"
/>

## Delivery outcomes and retries

Bossanova queues each event and sends it in the background, so a slow or
failing endpoint never holds up a session. Each attempt is classified like
this:

| Response                                           | Outcome                             |
| -------------------------------------------------- | ----------------------------------- |
| Any `2xx`                                          | Success.                            |
| `408`, `429` or any `5xx`                          | Retried.                            |
| Connection error or timeout                        | Retried.                            |
| `3xx`                                              | Failed. Redirects are not followed. |
| Any other status, such as `400`, `401` or `404`    | Failed.                             |
| An address that is not allowed (see the URL rules) | Failed.                             |

Each attempt has 10 seconds to finish, covering the connection, the TLS
handshake, the request and the response. Return a `2xx` quickly and do slow
work after you respond.

A delivery gets at most 6 attempts. The first is sent right away. After a
retried failure, the next attempt waits:

| After attempt | Wait before the next attempt |
| ------------- | ---------------------------- |
| 1             | 1 minute                     |
| 2             | 5 minutes                    |
| 3             | 30 minutes                   |
| 4             | 2 hours                      |
| 5             | 6 hours                      |

If the sixth attempt also fails, the delivery is marked failed. The last
attempt comes about 8 hours and 36 minutes after the first.

A delivery is in one of these states: `pending`, `in_flight`, `succeeded`,
`failed` or `cancelled`. **Activity** on the webhook's page lists deliveries
newest first. Expand one to see each attempt with its status code, duration
and the first 2 KiB of the response body, plus the request headers and body.
The signature header is recomputed with the current secret, so it differs
from what was sent before a rotation. `GetSessionWebhookDelivery` returns the
same details. Bossanova deletes finished deliveries 30 days after they were
created.

## Idempotency and ordering

Delivery is at least once. A request that already reached your server is
sometimes sent again, for example when Bossanova restarts before it records
the response, so a repeat must be harmless to your handler.

Deduplicate on the event id: the body's `id`, or the `X-Bossanova-Event-Id`
header. Every retry and every endpoint that receives the event sees the same
value. `X-Bossanova-Delivery` is narrower. It names one event sent to one
endpoint and stays the same across retries of that delivery.

Events also arrive out of order. A retry or a parallel send sometimes lands
a newer event before an older one. Compare `data.session.updated_at` with the
last value you stored for that session and ignore anything older.
