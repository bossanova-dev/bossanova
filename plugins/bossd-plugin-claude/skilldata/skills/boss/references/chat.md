<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Chat Control

### `boss chat`

Interact with session chats headlessly

### `boss chat send <session-id|chat-id> <message> [flags]`

Send a message to a chat

Delivers a follow-up message to a running chat identified by a session id or agent_session_id (the chat-id printed by `boss new --detach`). When given a session id, boss targets that session's primary chat. The daemon wakes a sleeping chat before pasting the message.

**Flags:**

- `--submit` — Submit the message (press Enter and verify); false prefills the composer without submitting (default: true)

```bash
boss chat send <session-id|chat-id> "please also add tests"
```

### `boss chat show <session-id|chat-id> [flags]`

Print a chat transcript

Prints the full conversation transcript for a chat or session's primary chat. Use --result-only to print just the final assistant response text (suitable for scripting). Use --limit to cap the number of messages.

**Flags:**

- `--limit` — Maximum number of messages to show (0 = all) (default: 0)
- `--result-only` — Print only the final assistant result text

```bash
boss chat show <session-id|chat-id>
boss chat show <session-id|chat-id> --result-only
boss chat show <session-id|chat-id> --limit 10
```

### `boss chat wait <session-id|chat-id> [flags]`

Wait for a chat to become idle, then print the result

Blocks until the chat identified by a session id or agent_session_id becomes idle or is waiting for input, then prints the final assistant result. Polls chat status every few seconds. Use --timeout to limit wait time. Typical recipe: `boss new --agent codex --repo R --prompt P --detach` then `boss chat wait <session-id|chat-id>` to collect the result.

**Flags:**

- `--timeout` — Maximum time to wait (e.g. 5m, 1h) (default: 30m0s)

```bash
boss chat wait <session-id|chat-id>
boss chat wait <session-id|chat-id> --timeout 10m
# Full cross-agent second-opinion recipe
CHAT=$(boss new --agent codex --repo my-repo --prompt "second opinion on PR #42" --detach | awk '/^chat-id/{print $2}') && boss chat wait $CHAT
```
