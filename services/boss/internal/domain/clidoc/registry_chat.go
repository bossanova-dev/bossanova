package clidoc

// chatProse is the prose for the `boss chat` group, kept beside the main
// registry so that literal stays inside its file-size budget.
func chatProse() map[string]Prose {
	return map[string]Prose{
		"boss chat new": {
			Long: "Starts a brand-new live chat inside an existing session, reusing that " +
				"session's worktree, branch and PR with a clean context. This is the CLI " +
				"counterpart of the MCP start_chat tool. `boss new` is not a substitute: on " +
				"a session that is already live the daemon attaches to it instead, and the " +
				"supplied prompt is never run. The agent_session_id is minted for you; the " +
				"command fails rather than reporting success if the daemon could not spawn " +
				"a live agent behind the chat. An omitted --agent inherits the session's " +
				"own agent; --title names the chat. Print the id with --json and feed it " +
				"straight to `boss chat send`.",
			Examples: []Example{
				{Command: "boss chat new <session-id>"},
				{
					Command:     "boss chat new <session-id> --title \"repair round\" --json",
					Explanation: "Capture chat.agent_session_id, then `boss chat send <chat-id> ... --submit`",
				},
			},
		},
		"boss chat send": {
			Long: "Delivers a follow-up message to a running chat identified by a " +
				"session id or agent_session_id (the chat-id printed by `boss new --detach` " +
				"or `boss chat new --json`). " +
				"When given a session id, boss targets that session's primary chat. " +
				"The daemon wakes a sleeping chat before pasting the message; " +
				"--wake-if-asleep defaults to true and exists so a caller can pass " +
				"--wake-if-asleep=false to leave a deliberately stopped chat stopped.",
			Examples: []Example{
				{Command: "boss chat send <session-id|chat-id> \"please also add tests\""},
				{
					Command:     "boss chat send <chat-id> \"status?\" --wake-if-asleep=false",
					Explanation: "Do not wake a stopped chat just to deliver this message",
				},
			},
		},
		"boss chat rename": {
			Long: "Renames a chat identified by a session id or agent_session_id (the " +
				"chat-id printed by `boss new --detach` or `boss chat new --json`). " +
				"When given a session id, boss renames that session's primary chat — " +
				"use `boss rename` to retitle the session itself. " +
				"The new title is the trailing arguments joined with single spaces, so a " +
				"multi-word title needs no quoting; a title that is empty or only " +
				"whitespace is rejected before anything is sent. " +
				"Note that renaming a chat id that does not exist reports success: the " +
				"daemon's update matches no rows and does not fail.",
			Examples: []Example{
				{Command: "boss chat rename <session-id|chat-id> \"repair round 2\""},
				{
					Command:     "boss chat rename <chat-id> second opinion on PR 42",
					Explanation: "Trailing words are joined, so the quotes are optional",
				},
			},
		},
		"boss chat delete": {
			Long: "Deletes one chat by its agent_session_id (the chat-id printed by " +
				"`boss chat new --json`): the daemon stops the chat's agent pane and " +
				"removes the chat from its session. The session, its worktree and its " +
				"other chats are untouched. Unlike the other chat commands it refuses a " +
				"session id, so a mistyped id cannot stop a session's primary agent; " +
				"list a session's chats with `boss chats <session-id>`. Deleting a chat " +
				"that is already gone succeeds. --json prints {agent_session_id, deleted}.",
			Examples: []Example{
				{Command: "boss chat delete <chat-id>"},
				{
					Command:     "boss --host build-host chat delete <chat-id> --json",
					Explanation: "Close a chat on another machine's daemon",
				},
			},
		},
		"boss chat show": {
			Long: "Prints the full conversation transcript for a chat or session's primary chat. " +
				"Use --result-only to print just the final assistant response text " +
				"(suitable for scripting). Use --limit to cap the number of messages.",
			Examples: []Example{
				{Command: "boss chat show <session-id|chat-id>"},
				{Command: "boss chat show <session-id|chat-id> --result-only"},
				{Command: "boss chat show <session-id|chat-id> --limit 10"},
			},
		},
		"boss chat wait": {
			Long: "Blocks until the chat identified by a session id or agent_session_id becomes " +
				"idle or is waiting for input, then prints the final assistant result. " +
				"Polls chat status every few seconds. Use --timeout to limit wait time. " +
				"Typical recipe: `boss new --agent codex --repo R --prompt P --detach` " +
				"then `boss chat wait <session-id|chat-id>` to collect the result.",
			Examples: []Example{
				{Command: "boss chat wait <session-id|chat-id>"},
				{Command: "boss chat wait <session-id|chat-id> --timeout 10m"},
				{
					// `sed`, not `awk '{print $2}'`, and not `cut -d' ' -f2` either. This example is
					// rendered verbatim into a shell fence in the published `boss` skill, and a slash
					// command rewrites `$1`-`$9` in a skill body before any shell parses it, so the awk
					// form would ship with its field reference blanked. `cut` is not the fix: the real
					// output pads the label to a column, so the delimiter run makes field 2 empty. The
					// substitution strips the label and any following whitespace, which is what the
					// reader wants and what check-skill-shell header rule (j) requires of the rendered
					// file. A Go string is outside that gate's reach, so the constraint is recorded here.
					Command: "CHAT=$(boss new --agent codex --repo my-repo " +
						"--prompt \"second opinion on PR #42\" --detach | " +
						"sed -n 's/^chat-id:[[:space:]]*//p') && boss chat wait $CHAT",
					Explanation: "Full cross-agent second-opinion recipe",
				},
			},
		},
	}
}
