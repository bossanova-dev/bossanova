---
title: Signing In
description: Sign in to Bossanova Cloud from the terminal with boss login or from the TUI with l, and confirm that it worked.
---

import CommandTabs from '@site/src/components/CommandTabs';

# Signing In

Bossanova signs in with a **device code**. The client shows you a short code and
a verification URL, you approve that code in a browser, and the client stores
the credentials it gets back. There are two front ends for the same flow: the
`boss login` command and the `l` key in the Terminal UI (TUI). They diverge
in only two places, both called out below.

## When you need to sign in

Signing in is only needed for **Bossanova Cloud**: browser access to your
sessions through the [Web App](./web.md), and pairing more than one machine to
the same account. The TUI, the daemon, worktrees, agent plugins, and the pull
request automation all work signed out.

Already signed in? Check before you start:

<CommandTabs
cli="boss auth-status"
/>

`boss auth-status` reads this machine's own credential store, so there is no
chat prompt and no MCP tool for it; the tabs record that rather than leaving it
ambiguous.

If it prints `Logged in.` you are already signed in and can stop here;
[Confirming it worked](#confirming-it-worked) explains the rest of that output.
The TUI carries the same signal in its home action bar: it offers `[l]ogout`
when you are signed in, and `[l]ogin` when you are not.

## Sign in from the CLI {#cli}

<CommandTabs
cli="boss login"
/>

`boss login` drives a browser flow on this machine, so there is no chat prompt
and no MCP tool for it either.

The command prints your code and the verification URL, then waits:

```text

Your authentication code: <your-code>

Visit: <verification-url>

Waiting for authentication...
```

Both values above are placeholders. The real code is short-lived and
single-use, and the real URL carries that same code inside it — so treat both as
credentials, and keep them out of issues, chat messages, and screenshots.

`boss login` also tries to open the verification URL in your browser. When it
cannot, it says so and leaves the URL on screen for you:

```text
Could not open browser. Please visit the URL above.
```

Open that URL and approve the code. It does not have to be this machine's
browser: the URL works from **any device** (a phone, a tablet, another
laptop), which is what makes the flow usable over SSH or on a headless box.

:::warning Approve only a code you started yourself
Approving grants a device access to your account. If a code or a verification
URL reaches you any other way (someone sends you one, or one appears that you
did not ask for), do not approve it. Approve only the code that the `boss login`
or TUI run in front of you just printed.
:::

Once you approve, polling stops and the command prints your account:

```text
Logged in as you@example.com
```

The command keeps polling until the device code's own expiry. If it runs out,
start it again for a fresh code; see
[Troubleshooting](../help/troubleshooting.md#workos-device-code-flow-times-out).

## Sign in from the TUI {#tui}

Start the TUI and press `l` on the home screen:

<CommandTabs
cli="boss"
/>

The action bar at the bottom of the home screen reads `[l]ogin` while you are
signed out and `[l]ogout` once you are signed in, so one key covers both
directions and the label tells you which one you are about to get.

The login screen shows `Requesting device code...`, then the same two values the
CLI prints, with a spinner while it waits:

```text
Your authentication code: <your-code>

Visit: <verification-url>

Waiting for authentication...
```

The action bar offers `[esc] cancel`. As on the CLI, the verification URL can be
opened on **any device**. You do not have to approve from the machine running
the TUI.

The difference that matters: here, a failed browser-open is silent. The TUI
opens the verification URL on a best-effort basis and discards the result, so
there is no `Could not open browser` line and no error message; a browser that
never appeared looks exactly like a browser you simply did not notice. Do not
wait for a warning that is never coming. If no browser window shows up within a
second or two, take the `Visit:` URL off the screen and open it yourself.

Once you approve, the TUI does not stop on a "signed in" screen. It goes
straight into checking your Bossanova Cloud account, so what you see next is a
spinner and `Loading your account...` rather than a `Logged in as ...` line. On
an account with cloud access that check passes through to
`Bossanova Cloud is ready. Returning home...` and the TUI returns to the home
screen on its own; on an account without it, the same screen becomes the
checkout flow described in
[If Bossanova Cloud needs a subscription](#if-bossanova-cloud-needs-a-subscription).

The account check is skipped only on a client with no cloud endpoint at all —
the local-only posture described in [Web App](./web.md#local-only-mode), reached
by setting `BOSSD_ORCHESTRATOR_URL` to an explicitly empty value. That page sets
it in `bossd`'s environment; the check described here is the TUI's own, so it is
this process's environment that decides it. There the screen shows
`Logged in as you@example.com` and returns home on its own.

## Confirming it worked {#confirming-it-worked}

<CommandTabs
cli="boss auth-status"
/>

A signed-in machine reports the account and how much life the current access
token has left:

```text
Logged in.
  Email: you@example.com
  Token expires: 2027-01-30T09:15:00Z
  Remaining: 23h58m12s
```

A machine that never signed in, or that has signed out, reports:

```text
Not logged in.
Run 'boss login' to authenticate with Bossanova cloud.
```

There is a third outcome this guide does not cover: `Sign in required.`, with
the account and a reason, means stored credentials that can no longer be used;
[Troubleshooting](../help/troubleshooting.md#auth-and-login) has the remedies.

In the TUI, the home action bar is the quick version of the same check:
`[l]ogout` means you are signed in.

## If Bossanova Cloud needs a subscription

Being signed in and having Bossanova Cloud access are two different things.
Bossanova Cloud is a paid add-on to the free client, so a successful sign-in on
an account with no active subscription gets a follow-up, and the two surfaces
handle that follow-up very differently. Local sessions keep working either way.

### From the CLI

`boss login` prints a two-line message in place of the `Logged in as ...` line,
and opens the subscription page in a **second browser tab**:

```text
Bossanova Cloud requires an active subscription.
Local sessions are still available.
```

If that second tab cannot be opened, the URL is printed just above that message
instead:

```text
Open subscription page: https://app.bossanova.dev/subscribe?source=cli
```

That URL is the production default; a client pointed at a staging or local
environment prints that environment's subscribe page instead. And when the
account is only waiting for an entitlement refresh to land, the two-line message
is all you get: no second tab and no URL, because there is nothing to buy.

Then the command exits and hands you back your shell. Your credentials were
still stored (`boss auth-status` will say `Logged in.`), so the only thing
missing is the subscription. Finish checkout in the browser, then re-run
whatever needed cloud access.

### From the TUI

The post-approval account check described above is this same screen. On an
account with no active subscription it does not pass through; it becomes an
**interactive checkout screen** and stays there rather than printing a note and
returning home:

- It opens the checkout page in your browser. Unlike the login screen's browser
  open, this one is not silent: if it fails, the screen adds
  `Open this billing URL: ...` beneath the status line.
- It then polls your account every **3 seconds**, showing either
  `Loading your account...` or
  `Activating your subscription. This can take a few minutes...`.
- The action bar changes with the state:
  `[enter] re-open subscription page  [esc] cancel` once there is a checkout
  page to reopen, and `[enter] check again  [esc] cancel` while it waits for the
  activation to land. Every other state (the first check, creating the checkout,
  the timeout, a failure, and the success moment) shows
  `[enter] retry  [esc] cancel`. `o` re-opens the subscription page too.
- It stops waiting after **2 minutes** and shows
  `Subscription activation is taking longer than expected.` above
  `Press enter to check again or reopen checkout.` That is the wait timing
  out, not the purchase failing.
- On success it shows `Bossanova Cloud is ready. Returning home...` and returns
  to the home screen.
- If billing itself is unreachable it says so and offers
  `[enter] continue  [esc] cancel`; continuing drops you back into a working,
  local-only Bossanova.

`[esc]` leaves the checkout screen at any point, and leaving it undoes nothing
about the sign-in you just completed.

## Where to go next

- Set up browser access in [Web App](./web.md).
- `boss logout` removes the stored credentials from this machine; the full
  command table is in
  [Web App](./web.md#authentication-management).
- To point the flow at a non-production environment, set `BOSS_WORKOS_CLIENT_ID`
  and `BOSS_CLOUD_URL`; see [Settings](../reference/settings.md).
- What gets stored, and where, is in
  [Security and Permissions](../reference/security-and-permissions.md).
- If signing in fails, work through
  [Troubleshooting](../help/troubleshooting.md#auth-and-login).
