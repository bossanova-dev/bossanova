// Content/contract test for the repo-local bs-sweep-notes skill (BOS-607).
//
// The execution logic lives in the skill; this test pins the deployable
// contracts: both skill frontmatters, generated-mirror synchronization, the
// documented cron cadence branch, and identical fixture-capable gate probes.

import { test } from 'node:test'
import assert from 'node:assert/strict'
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { spawnSync } from 'node:child_process'

const read = (rel) => readFileSync(new URL(rel, import.meta.url), 'utf8')

const SKILL = read('../.claude/skills/bs-sweep-notes/SKILL.md')
const CODEX = read('../.codex/skills/bs-sweep-notes/SKILL.md')
const GATE = read('../.claude/skills/bs-sweep-notes/gate/gate.mjs')
const CODEX_GATE = read('../.codex/skills/bs-sweep-notes/gate/gate.mjs')

const CRON_NAME = 'Bossanova sweep notes'
const CLAUDE_GATE = 'node .claude/skills/bs-sweep-notes/gate/gate.mjs'
const CODEX_GATE_COMMAND = 'node .codex/skills/bs-sweep-notes/gate/gate.mjs'

function generatedMirror(source) {
  return source
    .replace(
      /^---\n([\s\S]*?)\n---\n/,
      '---\n$1\n---\n\n<!-- Generated from .claude/skills by make codex-skills. Do not edit directly. -->\n',
    )
    .replaceAll('.claude/skills/bs-sweep-notes', '.codex/skills/bs-sweep-notes')
}

test('both skill frontmatters name bs-sweep-notes and describe it', () => {
  for (const [label, skill] of [
    ['.claude', SKILL],
    ['.codex', CODEX],
  ]) {
    assert.match(skill, /^name:\s*bs-sweep-notes\s*$/m, `${label} name`)
    assert.match(skill, /^description:\s*.+/m, `${label} description`)
  }
})

test('generated Codex skill mirrors source with only the documented path rewrite', () => {
  assert.equal(CODEX, generatedMirror(SKILL))
})

test('complete argv resolution makes either explicit write-flag order write mode', () => {
  const match = SKILL.match(
    /Resolve\s+the\s+flags\s+before\s+reading\s+anything:\n\n```bash\n([\s\S]*?)```/,
  )
  assert.ok(match, 'source skill must contain an executable flag resolver')
  const resolve = (...args) =>
    spawnSync('bash', ['-c', `${match[1]}\nprintf '%s' "$WRITE"`, '--', ...args], {
      encoding: 'utf8',
    })

  assert.equal(resolve().stdout, 'false')
  assert.equal(resolve('--dry-run').stdout, 'false')
  assert.equal(resolve('--no-dry-run').stdout, 'true')
  assert.equal(resolve('--no-dry-run', '--dry-run').stdout, 'true')
  assert.equal(resolve('--dry-run', '--no-dry-run').stdout, 'true')
  assert.equal(resolve('--unexpected').status, 2)
})

test('snapshot validation rejects malformed notes and duplicate deletion IDs', () => {
  const match = SKILL.match(
    /boss[ ]notes[ ]ls --tag[ ]improvement --json --limit[ ]0 >"\$RUN_DIR\/notes\.json"\nnode -e '([^']+)'/,
  )
  assert.ok(match, 'source skill must contain executable snapshot validation')
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-snapshot-'))
  let fixtureNumber = 0
  const run = (notes) => {
    const notesPath = join(dir, `${fixtureNumber++}.json`)
    const idsPath = join(dir, `${fixtureNumber++}.ids`)
    writeFileSync(notesPath, JSON.stringify(notes))
    return spawnSync(process.execPath, ['-e', match[1], notesPath, idsPath], {
      encoding: 'utf8',
    })
  }
  const valid = {
    id: 'note-1',
    body: 'A problem',
    created_at: '2026-07-29T00:00:00Z',
    tags: ['improvement'],
  }
  try {
    assert.equal(run([valid]).status, 0)
    assert.notEqual(run([{}]).status, 0)
    assert.notEqual(run([{ ...valid, id: '' }]).status, 0)
    assert.notEqual(run([{ ...valid, body: null }]).status, 0)
    assert.notEqual(run([{ ...valid, created_at: null }]).status, 0)
    assert.notEqual(run([{ ...valid, tags: ['other'] }]).status, 0)
    assert.notEqual(run([valid, { ...valid }]).status, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('weekly cron registration records the no-gate cadence branch and names the probe', () => {
  for (const [label, skill, command] of [
    ['.claude', SKILL, CLAUDE_GATE],
    ['.codex', CODEX, CODEX_GATE_COMMAND],
  ]) {
    assert.ok(skill.includes(CRON_NAME), `${label} must use one cron name`)
    assert.ok(skill.includes("--schedule '@weekly'"), `${label} must use exact weekly cadence`)
    assert.ok(
      skill.includes('/bs-sweep-notes --no-dry-run'),
      `${label} must enter write mode explicitly`,
    )
    assert.ok(skill.includes(command), `${label} must document its local gate probe`)
    assert.match(
      skill,
      /GateCommand.*empty|no-gate\s+weekly\s+cadence|without\s+a\s+gate/i,
      `${label} must state the no-gate branch actually selected`,
    )
  }
})

test('production triage pins Linear injection and the full gate pipeline', () => {
  assert.ok(SKILL.includes('const { linearRequest } = await import(process.env.LIB)'))
  assert.ok(SKILL.includes('fetchMarkedLinearIssues({'))
  assert.ok(SKILL.includes('node "$GATE" cluster "$NOTES_JSON" >"$CLUSTERS_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" merge "$CLUSTERS_JSON" "$GROUPS_JSON" >"$MERGED_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" rank "$MERGED_JSON" >"$RANKED_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" stale "$RANKED_JSON" >"$SIGNALS_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" verdicts "$RANKED_JSON" "$VERDICTS_JSON"'))
  assert.ok(SKILL.includes('node "$GATE" select "$LIVE_JSON" "$LINEAR_JSON" >"$SEL_FILE"'))
  // The cap must NOT be passed as a shell expansion: the parent shell would
  // expand it before a VAR=n prefix reaches the gate, silently yielding the default.
  assert.doesNotMatch(SKILL, /select[^\n]*BS_SWEEP_NOTES_MAX_ISSUES/)
  assert.match(SKILL, /read\s+from `BS_SWEEP_NOTES_MAX_ISSUES` inside\s+the\s+gate\s+process/)
  assert.match(SKILL, /read\s+from `BS_SWEEP_NOTES_STALE_DAYS` inside\s+the\s+gate\s+process/)
  assert.match(SKILL, /defaulting\s+to\s+30\s+days/)
  assert.doesNotMatch(SKILL, /select[^\n]*BS_SWEEP_NOTES_STALE_DAYS/)
  assert.match(SKILL, /[Ee]very\s+mechanical\s+cluster\s+key\s+must\s+appear\s+exactly\s+once/)
  assert.match(
    SKILL,
    /If\s+either\s+dispatch\s+errors, stop\s+with\s+no\s+writes, deletions\s+or\s+retags/,
  )
  assert.doesNotMatch(SKILL, /dispatch.*inline\s+fallback/i)
})

test('terminal cleanliness is attributed to this run instead of requiring a globally clean tree', () => {
  assert.match(
    SKILL,
    /The\s+sweep's\s+own\s+write\s+surface\s+is\s+the\s+scratch\s+directory\s+\(`RUN_DIR`, outside\s+the\s+worktree\)\s+and\s+the\s+repo-scoped\s+lock\s+directory\s+under\s+the\s+git\s+common\s+directory/,
  )
  assert.match(
    SKILL,
    /if\s+a\s+future\s+change\s+adds\s+another\s+write\s+target,\s+update\s+this\s+list\s+and\s+the\s+Phase\s+5\s+attribution\s+rule\s+in\s+the\s+same\s+change/,
  )
  assert.doesNotMatch(SKILL, /BLOCKED:\s+worktree\s+is\s+dirty/)
  assert.match(
    SKILL,
    /git\s+status\s+--porcelain\s+>"\$STATUS_BASELINE"/,
    'Phase 0 must capture a porcelain baseline',
  )
  assert.match(
    SKILL,
    /if\s+\[\s+-s\s+"\$STATUS_BASELINE"\s+\];\s+then[\s\S]{0,120}worktree\s+baseline\s+observed/,
    'Phase 0 must report non-empty baseline context without blocking',
  )
  assert.match(
    SKILL,
    /LC_ALL=C\s+comm\s+-23\s+"\$STATUS_AFTER_SORTED"\s+"\$STATUS_BASELINE_SORTED"\s+>"\$STATUS_NEW"/,
    'Phase 5 must compute records added since baseline',
  )
  assert.match(
    SKILL,
    /LC_ALL=C\s+comm\s+-13\s+"\$STATUS_AFTER_SORTED"\s+"\$STATUS_BASELINE_SORTED"\s+>"\$STATUS_DISAPPEARED"/,
    'Phase 5 must compute baseline records that disappeared',
  )
  assert.match(
    SKILL,
    /foreign\s+writer\s+delta\s+\(non-fatal\)/,
    'Phase 5 must label non-attributable deltas as foreign writers',
  )
  assert.match(
    SKILL,
    /baseline\s+entries\s+disappeared\s+\(non-fatal\)/,
    'Phase 5 must not fail on baseline entries that disappeared',
  )
  assert.match(
    SKILL,
    /Compare\s+whole\s+porcelain\s+records\s+as\s+opaque\s+lines/,
    'Phase 5 must avoid hand-parsing porcelain paths',
  )
  assert.match(
    SKILL,
    /unclassifiable\s+status\s+delta\s+is\s+attributed\s+to\s+this\s+run\s+and\s+fails/,
    'Phase 5 must fail closed when attribution is uncertain',
  )
  assert.match(
    SKILL,
    /\*\*Terminal\s+cleanup\.\*\*[\s\S]{0,240}leaves\s+no\s+residue\s+of\s+this\s+run/,
  )
})

test('theming allows a cross-target merge and bounds the one authored field', () => {
  assert.match(SKILL, /Group\s+by\s+shared\s+problem, not\s+shared\s+file/)
  assert.match(SKILL, /members\s+may\s+span\s+different `Where:` targets/)
  assert.match(SKILL, /is\s+the \*\*only\*\* field\s+the\s+subagent\s+may\s+author/)
  assert.match(SKILL, /≤200\s+characters/)
  assert.match(
    SKILL,
    /must\s+not\s+invent\s+or\s+edit\s+keys, re-rank, summarize\s+fields, or\s+drop\s+fields/,
  )
  // Selection must be ranked, not alphabetical by problem statement.
  assert.match(SKILL, /ranking\s+by\s+note\s+count, then\s+by\s+oldest\s+note, then\s+by\s+key/)
})

test('the currency pass covers every theme and defaults to live', () => {
  assert.match(
    SKILL,
    /over \*\*every\*\* theme, not\s+only\s+those\s+that\s+would\s+fit\s+under\s+the\s+cap/,
  )
  for (const verdict of ['live', 'fixed', 'unverifiable']) {
    assert.ok(SKILL.includes(`\`${verdict}\``), `verdict ${verdict} must be documented`)
  }
  assert.match(SKILL, /Requires `evidence` naming\s+the\s+file\s+and\s+line/)
  assert.match(SKILL, /`live` is\s+the\s+safe\s+default/)
  // Signals must never be readable as a verdict on their own.
  assert.match(
    SKILL,
    /Read `missing` and `changedSince`[\s\S]{0,40}as\s+leads, never\s+as\s+answers/,
  )
  assert.match(SKILL, /\*\*Signals\s+are\s+not\s+verdicts\.\*\*/)
})

test('write mode files one epic parent before any child', () => {
  assert.match(SKILL, /create\s+the\s+epic\s+parent \*\*first\*\*/)
  assert.ok(SKILL.includes('`labels: ["epic"]`'))
  assert.match(SKILL, /deliberately \*\*not\*\* `agent-plan`/)
  assert.match(
    SKILL,
    /If\s+the\s+parent\s+create\s+fails\s+or\s+is\s+ambiguous, create\s+no\s+children/,
  )
  assert.match(SKILL, /`EPIC_MAX_CHILDREN` is\s+not\s+in\s+this\s+code\s+path/)
  assert.ok(SKILL.includes('`parentId`: the epic parent'))
  assert.ok(SKILL.includes('`team: "Bossanova"`'))
  assert.ok(SKILL.includes('`state: "Unplanned"`'))
  assert.ok(SKILL.includes('`labels: ["agent-plan"]`'))
  assert.ok(SKILL.includes('`title`: `cluster.title`'))
  assert.match(SKILL, /failed\s+or\s+ambiguous\s+create\s+stops\s+further\s+writes/)
  // The pre-create recheck must be a complete snapshot scanned locally, never a
  // fuzzy list_issues query: that query returns unmarked issues AND truncated
  // descriptions, which makes the exact line-anchored match impossible to evaluate.
  assert.ok(SKILL.includes('>"$RECHECK_JSON"'))
  assert.match(SKILL, /re-fetch\s+the \*\*complete\*\* marker\s+snapshot\s+once/)
  assert.match(SKILL, /Do \*\*not\*\* use `mcp__bossanova-linear__list_issues` for\s+this/)
  assert.match(SKILL, /returns\s+each `description` \*\*truncated\*\*/)
  assert.match(SKILL, /every `cluster\.sourceKeys` alias/)
  assert.match(SKILL, /If\s+the\s+re-fetch\s+fails, create\s+nothing\s+at\s+all/)
  // The tool is no longer used anywhere, so it must not stay in allowed-tools.
  assert.doesNotMatch(SKILL, /allowed-tools:.*list_issues/)
})

test('a live theme can still retire individually fixed member notes', () => {
  assert.ok(SKILL.includes('"fixedNotes": ["<note-id>"]'))
  assert.match(SKILL, /optional\s+on \*\*any\*\* verdict/)
  assert.match(
    SKILL,
    /A\s+broad\s+theme\s+is\s+almost\s+never\s+wholly `fixed`,\s+because\s+one\s+live\s+sibling/,
  )
  assert.match(SKILL, /ids\s+must\s+belong\s+to\s+this\s+theme/)
  assert.match(SKILL, /a\s+non-empty\s+list\s+requires `evidence`/)
  // The retirement set must come from the gate, not from prose unioning buckets.
  assert.ok(SKILL.includes('node "$GATE" retired "$BUCKETS_JSON"'))
  assert.match(SKILL, /rather\s+than\s+unioning\s+buckets\s+by\s+hand/)
  assert.ok(SKILL.includes('node "$GATE" retired "$BUCKETS_JSON" "$SEL_FILE"'))
  assert.match(
    SKILL,
    /can\s+be\s+both\s+filed\s+as\s+a\s+child\s+and\s+have\s+some\s+of\s+its\s+notes\s+retired/,
  )
})

test('every child carries its verbatim source notes, and deletion depends on it', () => {
  assert.ok(SKILL.includes('`title`: exactly `Source notes (<issue-id>)`'))
  assert.match(SKILL, /every\s+member\s+note's \*\*unmodified\*\* `body`/)
  // Deleting the notes is only safe once the evidence exists on the ticket.
  assert.match(SKILL, /\*\*The\s+attachment\s+is\s+the\s+precondition\s+for\s+deletion\.\*\*/)
  assert.match(SKILL, /a\s+theme\s+whose\s+attachment\s+failed\s+keeps\s+its\s+notes/)
  assert.match(SKILL, /Never\s+infer\s+the\s+attachment\s+from\s+the\s+create\s+response/)
  // attachmentCreate mints a new row per call, so a resume must not duplicate.
  assert.match(SKILL, /`attachmentCreate` is \*\*not\s+idempotent\*\*/)
  // A bare note id in a filed description is a dead reference after Phase 4.
  assert.match(SKILL, /Never\s+cite\s+a\s+bare\s+note\s+id\s+as\s+if\s+it\s+were\s+a\s+lookup/)
  // The tools must be declared.
  assert.match(SKILL, /allowed-tools:.*prepare_attachment_upload/)
  assert.match(SKILL, /allowed-tools:.*create_attachment_from_upload/)
})

test('fixed themes are retagged rather than deleted, and nothing else is touched', () => {
  assert.ok(SKILL.includes('boss notes edit <id> --tag stale'))
  assert.match(SKILL, /`--tag` REPLACES\s+the\s+whole\s+tag\s+set/)
  assert.match(SKILL, /Deletion\s+is\s+not\s+used\s+here/)
  assert.match(SKILL, /`boss\s+notes\s+rm` is\s+permanent/)
  assert.match(SKILL, /Expired\s+themes\s+are\s+retagged\s+`stale`,\s+never\s+deleted/)
  assert.match(SKILL, /Do\s+not\s+use `boss\s+notes\s+rm` on\s+an\s+expired\s+note/)
  assert.match(SKILL, /Every\s+id\s+must\s+appear\s+in `snapshot-ids`/)
  assert.match(SKILL, /every\s+initial `dropped` theme\s+whose\s+reason\s+is `already-tracked`/)
  // Deliberately narrower than the old blanket rule: an `unverifiable` theme may
  // still retire a member the gate named, so the guard is per-note, not per-bucket.
  assert.match(
    SKILL,
    /Never\s+touch\s+a `deferred` theme, nor\s+any\s+note\s+the\s+gate\s+did\s+not\s+name/,
  )
  // Delete and retag sets overlap heavily; precedence must be stated, not left to loop order.
  assert.match(
    SKILL,
    /\*\*Deletion\s+wins\*\*: subtract\s+the\s+delete\s+set\s+above\s+before\s+retagging/,
  )
})

test('selection and reporting document expiry and convergence arithmetic', () => {
  assert.match(SKILL, /fresh\s+JSON\s+with\s+exactly\s+those\s+four\s+array\s+buckets/)
  assert.match(SKILL, /`selected`, `deferred` \(`over-cap`\), `dropped`[\s\S]{0,80}`expired`/)
  assert.match(SKILL, /`already-tracked` wins\s+before\s+expiry/)
  assert.match(SKILL, /expiry\s+wins\s+before\s+the\s+cap/)
  assert.match(SKILL, /newest\s+parseable\s+note\s+timestamp\s+is\s+older/)
  assert.match(SKILL, /no\s+parseable\s+timestamp\s+remains\s+live/)
  for (const pattern of [
    /snapshot\s+window\s+in\s+days/,
    /arrival\s+rate\s+over\s+that\s+window/,
    /cap\s+in\s+force/,
    /implied\s+drain\s+rate/,
    /expired\s+count/,
    /whether\s+the\s+backlog\s+converges/,
  ]) {
    assert.match(SKILL, pattern)
  }
  assert.match(SKILL, /rate\s+is\s+not\s+computable/)
  assert.match(SKILL, /rather\s+than\s+fabricating\s+a\s+divide-by-zero\s+rate/)
})

test('both gate mirrors carry the same cron name and fixture-capable fail-closed probe', () => {
  assert.equal(GATE, CODEX_GATE, 'gate files must be byte-identical')
  assert.ok(GATE.includes(`const CRON_NAME = '${CRON_NAME}'`), 'gate cron name matches SKILL')
  assert.ok(GATE.includes('BS_SWEEP_NOTES_FIXTURE'), 'gate supports deterministic fixture runs')
  assert.match(GATE, /fail-closed/i, 'gate documents fail-closed behavior')
})

test('fixture gate accepts only object entries with an improvement tag', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-gate-'))
  const gate = new URL('../.claude/skills/bs-sweep-notes/gate/gate.mjs', import.meta.url)
  let fixtureNumber = 0
  const run = (fixture) => {
    const path = join(dir, `${fixtureNumber++}.json`)
    writeFileSync(path, JSON.stringify(fixture))
    return spawnSync(process.execPath, [gate.pathname], {
      env: { ...process.env, BS_SWEEP_NOTES_FIXTURE: path },
      encoding: 'utf8',
    })
  }
  try {
    assert.equal(run({ notes: [{ id: 'note-1', tags: ['improvement'] }] }).status, 0)
    assert.equal(run([{ id: 'note-raw', tags: ['improvement'] }]).status, 0)
    assert.notEqual(run({ notes: [{ id: 'note-2', tags: ['other'] }] }).status, 0)
    assert.notEqual(run({ notes: [{ id: 'missing-tags' }] }).status, 0)
    assert.notEqual(run({ notes: [null] }).status, 0)
    assert.notEqual(run({ notes: [['not', 'a', 'note']] }).status, 0)
    assert.notEqual(run({ notes: [{ id: '', tags: ['improvement'] }] }).status, 0)
    assert.notEqual(
      run({
        notes: [
          { id: 'valid', tags: ['improvement'] },
          { id: 'malformed', tags: ['other'] },
        ],
      }).status,
      0,
    )
    assert.notEqual(run({ notes: 'not-an-array' }).status, 0)
    assert.notEqual(run(null).status, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate fails closed when BOSS_BIN returns malformed note entries', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-live-gate-'))
  const gate = new URL('../.claude/skills/bs-sweep-notes/gate/gate.mjs', import.meta.url)
  const fakeBoss = join(dir, 'boss')
  const run = (output) => {
    writeFileSync(fakeBoss, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
    chmodSync(fakeBoss, 0o755)
    return spawnSync(process.execPath, [gate.pathname], {
      env: { ...process.env, BOSS_BIN: fakeBoss },
      encoding: 'utf8',
    })
  }
  try {
    assert.notEqual(run('[null]').status, 0)
    assert.notEqual(run('[{}]').status, 0)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

// BOS-788: the gate must resolve the `boss` binary before shelling out, so a
// stale $BOSS_BIN (a cron session's context prompt naming an already-deleted
// sibling worktree) falls through to the next candidate instead of dying with an
// opaque ENOENT that a run reads as "capability unavailable".
//
// Each case injects BOSS_BIN and PATH and runs from a cwd outside this checkout,
// so no case depends on the machine's real `boss` install or on ./bin/boss
// happening to exist here. The gate is invoked by absolute path, so its own
// relative imports still resolve from import.meta.url.
const GATE_PATH = new URL('../.claude/skills/bs-sweep-notes/gate/gate.mjs', import.meta.url)
  .pathname
const HEALTHY_NOTES = '[{"id":"note-1","tags":["improvement"]}]'

function writeFakeBoss(dir, output = HEALTHY_NOTES) {
  const fake = join(dir, 'boss')
  writeFileSync(fake, `#!/bin/sh\nprintf '%s\\n' '${output}'\n`)
  chmodSync(fake, 0o755)
  return fake
}

// A cwd with no ancestor `.git`, so the resolver's ./bin/boss arm cannot reach
// this repo's build and every case is decided by BOSS_BIN and PATH alone.
function runGate({ bossBin, path: pathEntries, cwd }) {
  return spawnSync(process.execPath, [GATE_PATH], {
    cwd,
    env: { BOSS_BIN: bossBin, PATH: pathEntries },
    encoding: 'utf8',
  })
}

test('live gate succeeds when BOSS_BIN names a healthy boss binary', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-boss-ok-'))
  try {
    const result = runGate({ bossBin: writeFakeBoss(dir), path: '', cwd: dir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate reports the resolver reason when no boss binary resolves at all', () => {
  const dir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-boss-missing-'))
  const stale = join(dir, 'gone', 'bin', 'boss')
  try {
    const result = runGate({ bossBin: stale, path: '', cwd: dir })
    assert.notEqual(result.status, 0)
    // The resolver's own `reason`, not a bare spawn failure: a run that reads
    // ENOENT as "capability unavailable" is the bug this test pins.
    assert.match(result.stderr, /no\s+usable\s+boss\s+executable/)
    assert.ok(result.stderr.includes(stale), `reason must name BOSS_BIN: ${result.stderr}`)
    assert.doesNotMatch(result.stderr, /ENOENT|spawnSync/)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

test('live gate falls back to PATH when BOSS_BIN names a deleted worktree', () => {
  const pathDir = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-boss-path-'))
  // The reported scenario: BOSS_BIN was exported by a sibling worktree that has
  // since been reaped, while a healthy `boss` is still reachable another way.
  const reaped = mkdtempSync(join(tmpdir(), 'bs-sweep-notes-reaped-worktree-'))
  const stale = join(reaped, 'bin', 'boss')
  rmSync(reaped, { recursive: true, force: true })
  try {
    writeFakeBoss(pathDir)
    const result = runGate({ bossBin: stale, path: pathDir, cwd: pathDir })
    assert.equal(result.status, 0, result.stderr)
  } finally {
    rmSync(pathDir, { recursive: true, force: true })
  }
})
