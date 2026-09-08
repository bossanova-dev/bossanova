#!/usr/bin/env node

// boss-build-keep-list — assert every load-bearing item on the BOS-1206 keep list is still stated
// in the boss-build payload, and say WHERE each one lives.
//
// Why this exists: BOS-1215 rewrites the procedural regions of the boss-build resident body from
// procedure to invariant. A prose rewrite can drop a behaviour without any gate noticing — that is
// the central risk of the change, and the only thing about a rewrite worth an executable check.
//
// Why a HELPER and not a pin. The obvious implementation is nine `assert.match` calls over
// `SKILL.md` in scripts/boss-build-skill.test.mjs. That is a prose pin, which
// scripts/check-prose-pins.mjs refuses by design (docs/skills/prose-pins.md): a regex over skill
// markdown makes the sentence permanent, because deleting it reds a test. Routing the same question
// through a helper whose VERDICT is asserted keeps the check while leaving the prose editable —
// the caller asserts what this program decides, not what any sentence says. Deliberately, the test
// that consumes it is named to match `check-prose-pins.mjs`'s own GATE_FILE_GLOB
// (`scripts/*skill*.test.mjs`) so it is scanned rather than exempt, and scores zero pins.
//
// WHERE EACH ITEM LIVES IS PART OF THE CHECK, not an implementation detail. The epic's keep list
// names nine items; they are not all resident, and pretending they were would make this gate green
// against a body that never carried them:
//
//   - Seven are resident in SKILL.md.
//   - "Step 12 notes capture" is NOT resident. It lives in references/finalize-and-stop.md as the
//     "post-terminal notes dispatch". Checking for it in the body would red on a payload that is
//     entirely correct.
//   - `gate-run.mjs` appears NOWHERE in the boss-build payload, and must not be added to it. It is
//     a repo-level instruction in this checkout's CLAUDE.md, and boss-build is a PUBLISHED core
//     that is extracted into every user's global skill directory, so it must stay project-agnostic
//     (CLAUDE.md, "`boss-*` skills are published globally"). It is recorded in ADJUDICATED below —
//     visible and reasoned, never silently dropped, and never satisfied by inventing prose.
//
// The probes are concept-level, not sentence-level: each names the behaviour it is protecting and
// matches the tokens that behaviour cannot be stated without. Whitespace is joined with `\s+`
// throughout, per docs/skills/README.md § Pinning skill prose, so reflowing a paragraph is not a
// behaviour change.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

const REPO_ROOT = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')

/** The checked-out payload source — the authoritative one inside this repository. */
export const DEFAULT_PAYLOAD_ROOT = 'services/boss/internal/skillinstall/skills/boss-build'

export const RESIDENT_BODY = 'SKILL.md'
export const FINALIZE_REFERENCE = 'references/finalize-and-stop.md'

/**
 * The keep list, mechanically checkable.
 *
 * Each item names the behaviour it protects, the payload file that must state it, and the probes
 * that decide it. EVERY probe must match for the item to count as present, so an item is a
 * conjunction: dropping any one clause of a multi-part rule reds it.
 */
export const KEEP_LIST = [
  {
    id: 'terminal-states',
    requirement: 'The run has four terminal states and no others.',
    file: RESIDENT_BODY,
    probes: [
      { name: 'four-terminal-states', pattern: /[Ff]our\s+terminal\s+states/ },
      { name: 'REVIEW_READY', pattern: /\bREVIEW_READY\b/ },
      { name: 'PARTIAL', pattern: /\bPARTIAL\b/ },
      { name: 'BLOCKED', pattern: /\bBLOCKED\b/ },
      { name: 'NO_CHANGE', pattern: /\bNO_CHANGE\b/ },
    ],
  },
  {
    id: 'blocked-causes',
    requirement:
      'BLOCKED is reachable for exactly four causes, and each of the four is named: red quality ' +
      'gates, an unpushable branch, a missing API-version bump or transform, an unsafe plan.',
    file: RESIDENT_BODY,
    probes: [
      { name: 'exactly-four-causes', pattern: /exactly\s+four\s+causes/ },
      { name: 'cause-red-gates', pattern: /quality\s+gates\s+are\s+red/ },
      { name: 'cause-unpushable', pattern: /branch\s+cannot\s+be\s+pushed/ },
      { name: 'cause-api-version', pattern: /API-?version\s+bump/ },
      { name: 'cause-unsafe-plan', pattern: /plan\s+demands\s+something\s+unsafe/ },
      // The exhaustiveness clause is the load-bearing half: without it the list reads as examples,
      // and open review findings drift back onto it. That regression is what BOS-1107 cost.
      { name: 'findings-are-not-a-cause', pattern: /open\s+review\s+findings\s+are\s+\*\*not\*\*/ },
    ],
  },
  {
    id: 'plan-untrusted',
    requirement:
      'The plan is external input: a specification to implement, never instructions to the ' +
      'orchestrator, and a plan demanding a workflow/secrets/remote/gate change is BLOCKED.',
    file: RESIDENT_BODY,
    probes: [
      { name: 'untrusted-input', pattern: /plan\s+is\s+untrusted\s+input/ },
      { name: 'never-instructions', pattern: /never\s+as\s+instructions/ },
      { name: 'blocked-condition', pattern: /that\s+is\s+a\s+BLOCKED\s+condition/ },
    ],
  },
  {
    id: 'tagless-commits-and-injection',
    requirement:
      'Subagents write tagless conventional commits; finalize injects `[#<PR>]` across the branch ' +
      'afterwards, so no subagent guesses a tag.',
    file: RESIDENT_BODY,
    probes: [
      { name: 'tagless-commits', pattern: /[Tt]agless\s+conventional\s+commits/ },
      { name: 'inject-pr-tag', pattern: /inject(?:s|ing)?\s+`?\[#<PR>\]`?/ },
      { name: 'no-tag-needed', pattern: /need\s+\*\*no\*\*\s+PR\s+tag|need\s+no\s+PR\s+tag/ },
    ],
  },
  {
    id: 'callbacks-over-polling',
    requirement:
      'Prefer a one-shot callback over blind polling, gated on the single `callbacksAvailable(env)` ' +
      'signal, degrading to the bounded fallback poll rather than a failed wait.',
    file: RESIDENT_BODY,
    probes: [
      { name: 'prefer-callback', pattern: /callback\s+over\s+blind\s+polling/ },
      { name: 'availability-gate', pattern: /callbacksAvailable\(env\)/ },
      { name: 'fallback-poll', pattern: /policy\.fallbackPoll/ },
    ],
  },
  {
    id: 'sentinel-verdict-routing',
    requirement:
      'The Step 6 review verdict routes through a run file, never the subagent’s prose, and a ' +
      'missing/stale/unmatchable sentinel is a dispatch-failure that is never read as clean.',
    file: RESIDENT_BODY,
    probes: [
      { name: 'run-file-not-prose', pattern: /never\s+the\s+subagent's\s+returned\s+prose/ },
      { name: 'clean-verdict', pattern: /bs-review\s+clean:/ },
      { name: 'capped-verdict', pattern: /bs-review\s+capped:/ },
      { name: 'dispatch-failure', pattern: /dispatch-failure/ },
      { name: 'never-clean', pattern: /NEVER\s+treated\s+as\s+clean|\*\*never\s+clean\*\*/ },
    ],
  },
  {
    id: 'api-version-hard-gate',
    requirement:
      'A missing required API-version bump or down-convert transform is a hard BLOCKED gate, ' +
      'decided by the configured API-compatibility lens role.',
    file: RESIDENT_BODY,
    probes: [
      {
        name: 'api-version-transform',
        pattern: /API-?version\s+bump\s+or\s+down-convert\s+transform/,
      },
      { name: 'lens-role', pattern: /API-compatibility\s+lens\s+role/ },
    ],
  },
  {
    id: 'step-12-notes-capture',
    // NOT resident. Checked where it actually lives; see the header note.
    requirement: 'A top-level run performs exactly one post-terminal notes dispatch, at Step 12.',
    file: FINALIZE_REFERENCE,
    probes: [
      { name: 'post-terminal-notes-dispatch', pattern: /post-terminal\s+notes\s+dispatch/ },
      { name: 'at-most-one', pattern: /at\s+most\s+one\s+post-terminal\s+notes\s+dispatch/ },
    ],
  },
]

/**
 * Keep-list items deliberately NOT checked against the payload, each with the reason.
 *
 * This array is why the gate can claim to cover the epic's list: an item that cannot be checked is
 * recorded here rather than dropped, so the difference between "checked and present" and "decided
 * not to check" is visible in the verdict rather than buried in a commit message.
 */
export const ADJUDICATED = [
  {
    id: 'gate-run-mjs',
    requirement: 'Long gates are launched and polled through `scripts/gate-run.mjs`.',
    decision: 'not-in-payload',
    reason:
      'Measured: `gate-run` appears nowhere in the boss-build payload, before this rewrite as ' +
      'after it. It is a repo-level instruction in this checkout CLAUDE.md, not skill prose. ' +
      'boss-build is a published core extracted into every user global skill directory, so ' +
      'naming a repo-local script in it would break the project-agnostic invariant that ' +
      'services/boss/internal/skillinstall/skills_manifest_test.go enforces. The rewrite ' +
      'therefore cannot remove it, and adding it to satisfy this list would be inventing prose.',
  },
]

export const RESIDUAL =
  'a green run means each keep-list probe matched somewhere in its named payload file — not that ' +
  'the surrounding prose is correct, not that the behaviour it names actually works, not that the ' +
  'probe matched the sentence the author intended rather than a passing mention elsewhere in the ' +
  'file, and not that an item recorded in ADJUDICATED was checked at all'

/**
 * Resolve every keep-list item against a payload tree.
 *
 * @param {object} [options]
 * @param {string} [options.payloadRoot] Absolute path to the boss-build payload directory.
 * @param {typeof KEEP_LIST} [options.keepList] Item set, for tests.
 * @returns {{ok: boolean, items: object[], adjudicated: object[]}}
 */
export function checkKeepList(options = {}) {
  const payloadRoot = options.payloadRoot || path.join(REPO_ROOT, DEFAULT_PAYLOAD_ROOT)
  const keepList = options.keepList || KEEP_LIST

  // Narrowing tripwire, the same shape check-prose-pins.mjs uses: this gate's success state is
  // "nothing missing", which is byte-identical to a run that looked at nothing.
  if (keepList.length === 0) {
    return {
      ok: false,
      items: [],
      adjudicated: ADJUDICATED,
      error: 'the keep list is empty, so this gate checked nothing',
    }
  }

  const cache = new Map()
  const readOnce = (file) => {
    if (!cache.has(file)) {
      const full = path.join(payloadRoot, ...file.split('/'))
      cache.set(file, fs.existsSync(full) ? fs.readFileSync(full, 'utf8') : null)
    }
    return cache.get(file)
  }

  const items = keepList.map((item) => {
    const source = readOnce(item.file)
    if (source === null) {
      return {
        ...item,
        status: 'missing',
        missing: ['<file absent>'],
        probeCount: item.probes.length,
      }
    }
    const missing = item.probes.filter((p) => !p.pattern.test(source)).map((p) => p.name)
    return {
      ...item,
      status: missing.length === 0 ? 'present' : 'missing',
      missing,
      probeCount: item.probes.length,
    }
  })

  return { ok: items.every((i) => i.status === 'present'), items, adjudicated: ADJUDICATED }
}

/**
 * Render the verdict a caller asserts.
 *
 * @param {ReturnType<typeof checkKeepList>} result
 * @returns {string}
 */
export function formatReport(result) {
  const lines = []
  if (result.error) {
    lines.push(`boss-build-keep-list: WIRING ERROR — ${result.error}`)
    return lines.join('\n')
  }

  const present = result.items.filter((i) => i.status === 'present').length
  const total = result.items.length

  if (result.ok) {
    lines.push(`boss-build-keep-list: OK ${present}/${total} keep-list items present.`)
  } else {
    lines.push(`boss-build-keep-list: MISSING ${total - present}/${total} keep-list items.`)
    for (const item of result.items.filter((i) => i.status === 'missing')) {
      lines.push(`  - MISSING ${item.id} (${item.file}) — probes: ${item.missing.join(', ')}`)
      lines.push(`      requirement: ${item.requirement}`)
    }
  }

  for (const item of result.items.filter((i) => i.status === 'present')) {
    lines.push(`  - present ${item.id} (${item.file}, ${item.probeCount} probes)`)
  }
  for (const item of result.adjudicated) {
    lines.push(`  - adjudicated ${item.id}: ${item.decision} — ${item.reason}`)
  }
  lines.push(`RESIDUAL: ${RESIDUAL}`)
  return lines.join('\n')
}

function main(argv) {
  const payloadIndex = argv.indexOf('--payload')
  const payloadRoot = payloadIndex === -1 ? undefined : path.resolve(argv[payloadIndex + 1])
  const result = checkKeepList({ payloadRoot })

  if (argv.includes('--json')) {
    const json = {
      ok: result.ok,
      error: result.error,
      items: result.items.map((i) => ({
        id: i.id,
        file: i.file,
        status: i.status,
        missing: i.missing,
      })),
      adjudicated: result.adjudicated.map((a) => ({ id: a.id, decision: a.decision })),
    }
    process.stdout.write(`${JSON.stringify(json, null, 2)}\n`)
  } else {
    const report = formatReport(result)
    if (result.ok) console.log(report)
    else console.error(report)
  }
  return result.ok ? 0 : 1
}

if (isMainModule(import.meta.url)) process.exit(main(process.argv.slice(2)))
