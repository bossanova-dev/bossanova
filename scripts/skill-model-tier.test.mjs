// Structural gate for tier -> model routing across the sweep skills.
//
// The `<!-- tier: … -->` annotation has NO runtime consumer — no Go, TS, JS or shell code
// reads it. What actually routes a leg is the `model:` prose directive, which the
// orchestrating agent passes to the Agent/Task tool's `model` argument. So an annotation on
// its own proves nothing; this file gates the directive itself.
//
// Deliberately small and structural, in the shape scripts/bs-sweep-debt-skill.test.mjs and
// scripts/bs-sweep-mutation-skill.test.mjs already use — an explicit table of
// (skill, section) pairs, NOT a tree-wide regex sweep. Over BOTH the `.claude` source and
// the generated `.codex` mirror, because a mirror that lost the directive routes nothing.
//
// Positive table — each routed section must satisfy all five properties:
//   1. it carries a `model:` directive naming `sonnet`;
//   2. it names NO dated model id — the alias is a deliberately moving target, and pinning a
//      date silently freezes the leg on a model that will age out;
//   3. it carries no second, contradicting `model:` value (opus/haiku) in the same section;
//   4. the directive is `$BOSS_AGENT`-guarded, naming both lowercase `claude` and the
//      `codex`-omit case (Codex has no per-leg subagent, so the directive is inert there);
//   5. it carries an escalate/revert sentence naming Opus — without it a degraded cheap leg
//      has no documented exit.
//
// Negative table — the judgment legs must contain no `sonnet` at all, so a copy-paste of a
// routed paragraph into a judgment section fails here rather than quietly downgrading review.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const rootDir = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')
const TREES = ['.claude/skills', '.codex/skills']

const read = (rel) => fs.readFileSync(path.join(rootDir, rel), 'utf8')

/**
 * Split a SKILL.md on headings of the given level and return the section whose heading text
 * starts with `prefix`. The prefix is matched with its trailing punctuation included (e.g.
 * `'Phase 1.5:'`) so a sibling section like `Phase 1.5b:` cannot be picked up instead.
 *
 * Splitting on `###` alone does NOT stop at a following `##`, so the last `###` of a file
 * would swallow every remaining `##` section (e.g. `bs-sweep-mutation`'s
 * `### 8. Create a Ready Green PR` ran to EOF, ~6.7 KB past its own end). That over-broad body
 * makes the JUDGMENT table's `!includes('sonnet')` a future false red on unrelated tail prose —
 * and this branch adds `tier: sonnet` text to sibling sweeps' red-flags tables, so it is a
 * realistic edit. Truncate at the first heading of the same level or shallower.
 */
function section(body, { heading, prefix }) {
  const found = body.split(new RegExp(`\\n${heading} `)).find((s) => s.startsWith(prefix))
  if (!found) return found
  const shallower = heading.length > 2 ? found.search(/\n#{2} /) : -1
  return shallower === -1 ? found : found.slice(0, shallower)
}

// Every dispatch site that routes to the cheap tier today. `bs-sweep-mutation` §2 and
// `bs-sweep-debt` Phase 3 also route, but they are already gated in their own content tests
// — duplicating them here would just double the maintenance cost of one edit.
const ROUTED = [
  { skill: 'bs-sweep-review', heading: '##', prefix: 'Phase 2:', what: 'triage' },
  { skill: 'bs-sweep-sentry', heading: '##', prefix: 'Phase 2:', what: 'triage' },
  { skill: 'bs-sweep-security', heading: '##', prefix: 'Phase 1.5:', what: 'gosec probe' },
  { skill: 'bs-sweep-security', heading: '##', prefix: 'Phase 2:', what: 'select-batch triage' },
  { skill: 'bs-sweep-tests', heading: '##', prefix: 'Phase 3:', what: 'breadth survey' },
]

// Judgment legs. A `sonnet` token appearing in any of these is a downgrade of work whose
// failure mode is SILENT (a missed finding looks exactly like no finding).
const JUDGMENT = [
  { skill: 'bs-sweep-security', heading: '##', prefix: 'Phase 4:', what: 'boss-repair watch' },
  // bs-sweep-tests is the file this branch newly routed, so its two judgment legs are the
  // likeliest copy-paste victims: Phase 6 authors code and its coverage-neutrality proof is
  // the load-bearing judgment, and Phase 8 watches checks. Both sit in the same file as the
  // routed Phase 3 paragraph, ~60 lines below it.
  { skill: 'bs-sweep-tests', heading: '##', prefix: 'Phase 6:', what: 'remove/tighten + verify' },
  { skill: 'bs-sweep-tests', heading: '##', prefix: 'Phase 8:', what: 'PR-gate + check watch' },
  { skill: 'bs-sweep-debt', heading: '##', prefix: 'Phase 6', what: 'fix' },
  { skill: 'bs-sweep-debt', heading: '##', prefix: 'Phase 8', what: 'check-watch' },
  { skill: 'bs-sweep-mutation', heading: '###', prefix: '3. Phase A', what: 'kill survivors' },
  { skill: 'bs-sweep-mutation', heading: '###', prefix: '4. Phase B', what: 'coverage increment' },
  {
    skill: 'bs-sweep-mutation',
    heading: '###',
    prefix: '8. Create a Ready Green PR',
    what: 'completion watch',
  },
]

for (const site of ROUTED) {
  test(`${site.skill} ${site.what} carries a provider-guarded sonnet directive (both mirrors)`, () => {
    for (const tree of TREES) {
      const rel = `${tree}/${site.skill}/SKILL.md`
      const body = section(read(rel), site)
      assert.ok(body, `${rel} must have a "${site.prefix}" section`)

      // ORDER IS DELIBERATE. The dated-id check runs BEFORE the alias check: rewriting
      // `model: sonnet` as `model: claude-sonnet-4-5-…` also deletes the bare alias, so with
      // the alias check first the dated-id rule would never be the assertion that fires and
      // could rot undetected. (Verified by falsification.)

      // 2. no pinned id. The alias moving is the point; a date or version freezes the leg on a
      // model that will age out, silently. The test is "the value contains a digit", not
      // "the value starts with `claude-`": a `claude-`-anchored pattern misses `sonnet-4-5`,
      // which is equally pinned, and which the assertion below would then wave through
      // (`sonnet\b` matches with a hyphen next). Every pinned id carries a digit; the bare
      // alias carries none. (Cross-model review found the `claude-`-only form; verified.)
      assert.doesNotMatch(
        body,
        /model:\s*"?[a-z][-\w.]*\d/i,
        `${rel} ${site.what} must use the bare sonnet alias, not a pinned/dated model id`,
      )

      // 1. a real model directive naming the sonnet alias. `model:` and `sonnet` must be
      // ADJACENT, not merely both present: every routed section also carries a
      // `<!-- tier: sonnet -->` annotation and prose about dropping "the `model:` line", so
      // asserting the two tokens independently passes on a section whose actual directive
      // has been deleted. (Verified by falsification: the independent form stayed green.)
      // The trailing lookahead is what makes this "the bare alias" rather than "starts with
      // sonnet": `\b` alone matches `model: sonnet-4-5`, since a word boundary sits between
      // the `t` and the `-`.
      assert.match(
        body,
        /model:\s*"?sonnet"?(?![-\w.])/,
        `${rel} ${site.what} must carry a bare "model: sonnet" directive, not just the tier annotation`,
      )

      // 3. no second, contradicting model: value in the same section.
      assert.doesNotMatch(
        body,
        /model:\s*"?(opus|haiku)\b/i,
        `${rel} ${site.what} must not carry a contradicting model: value`,
      )

      // 4. $BOSS_AGENT-guarded, covering both the claude and codex-omit cases. The agent
      // names must sit NEXT TO `$BOSS_AGENT`, not merely somewhere in the section:
      // `bs-sweep-security` Phase 1.5 is ~250 lines and quotes `~/.claude/skills/…` paths and
      // the `codex-skills` mirror in unrelated prose, so a section-wide `\bclaude\b` /
      // `\bcodex\b` pair is satisfied there even with the per-agent naming deleted from the
      // guard. (Verified by falsification: rewriting the guard to a bare "when `$BOSS_AGENT`
      // is set … otherwise omit" left the section-wide form green.) The window spans a
      // newline because the paragraph wraps between `$BOSS_AGENT` and the agent name.
      assert.match(
        body,
        /\$BOSS_AGENT/,
        `${rel} ${site.what} directive must be $BOSS_AGENT-guarded`,
      )
      assert.match(
        body,
        /\$BOSS_AGENT[\s\S]{0,24}\bclaude\b/,
        `${rel} ${site.what} guard must name lowercase claude next to $BOSS_AGENT`,
      )
      assert.match(
        body,
        /\$BOSS_AGENT[\s\S]{0,24}\bcodex\b/,
        `${rel} ${site.what} guard must cover the codex-omit case next to $BOSS_AGENT`,
      )

      // 5. an escalate/revert sentence naming Opus. The inter-word gaps are `\s+`, not literal
      // spaces: these are prose paragraphs that prettier reflows on any neighbouring edit, so a
      // literal-space pattern goes red the moment "revert to Opus" happens to wrap across a line
      // — a false failure about formatting, not about the escalate contract being absent.
      assert.match(
        body,
        /Escalate\s+contract/,
        `${rel} ${site.what} must carry an escalate contract`,
      )
      assert.match(
        body,
        /revert\s+to\s+Opus/,
        `${rel} ${site.what} escalate contract must name reverting to Opus`,
      )
    }
  })
}

for (const site of JUDGMENT) {
  test(`${site.skill} ${site.what} stays on Opus — no sonnet directive (both mirrors)`, () => {
    for (const tree of TREES) {
      const rel = `${tree}/${site.skill}/SKILL.md`
      const body = section(read(rel), site)
      assert.ok(body, `${rel} must have a "${site.prefix}" section`)
      assert.ok(
        !body.includes('sonnet'),
        `${rel} ${site.what} is a judgment leg and must not carry a sonnet directive`,
      )
    }
  })
}

// A table that silently matched nothing would pass every assertion above. Pin the sizes so a
// renamed heading shows up as a missing section (asserted per-site) rather than as a table
// that quietly shrank.
test('both routing tables are non-empty and cover the expected sites', () => {
  assert.equal(ROUTED.length, 5, 'the routed table must cover all five routed sections')
  assert.equal(JUDGMENT.length, 8, 'the judgment table must cover all eight stay-on-Opus sections')
})
