# Code Review Reception

## Overview

**Core principle:** Verify before implementing. Ask before assuming. Technical correctness over social comfort.

## The Response Pattern

```
WHEN receiving code review feedback:

1. READ: Complete feedback without reacting
2. UNDERSTAND: Restate requirement in own words (or ask)
3. VERIFY: Check against codebase reality
4. EVALUATE: Technically sound for THIS codebase?
5. RESPOND: Technical acknowledgment or reasoned pushback
6. IMPLEMENT: One item at a time, test each
```

## Within-Run Observations

When the fix brief includes carried observations, treat them as constraints on what the fix may
write. Verify the current premise first, then avoid introducing the named defect class while making
the fix.

## Forbidden Responses

**NEVER:**

- "You're absolutely right!" (explicit instruction-file violation)
- "Great point!" / "Excellent feedback!" (performative)
- "Let me implement that now" (before verification)

**INSTEAD:**

- Restate the technical requirement
- Ask clarifying questions
- Push back with technical reasoning if wrong
- Just start working (actions > words)

## Handling Unclear Feedback

```
IF any item is unclear:
  STOP - do not implement anything yet
  ASK for clarification on unclear items
```

## Source-Specific Handling

### From your human partner

- **Trusted** - implement after understanding
- **Still ask** if scope unclear
- **No performative agreement**
- **Skip to action** or technical acknowledgment

### From External Reviewers

```
BEFORE implementing:
  1. Check: Technically correct for THIS codebase?
  2. Check: Breaks existing functionality?
  3. Check: Reason for current implementation?
  4. Check: Works on all platforms/versions?
  5. Check: Does reviewer understand full context?

IF suggestion seems wrong:
  Push back with technical reasoning

IF can't easily verify:
  Say so: "I can't verify this without [X]. Should I [investigate/ask/proceed]?"

IF conflicts with your human partner's prior decisions:
  Stop and discuss with your human partner first
```

### From Bot Reviewers, After a Clean Review

Identify an automated reviewer generically from the review author, never from a list of product
names: the GraphQL author's `isBot` field, the REST author's `"type": "Bot"` value, or a login
ending in the `[bot]` suffix. Any one signal is sufficient.

The shortcut is verdict-gated: only a verdict positively recorded as `clean` unlocks it; `capped`,
`none`, or an absent record means bot feedback is triaged exactly as today. The verdict in question
is this run's own whole-branch review result, recorded as `REVIEW_VERDICT` in the run note at
`$(git rev-parse --git-dir)/boss-build-review-verdict`.

#### The `REVIEW_VERDICT` run note

Step 6 writes that note the same way Step 5 records its pre-dispatch HEAD: a plain file under the git
dir, outside the worktree, which survives the fresh shell each later block runs in.
`git rev-parse --git-dir` resolves per worktree, so a repair pass in a worktree reads that run's
verdict and never a sibling session's.

`REVIEW_VERDICT` is exactly one of `clean`, `capped`, or `none`: the `case` that writes it collapses
`dispatch-failure`, and anything else it cannot classify, to `none`. boss-build's Preflight clears any
note a previous run left, so the file can never carry a stale verdict across runs. The note is
advisory routing input only — it never substitutes for the file verdict Step 6 itself routes on.

The note is run-scoped, not diff-scoped: it records that this run's whole-branch review passed, and
it is deliberately not re-pinned to a later HEAD when the settle loop or a repair pass commits on
top.

Step 10 is not the note's only reader. A repair pass reads it from a later session that clears
nothing; what bounds the reader there is its own acknowledge-once contract — one grouped response
per bot review, per pass.

After a clean verdict, a bot review is **advisory**. It gets exactly one grouped response comment
per bot review, posted within the bot's own threads, carrying a per-finding reason for every finding
it raised — never a blanket dismissal, and never silence. Reply inside the thread as
`## GitHub Thread Replies` below describes, not as a top-level PR comment: a comment posted at the
top level is not an answer to the threads it was supposed to close.

Advisory is not ignored: a bot finding that names a real defect is still fixed — advisory means it
does not mechanically open a fix cycle, not that the finding is dropped. Judge each finding on the
same technical terms as any other external review, then either fix it and say so in the grouped
response, or give the specific reason it needs no change. A fix taken on the advisory path gets
the same finalize re-verification a Step 8 repair would: run the gates, commit, push, and re-verify
finalize on the new head before the grouped response is posted.

## YAGNI Check for "Professional" Features

```
IF reviewer suggests "implementing properly":
  grep codebase for actual usage

  IF unused: "This endpoint isn't called. Remove it (YAGNI)?"
  IF used: Then implement properly
```

## Implementation Order

```
FOR multi-item feedback:
  1. Clarify anything unclear FIRST
  2. Then implement in this order:
     - Blocking issues (breaks, security)
     - Simple fixes (typos, imports)
     - Complex fixes (refactoring, logic)
  3. Test each fix individually
  4. Verify no regressions
```

## When To Push Back

Push back when:

- Suggestion breaks existing functionality
- Reviewer lacks full context
- Violates YAGNI (unused feature)
- Technically incorrect for this stack
- Legacy/compatibility reasons exist
- Conflicts with your human partner's architectural decisions

**How to push back:**

- Use technical reasoning, not defensiveness
- Ask specific questions
- Reference working tests/code
- Involve your human partner if architectural

**If you're uncomfortable pushing back out loud:** Name that tension, then tell your partner about the issue you've seen. They'll appreciate your honesty.

## Acknowledging Correct Feedback

When feedback IS correct:

```
✅ "Fixed. [Brief description of what changed]"
✅ "Good catch - [specific issue]. Fixed in [location]."
✅ [Just fix it and show in the code]

❌ "You're absolutely right!"
❌ "Great point!"
❌ "Thanks for catching that!"
❌ "Thanks for [anything]"
❌ ANY gratitude expression
```

**If you catch yourself about to write "Thanks":** DELETE IT. State the fix instead.

## Gracefully Correcting Your Pushback

If you pushed back and were wrong:

```
✅ "You were right - I checked [X] and it does [Y]. Implementing now."
✅ "Verified this and you're correct. My initial understanding was wrong because [reason]. Fixing."

❌ Long apology
❌ Defending why you pushed back
❌ Over-explaining
```

## GitHub Thread Replies

When replying to inline review comments on GitHub, reply in the comment thread (`gh api repos/{owner}/{repo}/pulls/{pr}/comments/{id}/replies`), not as a top-level PR comment.
