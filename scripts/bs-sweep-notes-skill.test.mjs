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
  const match = SKILL.match(/Resolve the flags before reading anything:\n\n```bash\n([\s\S]*?)```/)
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
    /boss notes ls --tag improvement --json --limit 0 >"\$RUN_DIR\/notes\.json"\nnode -e '([^']+)'/,
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
      /GateCommand.*empty|no-gate weekly cadence|without a gate/i,
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
  assert.match(SKILL, /read from `BS_SWEEP_NOTES_MAX_ISSUES` inside the gate process/)
  assert.match(SKILL, /[Ee]very mechanical cluster key\s+must appear exactly once/)
  assert.match(SKILL, /If either dispatch errors, stop with no writes, deletions or retags/)
  assert.doesNotMatch(SKILL, /dispatch.*inline fallback/i)
})

test('theming allows a cross-target merge and bounds the one authored field', () => {
  assert.match(SKILL, /Group by shared problem, not shared file/)
  assert.match(SKILL, /members may span different `Where:` targets/)
  assert.match(SKILL, /is the \*\*only\*\* field the subagent\s+may author/)
  assert.match(SKILL, /≤200 characters/)
  assert.match(SKILL, /must not invent or edit keys, re-rank, summarize fields, or drop\s+fields/)
  // Selection must be ranked, not alphabetical by problem statement.
  assert.match(SKILL, /ranking by note count, then by oldest note, then by key/)
})

test('the currency pass covers every theme and defaults to live', () => {
  assert.match(SKILL, /over \*\*every\*\* theme, not only those that\s+would fit under the cap/)
  for (const verdict of ['live', 'fixed', 'unverifiable']) {
    assert.ok(SKILL.includes(`\`${verdict}\``), `verdict ${verdict} must be documented`)
  }
  assert.match(SKILL, /Requires `evidence` naming the file and line/)
  assert.match(SKILL, /`live` is the safe default/)
  // Signals must never be readable as a verdict on their own.
  assert.match(SKILL, /Read `missing` and `changedSince`[\s\S]{0,40}as leads, never as answers/)
  assert.match(SKILL, /\*\*Signals are not verdicts\.\*\*/)
})

test('write mode files one epic parent before any child', () => {
  assert.match(SKILL, /create the epic parent \*\*first\*\*/)
  assert.ok(SKILL.includes('`labels: ["epic"]`'))
  assert.match(SKILL, /deliberately \*\*not\*\* `agent-plan`/)
  assert.match(SKILL, /If the parent create\s+fails or is ambiguous, create no children/)
  assert.match(SKILL, /`EPIC_MAX_CHILDREN` is not in this code path/)
  assert.ok(SKILL.includes('`parentId`: the epic parent'))
  assert.ok(SKILL.includes('`team: "Bossanova"`'))
  assert.ok(SKILL.includes('`state: "Unplanned"`'))
  assert.ok(SKILL.includes('`labels: ["agent-plan"]`'))
  assert.ok(SKILL.includes('`title`: `cluster.title`'))
  assert.match(SKILL, /failed or ambiguous create stops further\s+writes/)
  // The pre-create recheck must be a complete snapshot scanned locally, never a
  // fuzzy list_issues query: that query returns unmarked issues AND truncated
  // descriptions, which makes the exact line-anchored match impossible to evaluate.
  assert.ok(SKILL.includes('>"$RECHECK_JSON"'))
  assert.match(SKILL, /re-fetch the \*\*complete\*\* marker snapshot once/)
  assert.match(SKILL, /Do \*\*not\*\* use `mcp__bossanova-linear__list_issues` for this/)
  assert.match(SKILL, /returns each `description` \*\*truncated\*\*/)
  assert.match(SKILL, /every `cluster\.sourceKeys` alias/)
  assert.match(SKILL, /If the re-fetch fails, create nothing at all/)
  // The tool is no longer used anywhere, so it must not stay in allowed-tools.
  assert.doesNotMatch(SKILL, /allowed-tools:.*list_issues/)
})

test('a live theme can still retire individually fixed member notes', () => {
  assert.ok(SKILL.includes('"fixedNotes": ["<note-id>"]'))
  assert.match(SKILL, /optional on \*\*any\*\* verdict/)
  assert.match(SKILL, /A broad theme is almost never wholly `fixed`,\s+because one live sibling/)
  assert.match(SKILL, /ids must belong to this theme/)
  assert.match(SKILL, /a non-empty list\s+requires `evidence`/)
  // The retirement set must come from the gate, not from prose unioning buckets.
  assert.ok(SKILL.includes('node "$GATE" retired "$BUCKETS_JSON"'))
  assert.match(SKILL, /rather than unioning buckets by hand/)
  assert.match(SKILL, /can be both filed as a child and have some of its notes retired/)
})

test('every child carries its verbatim source notes, and deletion depends on it', () => {
  assert.ok(SKILL.includes('`title`: exactly `Source notes (<issue-id>)`'))
  assert.match(SKILL, /every member note's \*\*unmodified\*\* `body`/)
  // Deleting the notes is only safe once the evidence exists on the ticket.
  assert.match(SKILL, /\*\*The attachment is the precondition for deletion\.\*\*/)
  assert.match(SKILL, /a theme whose\s+attachment failed keeps its notes/)
  assert.match(SKILL, /Never infer the attachment from the create response/)
  // attachmentCreate mints a new row per call, so a resume must not duplicate.
  assert.match(SKILL, /`attachmentCreate` is \*\*not idempotent\*\*/)
  // A bare note id in a filed description is a dead reference after Phase 4.
  assert.match(SKILL, /Never\s+cite a bare note id as if it were a lookup/)
  // The tools must be declared.
  assert.match(SKILL, /allowed-tools:.*prepare_attachment_upload/)
  assert.match(SKILL, /allowed-tools:.*create_attachment_from_upload/)
})

test('fixed themes are retagged rather than deleted, and nothing else is touched', () => {
  assert.ok(SKILL.includes('boss notes edit <id> --tag stale'))
  assert.match(SKILL, /`--tag` REPLACES the whole tag set/)
  assert.match(SKILL, /Deletion is not used here/)
  assert.match(SKILL, /`boss notes rm` is\s+permanent/)
  assert.match(SKILL, /Every id must appear in `snapshot-ids`/)
  assert.match(SKILL, /every initial `dropped` theme whose reason is `already-tracked`/)
  // Deliberately narrower than the old blanket rule: an `unverifiable` theme may
  // still retire a member the gate named, so the guard is per-note, not per-bucket.
  assert.match(SKILL, /Never touch a `deferred` theme, nor any note the gate did not name/)
  // Delete and retag sets overlap heavily; precedence must be stated, not left to loop order.
  assert.match(SKILL, /\*\*Deletion wins\*\*: subtract the delete set above before retagging/)
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
