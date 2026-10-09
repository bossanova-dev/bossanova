<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Session Webhooks

### `boss webhook`

Manage outbound session webhooks for your organization (Bossanova Cloud)

### `boss webhook add <url> [flags]`

Register a webhook endpoint and print its signing secret once

**Flags:**

- `--description` — Description shown beside the webhook (at most 200 characters)
- `--disabled` — Create the webhook disabled
- `--event` — Event type to subscribe to (see 'boss webhook events'); repeat for several
- `--json` — Emit {webhook, secret} as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)
- `--secret-file` — Read the signing secret from a file (or '-' for stdin; default: generated)

### `boss webhook deliveries <webhook-id> [flags]`

List a webhook's deliveries, newest first

**Flags:**

- `--event` — Only deliveries of this event type
- `--json` — Emit {deliveries, next_page_token} as a stable JSON schema
- `--live` — Only live (non-test) deliveries
- `--org` — Organization id; must be your active organization (empty uses it)
- `--page-size` — Deliveries per page (0 = server default 25; at most 100) (default: 0)
- `--page-token` — Page token from a previous call with the same filters
- `--status` — Only deliveries in this status: pending, in_flight, succeeded, failed or cancelled
- `--test` — Only test deliveries

### `boss webhook delivery <delivery-id> [flags]`

Show one delivery with every attempt, the request body and headers

**Flags:**

- `--json` — Emit {delivery, attempts, request_body, request_headers} as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)

### `boss webhook edit <webhook-id> [flags]`

Change a webhook's URL, description, events or enabled state

**Flags:**

- `--description` — New description
- `--disable` — Disable the webhook (cancels its undelivered deliveries)
- `--enable` — Enable the webhook
- `--event` — Replace the subscribed event types; repeat for several
- `--json` — Emit the webhook as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)
- `--url` — New endpoint URL (https)

### `boss webhook events [flags]`

List the event types a webhook can subscribe to

**Flags:**

- `--json` — Emit {event_types} as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)

### `boss webhook ls [flags]`

List the organization's webhooks

**Flags:**

- `--json` — Emit {webhooks} as a stable JSON schema instead of a table
- `--org` — Organization id; must be your active organization (empty uses it)

### `boss webhook rm <webhook-id> [flags]`

Delete a webhook and its delivery history

**Flags:**

- `--json` — Emit the deleted webhook id as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)
- `--yes`, `-y` — Confirm the permanent deletion (required)

### `boss webhook rotate-secret <webhook-id> [flags]`

Replace a webhook's signing secret and print the new one once

**Flags:**

- `--json` — Emit {webhook, secret} as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)
- `--secret-file` — Read the new signing secret from a file (or '-' for stdin; default: generated)

### `boss webhook test <webhook-id> <event-type> [flags]`

Send one test delivery to a webhook and show the result

**Flags:**

- `--json` — Emit {delivery, attempt} as a stable JSON schema
- `--org` — Organization id; must be your active organization (empty uses it)
- `--payload-file` — JSON object to send (or '-' for stdin; default: the event's catalog sample)
