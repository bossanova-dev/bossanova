<!-- GENERATED from the boss CLI by `make gen-skill` — do not edit by hand. Index: ../SKILL.md -->

## Chat Control

### `boss chat`

Interact with session chats headlessly

### `boss chat new <session-id> [flags]`

Start a new live chat inside an existing session

Starts a brand-new live chat inside an existing session, reusing that session's worktree, branch and PR with a clean context. This is the CLI counterpart of the MCP start_chat tool. `boss new` is not a substitute: on a session that is already live the daemon attaches to it instead, and the supplied prompt is never run. The agent_session_id is minted for you; the command fails rather than reporting success if the daemon could not spawn a live agent behind the chat. An omitted --agent inherits the session's own agent; --title names the chat. Print the id with --json and feed it straight to `boss chat send`.

**Flags:**

- `--agent` — Agent plugin to run (empty inherits the session's agent)
- `--json` — Emit the new chat as a machine-readable JSON envelope
- `--title` — Title for the new chat

```bash
boss chat new <session-id>
# Capture chat.agent_session_id, then `boss chat send <chat-id> ... --submit`
boss chat new <session-id> --title "repair round" --json
```

### `boss chat rename <session-id|chat-id> <new-title...>`

Rename a chat (updates its title)

Renames a chat identified by a session id or agent_session_id (the chat-id printed by `boss new --detach` or `boss chat new --json`). When given a session id, boss renames that session's primary chat — use `boss rename` to retitle the session itself. The new title is the trailing arguments joined with single spaces, so a multi-word title needs no quoting; a title that is empty or only whitespace is rejected before anything is sent. Note that renaming a chat id that does not exist reports success: the daemon's update matches no rows and does not fail.

```bash
boss chat rename <session-id|chat-id> "repair round 2"
# Trailing words are joined, so the quotes are optional
boss chat rename <chat-id> second opinion on PR 42
```

### `boss chat send <session-id|chat-id> <message> [flags]`

Send a message to a chat

Delivers a follow-up message to a running chat identified by a session id or agent_session_id (the chat-id printed by `boss new --detach` or `boss chat new --json`). When given a session id, boss targets that session's primary chat. The daemon wakes a sleeping chat before pasting the message; --wake-if-asleep defaults to true and exists so a caller can pass --wake-if-asleep=false to leave a deliberately stopped chat stopped.

**Flags:**

- `--submit` — Submit the message (press Enter and verify); false prefills the composer without submitting (default: true)
- `--wake-if-asleep` — Wake a sleeping chat before delivering; false leaves a stopped chat stopped (default: true)

```bash
boss chat send <session-id|chat-id> "please also add tests"
# Do not wake a stopped chat just to deliver this message
boss chat send <chat-id> "status?" --wake-if-asleep=false
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
CHAT=$(boss new --agent codex --repo my-repo --prompt "second opinion on PR #42" --detach | sed -n 's/^chat-id:[[:space:]]*//p') && boss chat wait $CHAT
```
