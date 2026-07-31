// Content/contract test for the bs-sweep-review skill (BOS-617).
//
// The "content-test file pattern" the bs-sweep-* family uses:
// scripts/bs-<skill>-skill.test.mjs, wired into `make test-smoke` via the
// `scripts/bs-*-skill.test.mjs` glob. It pins the byte-stable external contracts
// the SKILL.md documents — the dry-run default, the single-flight lock, the awaited
// triage dispatch, the one-stub-per-run invariant, the stub's tracker shape and its
// exact last-line marker — against BOTH the source and the generated `.codex`
// mirror, so an un-synced mirror or a silently weakened contract fails the build.
//
// Node built-ins only — cron worktrees are dependency-free.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, rmSync, writeFileSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { MARKER_PREFIX, REVIEW_BOT_LOGIN } from './sweep-review-gate.mjs'

const readSkill = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')
const SKILL = readSkill('../.claude/skills/bs-sweep-review/SKILL.md')
const CODEX = readSkill('../.codex/skills/bs-sweep-review/SKILL.md')

// The family-wide teardown sentence, asserted verbatim by every sibling skill test
// (see scripts/bs-sweep-security-skill.test.mjs).
const NOTES_TEARDOWN =
  "Before exiting, follow `bs-record-notes` with this run's outcome. Recording is non-fatal: never change the terminal state, exit code, or `git status --porcelain`. Skip gated/no-op runs that observed nothing."

const BOTH = [
  ['source', SKILL],
  ['codex mirror', CODEX],
]

/** Count non-overlapping occurrences of a literal substring. */
function countOf(haystack, needle) {
  let n = 0
  let i = 0
  for (;;) {
    const at = haystack.indexOf(needle, i)
    if (at === -1) return n
    n += 1
    i = at + needle.length
  }
}

function frontmatterValue(skill, key) {
  const match = skill.match(new RegExp(`^${key}:\\s*(.+)$`, 'm'))
  assert.ok(match, `skill frontmatter must declare ${key}`)
  return match[1]
}

function allowedTools(skill) {
  return frontmatterValue(skill, 'allowed-tools')
    .split(',')
    .map((t) => t.trim())
}

// ---------------------------------------------------------------------------
// Frontmatter — identity + the exact tool surface the phases need.
// ---------------------------------------------------------------------------

test('both copies declare the bs-sweep-review skill name', () => {
  for (const [label, skill] of BOTH) {
    assert.equal(frontmatterValue(skill, 'name'), 'bs-sweep-review', `${label} name drifted`)
  }
})

test('allowed-tools declares exactly the tools the phases need, incl. the tracker writes', () => {
  const expected = [
    'Bash',
    'Read',
    'Glob',
    'Grep',
    'Task',
    'Skill',
    'mcp__bossanova-linear__list_issues',
    'mcp__bossanova-linear__save_issue',
  ]
  for (const [label, skill] of BOTH) {
    const tools = allowedTools(skill)
    assert.deepEqual(tools, expected, `${label} allowed-tools drifted`)
  }
})

test('the tracker read + write tools are both declared (dedupe re-check + create)', () => {
  for (const [label, skill] of BOTH) {
    const tools = allowedTools(skill)
    assert.ok(tools.includes('mcp__bossanova-linear__list_issues'), `${label} needs list_issues`)
    assert.ok(tools.includes('mcp__bossanova-linear__save_issue'), `${label} needs save_issue`)
  }
})

// ---------------------------------------------------------------------------
// The durable --dry-run contract: default preview, --no-dry-run the ONLY enabler.
// ---------------------------------------------------------------------------

test('--dry-run is the default and makes zero writes', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('--dry-run'), `${label} must document the dry-run flag`)
    assert.match(skill, /zero.{0,60}writes/i, `${label} must state dry-run makes zero writes`)
    assert.match(skill, /--dry-run.{0,40}default|default.{0,40}--dry-run/is)
  }
})

test('--no-dry-run is the ONLY write enabler and Phase 3 is gated on it', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('--no-dry-run'), `${label} must document the write flag`)
    // The deterministic, order-independent flag resolution.
    assert.ok(skill.includes('DRY_RUN=true'), `${label} must default the shell flag to true`)
    assert.ok(skill.includes('--no-dry-run) DRY_RUN=false'), `${label} flag parse drifted`)
    // Phase 3 (the only write path) is wrapped in the false guard.
    assert.ok(skill.includes('if [ "$DRY_RUN" = "false" ]; then'), `${label} Phase 3 guard drifted`)
  }
})

test('git status --porcelain must be empty in BOTH modes (the skill never commits)', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('git status --porcelain'), `${label} clean-tree contract missing`)
    assert.match(skill, /empty.{0,40}both|both.{0,60}modes/is, `${label} must cover both modes`)
  }
})

// ---------------------------------------------------------------------------
// Single-flight lock + mandatory teardown.
// ---------------------------------------------------------------------------

test('the single-flight mkdir lock, stale reclaim and idempotent teardown are documented', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('bs-sweep-review.lock'), `${label} must name the lock dir`)
    assert.ok(
      skill.includes('BS_REVIEWSWEEP_LOCK_STALE_SECS'),
      `${label} stale-reclaim env missing`,
    )
    assert.ok(skill.includes('mkdir'), `${label} must use mkdir as the mutex`)
    assert.ok(skill.includes('teardown()'), `${label} must define an explicit teardown`)
    assert.match(skill, /exits? 0 without acting/i, `${label} concurrent-run behaviour missing`)
  }
})

test('teardown is mandatory on EVERY post-lock terminal path', () => {
  for (const [label, skill] of BOTH) {
    assert.match(skill, /Teardown \(MANDATORY, every path\)/i, `${label} Phase 5 heading drifted`)
    assert.match(
      skill,
      /every terminal path.{0,80}lock/is,
      `${label} must bind teardown to every post-lock terminal path`,
    )
  }
})

test('the non-fatal notes teardown sentence is byte-identical in both copies', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes(NOTES_TEARDOWN), `${label} must include the notes teardown contract`)
  }
})

// ---------------------------------------------------------------------------
// Triage dispatch — ONE awaited general-purpose subagent, never backgrounded.
// ---------------------------------------------------------------------------

test('triage runs in an awaited general-purpose subagent, never run_in_background', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(
      countOf(skill, 'subagent_type: general-purpose') >= 1,
      `${label} must name the general-purpose subagent type`,
    )
    assert.ok(countOf(skill, '<!-- tier: opus -->') >= 1, `${label} dispatch must be tier: opus`)
    assert.ok(skill.includes('run_in_background'), `${label} must state the never-background rule`)
    assert.match(
      skill,
      /never.{0,60}run_in_background|run_in_background.{0,60}never/is,
      `${label} must forbid run_in_background`,
    )
    assert.ok(skill.includes('await'), `${label} dispatches must say they are awaited`)
  }
})

test('the orchestrator never re-implements selection — it shells the pure-core gate', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(
      skill.includes('scripts/sweep-review-gate.mjs'),
      `${label} must resolve the gate core`,
    )
    assert.ok(skill.includes('selectStub'), `${label} must name the pure-core decision function`)
    assert.match(
      skill,
      /orchestrator, not a? ?decision engine/i,
      `${label} must state the orchestrator/decision split`,
    )
  }
})

// ---------------------------------------------------------------------------
// Scope — release PRs by BASE branch, bot-authored inline comments only.
// ---------------------------------------------------------------------------

test('scope is release PRs (base staging/production) and the review bot only', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('--base staging'), `${label} must scope by base staging`)
    assert.ok(skill.includes('--base production'), `${label} must scope by base production`)
    assert.ok(skill.includes(REVIEW_BOT_LOGIN), `${label} must scope to ${REVIEW_BOT_LOGIN}`)
    assert.match(skill, /base branch/i, `${label} must say the selector is the base branch`)
  }
})

// ---------------------------------------------------------------------------
// One stub per run, dedupe, and the never-retry rule.
// ---------------------------------------------------------------------------

test('exactly one stub per run is an invariant', () => {
  for (const [label, skill] of BOTH) {
    assert.match(skill, /one stub per run/i, `${label} must state the one-stub-per-run invariant`)
    assert.match(skill, /exactly \*\*one\*\*|exactly one/i, `${label} must say exactly one`)
    assert.match(
      skill,
      /NEVER create a second stub/i,
      `${label} must forbid a second stub in one run`,
    )
  }
})

test('an ambiguous create response is a BLOCKED stop, never a retry', () => {
  for (const [label, skill] of BOTH) {
    assert.match(
      skill,
      /ambiguous create[^.]{0,120}BLOCKED/is,
      `${label} must route an ambiguous create to BLOCKED`,
    )
    assert.match(skill, /never.{0,30}retry|not.{0,10}a retry/i, `${label} must forbid the retry`)
  }
})

test('the pre-create re-check queries the marker across ALL states incl. archived', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('includeArchived: true'), `${label} re-check must include archived`)
    assert.ok(
      skill.includes('mcp__bossanova-linear__list_issues'),
      `${label} must name the re-check tool`,
    )
    assert.match(skill, /re-check/i, `${label} must document the pre-create re-check`)
  }
})

test('a truncated dedupe snapshot is fail-closed BLOCKED, never triaged', () => {
  for (const [label, skill] of BOTH) {
    assert.match(skill, /fail-closed/i, `${label} must state the fail-closed rule`)
    assert.match(
      skill,
      /partial dedupe snapshot must never be triaged/i,
      `${label} must forbid triaging a partial snapshot`,
    )
  }
})

// ---------------------------------------------------------------------------
// The stub's tracker shape + its exact last-line marker.
// ---------------------------------------------------------------------------

test('the stub is Bossanova / Unplanned / agent-plan + bug, with no assignee or estimate', () => {
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('team: "Bossanova"'), `${label} team drifted`)
    assert.ok(skill.includes('state: "Unplanned"'), `${label} state drifted`)
    assert.ok(skill.includes('labels: ["agent-plan", "bug"]'), `${label} labels drifted`)
    assert.match(
      skill,
      /Do \*\*NOT\*\* set assignee \/ priority \/ project \/ estimate/i,
      `${label} must forbid setting assignee/priority/estimate`,
    )
    assert.match(skill, /never.{0,30}create labels|NEVER\s*\n?\s*create labels/i)
  }
})

test('the marker is the EXACT last line and matches the pure core prefix', () => {
  assert.equal(MARKER_PREFIX, 'Review: ')
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('Review: <path>@<line>'), `${label} marker template drifted`)
    assert.match(skill, /exact last line/i, `${label} must state the marker is the last line`)
    assert.match(skill, /\^Review: <path>@<line>\$/, `${label} line-anchored rule missing`)
  }
})

// ---------------------------------------------------------------------------
// Cron gate — fail-closed to SKIP, registered per copy at the right path.
// ---------------------------------------------------------------------------

test('the cron gate is registered and documented as fail-closed to SKIP', () => {
  assert.ok(
    SKILL.includes('node .claude/skills/bs-sweep-review/gate/gate.mjs'),
    'source must register the .claude gate path',
  )
  assert.ok(
    CODEX.includes('node .codex/skills/bs-sweep-review/gate/gate.mjs'),
    'codex mirror must register the mirrored gate path',
  )
  for (const [label, skill] of BOTH) {
    assert.match(
      skill,
      /fail-closed.{0,40}skip|skip.{0,40}fail-closed/is,
      `${label} gate must be fail-closed to skip`,
    )
    assert.match(skill, /zero.{0,30}agent tokens/i, `${label} must state the idle-cost property`)
  }
})

// ---------------------------------------------------------------------------
// Scratch-path plumbing. Every `process.env.X` a Phase snippet reads must be
// `export`ed by an earlier shell block in the SAME file, or the snippet gets
// `undefined` and the run BLOCKED-stops on its first invocation — a total,
// silent-until-fired outage that no other assertion here would catch. Ambient
// vars the cron harness injects (docs/cron.md) are the only exemptions.
// ---------------------------------------------------------------------------

// Injected by the daemon/cron harness, never exported by the skill itself.
const AMBIENT_ENV = new Set(['LINEAR_API_KEY', 'TMPDIR', 'REPO_DIR', 'WORKTREE_DIR'])

test('every process.env var a snippet reads is exported by the skill (or ambient)', () => {
  for (const [label, skill] of BOTH) {
    const read = new Set(
      [...skill.matchAll(/process\.env\.([A-Za-z_][A-Za-z0-9_]*)/g)].map((m) => m[1]),
    )
    const exported = new Set(
      [...skill.matchAll(/^export ([A-Za-z_][A-Za-z0-9_ ]*)$/gm)].flatMap((m) =>
        m[1].split(/\s+/).filter(Boolean),
      ),
    )
    assert.ok(read.size > 0, `${label} must read at least one env var`)
    for (const name of read) {
      if (AMBIENT_ENV.has(name)) continue
      assert.ok(
        exported.has(name),
        `${label} reads process.env.${name} but never exports it — the snippet would ` +
          `receive undefined and BLOCKED-stop every run`,
      )
    }
  }
})

// ---------------------------------------------------------------------------
// Executable-shape gates. The phases are copied out of this file and run as one
// bash session, so a fence that swallows the block below it, or a block that does
// not parse, is a SILENT total outage: `read_sel` never gets defined, `HAS_TOP`
// stays empty, Phase 3 reports "nothing to create" and exits 0 — while the dry-run
// path (which re-reads $SEL_FILE directly) still looks green. Prettier, the codex
// mirror check and substring assertions are all blind to it, so it is gated here.
// ---------------------------------------------------------------------------

/** Fenced blocks with the given info string, per CommonMark fence-length rules. */
function fencedBlocks(skill, infoString) {
  const out = []
  let fence = null
  let info = ''
  let buf = []
  for (const line of skill.split('\n')) {
    const m = line.match(/^(`{3,})(.*)$/)
    if (fence === null) {
      if (m) {
        fence = m[1]
        info = m[2].trim()
        buf = []
      }
      continue
    }
    if (m && m[1].length >= fence.length && m[2].trim() === '') {
      if (info === infoString) out.push(buf.join('\n'))
      fence = null
      continue
    }
    buf.push(line)
  }
  assert.equal(fence, null, 'unterminated code fence')
  return out
}

test('no code fence is wider than three backticks (a wide fence swallows the next block)', () => {
  for (const [label, skill] of BOTH) {
    const wide = skill.split('\n').filter((l) => /^`{4,}/.test(l))
    assert.deepEqual(wide, [], `${label} has a >3-backtick fence: ${wide.join(' | ')}`)
  }
})

test('every ```bash block parses as bash (bash -n) — the phases are run as one script', () => {
  const scratch = join(tmpdir(), `bs-sweep-review-blk-${process.pid}.sh`)
  try {
    for (const [label, skill] of BOTH) {
      const blocks = fencedBlocks(skill, 'bash')
      // Exact, not a floor: an unclosed fence swallows the block after it, which
      // shows up here as a DROPPED block long before it shows up as a broken run.
      assert.equal(blocks.length, 11, `${label} bash-block count drifted`)
      blocks.forEach((body, i) => {
        writeFileSync(scratch, body)
        try {
          execFileSync('bash', ['-n', scratch], { stdio: 'pipe' })
        } catch (err) {
          assert.fail(
            `${label} bash block #${i + 1} does not parse: ${String(err.stderr ?? err).trim()}`,
          )
        }
      })
    }
  } finally {
    rmSync(scratch, { force: true })
  }
})

test('Phase 3 can actually obtain the fields it writes — read_sel + a verbatim body file', () => {
  // Phase 2 emitting only HAS_TOP would leave Phase 3 with no documented way to
  // reach toFile.marker / .title / .body, forcing hand-transcription of multi-KB
  // markdown in the one step the skill forbids improvising (`used **as-is**`).
  for (const [label, skill] of BOTH) {
    assert.ok(skill.includes('read_sel()'), `${label} must define the compact read_sel accessor`)
    for (const field of ['marker', 'title', 'key', 'hasTop']) {
      assert.match(
        skill,
        new RegExp(`^\\s*${field}:`, 'm'),
        `${label} read_sel must surface ${field}`,
      )
    }
    // Phase 3's list_issues/save_issue are MCP calls made OUTSIDE the bash session
    // and shell state does not persist across command blocks, so the scalars must be
    // PRINTED to reach the orchestrator — capturing them in $SELJSON is not enough.
    assert.match(
      skill,
      /printf 'selection: %s\\n' "\$SELJSON"/,
      `${label} must print the selection blob; a captured-only $SELJSON never reaches Phase 3`,
    )
    assert.ok(skill.includes('BODY_FILE'), `${label} must route the body through its own file`)
    assert.match(
      skill,
      /exact.{0,40}contents of `\$BODY_FILE`/is,
      `${label} must bind the description to $BODY_FILE verbatim`,
    )
  }
})

test('every $TMPDIR scratch file the phases write is removed by teardown', () => {
  for (const [label, skill] of BOTH) {
    const written = new Set(
      [...skill.matchAll(/\$\{TMPDIR:-\/tmp\}\/(bs-sweep-review-[A-Za-z0-9._-]+)/g)].map(
        (m) => m[1],
      ),
    )
    assert.ok(written.size >= 4, `${label} should declare the scratch set (found ${written.size})`)
    const teardown = skill.slice(skill.indexOf('teardown() {'), skill.indexOf('release_lock\n}'))
    assert.ok(teardown.length > 0, `${label} must define teardown()`)
    for (const file of written) {
      assert.ok(teardown.includes(file), `${label} teardown leaks ${file}`)
    }
  }
})

// ---------------------------------------------------------------------------
// Anchor-key semantics — the deliberate, documented trade-offs.
// ---------------------------------------------------------------------------

test('the anchor dedupe key and its two deliberate consequences are documented', () => {
  for (const [label, skill] of BOTH) {
    assert.match(
      skill,
      /same finding[^.]{0,120}both release PRs|both release PRs[^.]{0,120}one/is,
      `${label} must document the cross-PR collapse`,
    )
    assert.match(
      skill,
      /same anchor|one anchor/i,
      `${label} must document the same-anchor collision`,
    )
    assert.match(
      skill,
      /all.{0,30}comments|every comment/i,
      `${label} must say all comments render`,
    )
  }
})
