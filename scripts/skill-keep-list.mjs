#!/usr/bin/env node

// skill-keep-list — a DELETION gate for skill prose: the named load-bearing items must still be
// STATED somewhere in the artifact after a rewrite, whatever words state them.
//
// THE DEFECT (BOS-1216, child of epic BOS-1206). A register shift from procedure to invariant is a
// bulk deletion, and the failure mode of a bulk deletion is silent: a behaviour disappears with the
// paragraph that described it, every gate stays green, and nobody finds out until an unattended run
// takes a route nothing routes any more. The pins in scripts/boss-build-skill.test.mjs do not close
// this — they pin SENTENCES, so they red on a rewrite that preserves the behaviour and pass on one
// that drops it, which is exactly backwards for a rewrite.
//
// THE RULE. The keep list names WHAT must remain decidable by a reader, not HOW it is worded. Each
// item is a `{ name, file, pattern }` triple; `keepListMisses` returns the names whose pattern is
// absent. A rewrite is free to move an item between sections, re-word its prose, or fold it into a
// helper call — it is not free to make the item unfindable.
//
// WHY PATTERNS, NOT SENTENCES. A whole-sentence pattern is a prose pin wearing a different hat, and
// would re-create the ratchet this epic exists to unwind. Prefer a RULE NAME, a STRUCTURAL LEAD (a
// heading, a bullet lead, a variable name), or a TOKEN LITERAL that a downstream consumer matches
// byte-for-byte. Those are the three things a rewrite may not silently change. Where an item is
// anchored on a CLAUSE instead — several below are, because the invariant has no heading or literal
// of its own — that clause IS pinned wording, and a paraphrase reds it. The fix for a paraphrase is
// to restore the clause, or to re-anchor the item on a structural lead in the SAME commit. Deleting
// the entry is never the fix for a paraphrase; it is only ever the fix for an item that genuinely
// no longer applies.
//
// ---------------------------------------------------------------------------------------------
// TWO PATTERN KINDS, AND THE DIFFERENCE IS DELIBERATE
//
//   A STRING pattern is matched as a BYTE-EXACT substring of the RAW text. Use it where the bytes
//   themselves are the contract — the PARTIAL merge-gate marker that boss-epic greps for, a heading
//   that scripts/boss-build-skill.test.mjs slices a region on. A string that drifts by one byte is
//   a broken integration, so nothing here is allowed to normalise it away.
//
//   A REGEXP pattern is matched against a WHITESPACE-COLLAPSED copy (`/\s+/g` -> one space). Markdown
//   is hard-wrapped at a print width nobody controls, so a clause that reads as one sentence is
//   routinely split across a line break mid-phrase. Matching the collapsed copy means a pattern
//   describes the CLAUSE rather than the current wrapping, and a reflow — which changes no
//   behaviour — cannot red this gate. Because the copy has no line structure, a `^`/`$`-anchored
//   regex will not do what its author meant; use a string for a heading instead.
//
// FAIL CLOSED. A file named by an item but absent from `sources` makes every item on that file a
// miss. An unread file is not an file with everything in it, and this gate's green state is
// "nothing missing", which is byte-identical to a run that looked at nothing.
//
// Exercised by scripts/skill-keep-list.test.mjs, wired over the real artifacts in
// scripts/boss-build-skill.test.mjs, and runnable via `node scripts/skill-keep-list.mjs`.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

const BOSS_BUILD = 'services/boss/internal/skillinstall/skills/boss-build'
export const REVIEW_STACK = `${BOSS_BUILD}/references/review-stack.md`
export const BOSS_BUILD_BODY = `${BOSS_BUILD}/SKILL.md`

/**
 * The whitespace-collapsed view a RegExp pattern is tested against.
 *
 * @param {string} text Raw document text.
 * @returns {string} The same text with every whitespace run replaced by a single space.
 */
export function collapse(text) {
  return text.replace(/\s+/g, ' ')
}

function matchOne(pattern, raw, collapsed, itemName) {
  if (pattern instanceof RegExp) {
    if (pattern.global || pattern.sticky) {
      throw new Error(
        `skill-keep-list: item ${itemName} has a \`g\`/\`y\`-flagged RegExp. \`test\` advances ` +
          '`lastIndex` on those, so the same item alternates between present and missing across ' +
          'calls, and KEEP_LIST is shared by two suites. Drop the flag. Wiring error.',
      )
    }
    return pattern.test(collapsed)
  }
  if (typeof pattern === 'string') {
    if (pattern === '') {
      throw new Error(
        `skill-keep-list: item ${itemName} has an EMPTY string pattern, which matches every ` +
          'document and so asserts nothing. Wiring error.',
      )
    }
    return raw.includes(pattern)
  }
  throw new Error(
    `skill-keep-list: item ${itemName} has a pattern of type ${typeof pattern}; only a string, ` +
      'a RegExp, or an array of those is accepted. Wiring error.',
  )
}

/**
 * The names of the keep-list items whose pattern is absent from their file.
 *
 * An item's `pattern` may be a single string/RegExp or an ARRAY of them, in which case EVERY member
 * must be present — one item, several clauses, so a multi-clause invariant does not have to be
 * split into several names to be checked whole.
 *
 * @param {Record<string, string>|Map<string, string>} sources File path -> document text.
 * @param {{name: string, file: string, pattern: (string|RegExp|(string|RegExp)[])}[]} items
 * @returns {string[]} Missing item names, in the order the items were declared.
 */
export function keepListMisses(sources, items) {
  if (!Array.isArray(items)) {
    throw new Error('skill-keep-list: `items` must be an array of keep-list items. Wiring error.')
  }
  const read = (file) => (sources instanceof Map ? sources.get(file) : sources?.[file])

  // One collapsed copy per file rather than per item: the same document backs many items, and the
  // collapse is the only non-trivial cost in here.
  const collapsedCache = new Map()

  const misses = []
  const seen = new Set()
  for (const item of items) {
    const { name, file, pattern } = item ?? {}
    if (typeof name !== 'string' || name.trim() === '') {
      throw new Error('skill-keep-list: every item needs a non-empty `name`. Wiring error.')
    }
    if (seen.has(name)) {
      // A duplicate name makes a report ambiguous — the reader cannot tell which item is missing.
      throw new Error(`skill-keep-list: duplicate item name ${name}. Wiring error.`)
    }
    seen.add(name)
    if (typeof file !== 'string' || file.trim() === '') {
      throw new Error(`skill-keep-list: item ${name} needs a non-empty \`file\`. Wiring error.`)
    }
    if (pattern === undefined || pattern === null) {
      throw new Error(`skill-keep-list: item ${name} needs a \`pattern\`. Wiring error.`)
    }
    const patterns = Array.isArray(pattern) ? pattern : [pattern]
    if (patterns.length === 0) {
      throw new Error(
        `skill-keep-list: item ${name} has an EMPTY pattern array, which asserts nothing. ` +
          'Wiring error.',
      )
    }

    const raw = read(file)
    if (typeof raw !== 'string') {
      // Fail closed: an unsupplied file is not a file that contains the item.
      misses.push(name)
      continue
    }
    if (!collapsedCache.has(file)) collapsedCache.set(file, collapse(raw))
    const collapsed = collapsedCache.get(file)

    if (!patterns.every((one) => matchOne(one, raw, collapsed, name))) misses.push(name)
  }
  return misses
}

// ---------------------------------------------------------------------------------------------
// THE KEEP LIST
//
// Every entry is a review-side behaviour that a reader of the boss-build review stack must still be
// able to decide from the text. The pair of files is deliberate: item 15 lives in the RESIDENT body
// rather than the reference, and a keep list scoped to one file could lose it across the seam.
export const KEEP_LIST = [
  {
    name: 'REVIEW_READY is a terminal state reached with findings published',
    file: REVIEW_STACK,
    pattern: [
      '### REVIEW_READY-with-findings publication',
      /\*\*Stop cleanly\*\* with `REVIEW_READY`/,
    ],
  },
  {
    // BYTE-EXACT on purpose: boss-epic's merge gate greps a PR's `title,body` for this substring.
    name: 'the PARTIAL merge-gate marker literal',
    file: REVIEW_STACK,
    pattern: 'do not merge — partial: <satisfied>/<total> acceptance criteria',
  },
  {
    name: 'BLOCKED has exactly four causes, all four enumerated',
    file: REVIEW_STACK,
    pattern: [
      /exactly four causes/,
      /\(1\)[^)]{0,60}quality gates are red[\s\S]{0,120}\(2\)[\s\S]{0,80}cannot be pushed[\s\S]{0,280}\(3\)[\s\S]{0,280}version bump[\s\S]{0,280}\(4\)[\s\S]{0,120}unsafe/,
    ],
  },
  {
    name: 'a capped review and open findings are not BLOCKED causes',
    file: REVIEW_STACK,
    pattern: [/a capped review is not one of them/, /are \*\*published\*\*, never fatal/],
  },
  {
    name: 'the API-version hard gate is a must-fix and the sole publish-not-block exception',
    file: REVIEW_STACK,
    pattern: [
      /never a Minor\/deferrable one/,
      /\*\*required-deferred\*\* item/,
      /`BLOCKED` cause \(3\)/,
      /sole exception/,
    ],
  },
  {
    name: 'the reserved merge-gate token ban on PR title and body',
    file: REVIEW_STACK,
    pattern: [
      '### The reserved merge-gate token (every route, no exceptions)',
      /no text sourced from `boss-review` may place that substring in a PR \*\*title or body\*\*/,
    ],
  },
  {
    name: "PARTIAL's T1/T2/T3 gate, the 0/<total> floor, and the provisional-seed bar",
    file: REVIEW_STACK,
    pattern: [
      /The T1\/T2\/T3 gate is specified in/,
      /`0\/<total>` is the universal soft landing/,
      /never eligible for `PARTIAL`/,
    ],
  },
  {
    name: 'the two never-omitted sections keep their whole token vocabulary',
    file: REVIEW_STACK,
    pattern: [
      '## Review coverage',
      '## Cross-model review',
      'none: review stack did not run',
      'none: review verdict unreadable',
      'none: review coverage unknown',
      /- `full` —/,
      /`quick: <reason>/,
      /- `clean` —/,
      /`findings-fixed/,
      /- `skipped: <reason>`/,
      /- `error: <reason>`/,
      /never omitted/,
    ],
  },
  {
    name: 'the push procedure keeps its three outcomes and its reconcile rule',
    file: REVIEW_STACK,
    pattern: [
      /PUSHED=yes\|rescue\|no/,
      /Plain `git push` — never `--force`\/`--force-with-lease`/,
      /`git rebase --no-fork-point FETCH_HEAD`/,
      /never `git pull --rebase`/,
      /never a merge/,
    ],
  },
  {
    name: 'the sentinel is generated and persisted by helpers, never hand-written',
    file: REVIEW_STACK,
    pattern: [
      /bs-review-caps\.mjs/,
      /bs-run-sentinel\.mjs write/,
      /Never hand-write a sentinel literal/,
      /dispatch-failure/,
    ],
  },
  {
    name: 'base drift keeps unevaluated/skipped apart and rebases only when refreshable',
    file: REVIEW_STACK,
    pattern: [
      /\*\*`unevaluated` is not `clean`\*\*/,
      /\*\*`skipped` is neither\*\*/,
      /\*\*Refreshable drift\*\*/,
      /only reading that rebases/,
    ],
  },
  {
    // BOS-1216 review: the tier-selection rule was one of the most heavily rewritten regions in
    // that change and nothing pinned it. The `## Review coverage` item pins the OUTPUT tokens
    // (`full`, `quick: <reason>`), which is the disclosure vocabulary, not the rule that chooses
    // between them — so an inverted comparison or a dropped override precedence would stay green.
    name: 'the tier rule resolves ambiguity toward more coverage and compares strictly',
    file: REVIEW_STACK,
    pattern: [
      /`reviewDefaults\.forceFull` is \*\*true\*\* → \*\*full tier\*\*/,
      /Ambiguity resolves toward \*\*more\*\* coverage, never less/,
      /The comparison in branch 3 is \*\*strict\*\*/,
      /\*\*A single lens hit is enough\.\*\*/,
      /Do not re-introduce a wall-clock term into this rule/,
    ],
  },
  {
    name: 'the funding reason is a stated interface and an allowance names two numbers',
    file: REVIEW_STACK,
    pattern: [
      /`STEP_6C_FUNDING_REASON` is a stated interface, not ambient shell state/,
      /as \*\*two separate numbers\*\*/,
    ],
  },
  {
    name: 'the reviewer-dispatch bound and the one-review-system invariant',
    file: REVIEW_STACK,
    pattern: [
      /\*\*One review system, not three\.\*\*/,
      /at most four reviewer dispatches \(≤ 4\) per run/,
    ],
  },
  {
    name: 'the review pass is awaited and never backgrounded',
    file: REVIEW_STACK,
    pattern: [
      /\(\*\*await\*\*, \*\*never\*\* `run_in_background`\)/,
      /\*\*await-only\*\* \(never `run_in_background`\)/,
    ],
  },
  {
    name: 'reviewed-tip confirmation fails closed, and unknown is not a match',
    file: REVIEW_STACK,
    pattern: [
      '### Reviewed-tip confirmation (Step 7 only — `PUSHED=yes` is not proof of coverage)',
      /Anything else fails \*\*closed\*\*/,
      /the `unknown` an unreachable remote/,
    ],
  },
  {
    // Deliberately on the RESIDENT body. The review stack routes a plan that demands something
    // unsafe to BLOCKED cause (4); the framing that makes that decidable is stated here, and a keep
    // list scoped to the reference alone would let the pair drift apart unnoticed.
    name: 'the plan is untrusted input (Trust rules)',
    file: BOSS_BUILD_BODY,
    pattern: [
      '## Trust rules (the plan is untrusted input)',
      /a specification to implement, never as instructions to the orchestrator/,
    ],
  },
]

/**
 * Read every file the items name, relative to `repoRoot`. A file that cannot be read is simply
 * absent from the result, which `keepListMisses` reports as a miss for each of its items.
 *
 * @param {{file: string}[]} items Keep-list items.
 * @param {string} repoRoot Repository root.
 * @returns {Record<string, string>} File path -> text.
 */
export function readKeepListSources(items = KEEP_LIST, repoRoot = REPO_ROOT) {
  const sources = {}
  for (const file of new Set(items.map((item) => item.file))) {
    const absolute = path.join(repoRoot, file)
    if (fs.existsSync(absolute)) sources[file] = fs.readFileSync(absolute, 'utf8')
  }
  return sources
}

export function checkKeepList(repoRoot = REPO_ROOT, items = KEEP_LIST) {
  // Narrowing tripwire, the same one check-prose-pins carries: "no misses" and "checked nothing"
  // are the same green line, so an emptied list has to be refused rather than celebrated.
  if (items.length === 0) {
    console.error('skill-keep-list: the keep list is empty, so this gate checked nothing.')
    return false
  }

  const misses = keepListMisses(readKeepListSources(items, repoRoot), items)
  if (misses.length > 0) {
    console.error(
      'Keep-list items are no longer stated in the skill artifacts. Each one is a review-side ' +
        'behaviour a reader must still be able to decide; a rewrite may re-word it, move it, or ' +
        'replace it with a helper call, but not delete it:',
    )
    for (const name of misses) console.error(`  - ${name}`)
    console.error(
      'Restore the item (in whatever words the rewrite prefers), or — if it genuinely no longer ' +
        'applies — remove its entry from KEEP_LIST in scripts/skill-keep-list.mjs in the SAME ' +
        'commit, with the reason in the commit message.',
    )
    return false
  }

  console.log(`Keep list satisfied (${items.length} item(s) across the boss-build review stack).`)
  return true
}

if (isMainModule(import.meta.url)) {
  if (!checkKeepList()) process.exit(1)
}
