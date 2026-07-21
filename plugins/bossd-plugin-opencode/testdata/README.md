# opencode `--format json` fixtures

JSONL captures of `opencode run --format json` (opencode v1.18.3, upstream
`anomalyco/opencode`), modeled on the documented event schema with any
credentials scrubbed. Each line is one event; **every** event carries a
top-level `sessionID` of the form `ses_XXXXXXXXXXXXXXXXXXXX` (also duplicated
under `part.sessionID`). The stream starts at `step_start` — the user's own
prompt is not echoed.

| fixture                 | shape                                                                       |
| ----------------------- | --------------------------------------------------------------------------- |
| `run_fresh.jsonl`       | successful fresh run; first event's `sessionID` is the canonical session id |
| `run_resume.jsonl`      | resume run echoing a different session id                                   |
| `error_auth_401.jsonl`  | `error` event with `error.statusCode` 401                                   |
| `error_auth_403.jsonl`  | `error` event with `error.statusCode` 403                                   |
| `error_usage_429.jsonl` | `error` event with `error.statusCode` 429 (rate-limit / usage)              |

The parsers (`sessionIDFromOutput`, `detectAuthFailure`, `classifyUsageCap`) key
off the stable fields shown here — top-level `sessionID` and the nested
`error.{statusCode,message}` — and tolerate unknown event `type`s, so a future
opencode release that adds or renames event types does not break detection.
