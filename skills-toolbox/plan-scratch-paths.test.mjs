import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, relative } from 'node:path'
import { fileURLToPath } from 'node:url'
import {
  PLAN_SCRATCH_FAMILIES,
  PLAN_SCRATCH_ROOT,
  planScratchFamily,
  planScratchPath,
  planScratchToken,
  runScratchDir,
} from './plan-scratch-paths.mjs'

test('runScratchDir addresses scratch by run, under the scratch root', () => {
  assert.equal(runScratchDir('abc123'), '.linear-plans/run-abc123')
  assert.equal(runScratchDir('abc123', 'other-root'), 'other-root/run-abc123')
})

test('runScratchDir refuses a run id that could escape the scratch root', () => {
  assert.throws(() => runScratchDir(''), /requires a run id/)
  assert.throws(() => runScratchDir('  '), /requires a run id/)
  assert.throws(() => runScratchDir('..'), /invalid run id/)
  assert.throws(() => runScratchDir('a/b'), /invalid run id/)
})

test('planScratchPath builds every declared family inside this run directory', () => {
  const parts = {
    issueId: 'ABC-1',
    slug: 'ABC-1-a-title',
    parentId: 'ABC-9',
    key: 'k1',
    childId: 'ABC-2',
    n: '1',
  }
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const path = planScratchPath('r1', family.family, parts)
    assert.ok(
      path.startsWith('.linear-plans/run-r1/'),
      `${family.family} resolved outside the run directory: ${path}`,
    )
    assert.equal(path.split('/').length, 3, `${family.family} nested below the run directory`)
    assert.doesNotMatch(path, /undefined/, `${family.family} left an unfilled template part`)
  }
})

test('planScratchPath refuses an undeclared family instead of inventing a name', () => {
  assert.throws(
    () => planScratchPath('r1', 'safe-orig', { issueId: 'ABC-1' }),
    /undeclared scratch family "safe-orig"/,
  )
})

test('planScratchPath refuses a family whose template parts were not supplied', () => {
  assert.throws(() => planScratchPath('r1', 'precheck', {}), /unusable basename/)
})

test('every declared family round-trips: its own template resolves back to it', () => {
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const token = `${PLAN_SCRATCH_ROOT}/run-<RUN-SCRATCH-ID>/${family.template}`
    const result = planScratchToken(token)
    assert.ok(result.ok, `${family.family}: ${result.ok ? '' : result.reason}`)
    assert.deepEqual(
      result.families,
      [family.family],
      `${family.family} template resolved to ${JSON.stringify(result.families)}`,
    )
  }
})

test('every declared family round-trips from a concrete path too', () => {
  const parts = {
    issueId: 'ABC-1',
    slug: 'a-title',
    parentId: 'ABC-9',
    key: 'k1',
    childId: 'ABC-2',
    n: '1',
  }
  for (const family of PLAN_SCRATCH_FAMILIES) {
    const path = planScratchPath('r1', family.family, parts)
    const result = planScratchToken(path)
    assert.ok(result.ok, `${family.family}: ${result.ok ? '' : result.reason}`)
    assert.deepEqual(result.families, [family.family], `${family.family} → ${path}`)
  }
})

test('family keys and templates are unique', () => {
  const keys = PLAN_SCRATCH_FAMILIES.map((f) => f.family)
  const templates = PLAN_SCRATCH_FAMILIES.map((f) => f.template)
  assert.equal(new Set(keys).size, keys.length)
  assert.equal(new Set(templates).size, templates.length)
})

test('the four previously unnamed artifacts are declared families', () => {
  for (const family of [
    'attachment-guard-orig', // the guard safe source, once written as `<safe-orig.md>`
    'description', // the composed description, once `<new.md>` and a bare mktemp
    'deps-input', // the dependency-scan input, once `<the scratch JSON file you just wrote>`
    'epic-overview', // the epic parent overview, once composed with no path at all
  ]) {
    assert.ok(planScratchFamily(family), `${family} is not declared`)
  }
})

test('planScratchToken accepts the scratch root and this run directory', () => {
  assert.deepEqual(planScratchToken('.linear-plans'), { ok: true, kind: 'root', families: [] })
  assert.deepEqual(planScratchToken('.linear-plans/'), { ok: true, kind: 'root', families: [] })
  assert.deepEqual(planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>'), {
    ok: true,
    kind: 'run-dir',
    families: [],
  })
  assert.deepEqual(planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/'), {
    ok: true,
    kind: 'run-dir',
    families: [],
  })
})

test('planScratchToken rejects an artifact that is not run-scoped', () => {
  const result = planScratchToken('.linear-plans/<ISSUE-ID>.precheck.json')
  assert.equal(result.ok, false)
  assert.match(result.reason, /must be run-scoped/)
})

test('planScratchToken rejects an ad-hoc name even inside the run directory', () => {
  for (const adhoc of [
    '<ISSUE-ID>.safe-orig.md',
    '<ISSUE-ID>.raw.json',
    '<ISSUE-ID>.desc-new.md',
  ]) {
    const result = planScratchToken(`.linear-plans/run-<RUN-SCRATCH-ID>/${adhoc}`)
    assert.equal(result.ok, false, `${adhoc} was accepted`)
    assert.match(result.reason, /no declared scratch family/)
  }
})

test('planScratchToken rejects nesting below the run directory', () => {
  const result = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/children/<ISSUE-ID>-<slug>.md',
  )
  assert.equal(result.ok, false)
  assert.match(result.reason, /nests below/)
})

test('planScratchToken expands brace alternation and requires every branch to resolve', () => {
  const ok = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.{precheck,draft-metadata,premises,premise-states}.json',
  )
  assert.deepEqual(ok, {
    ok: true,
    kind: 'artifact',
    families: ['precheck', 'draft-metadata', 'premises', 'premise-states'],
  })

  const bad = planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.{precheck,raw}.json')
  assert.equal(bad.ok, false)
  assert.match(bad.reason, /no declared scratch family/)
})

test('planScratchToken distinguishes child artifacts from their parent forms', () => {
  const child = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.child-<CHILD-ID>.image-guard-orig.md',
  )
  assert.deepEqual(child.families, ['child-image-guard-orig'])

  const parent = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.image-guard-orig.md',
  )
  assert.deepEqual(parent.families, ['image-guard-orig'])

  const childPlan = planScratchToken(
    '.linear-plans/run-<RUN-SCRATCH-ID>/<PARENT>-child-<key>-<slug>.md',
  )
  assert.deepEqual(childPlan.families, ['child-plan'])

  const plan = planScratchToken('.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>-<slug>.md')
  assert.deepEqual(plan.families, ['plan'])
})

test('planScratchToken rejects a path outside the scratch root', () => {
  const result = planScratchToken('docs/plans/<ISSUE-ID>-<slug>.md')
  assert.equal(result.ok, false)
  assert.match(result.reason, /not a plan-scratch path/)
})

// ---------------------------------------------------------------------------
// The payload ratchet.
//
// The registry above is only worth having if the published skill actually uses
// it. This walks every markdown file in the published boss-plan payload, pulls
// out every `.linear-plans/…` path token it cites, and requires each to resolve
// through `planScratchToken`. It is the gate that fails on the next scratch path
// an agent was told to invent rather than to look up.
// ---------------------------------------------------------------------------

const PAYLOAD_DIR = fileURLToPath(
  new URL('../services/boss/internal/skillinstall/skills/boss-plan/', import.meta.url),
)

/** Every `.md` file in the published payload, recursively. */
function payloadMarkdown(dir = PAYLOAD_DIR, out = []) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name)
    if (entry.isDirectory()) payloadMarkdown(path, out)
    else if (entry.name.endsWith('.md')) out.push(path)
  }
  return out
}

// A token runs from `.linear-plans` to the first character that cannot be part of
// a cited path. Markdown delimiters (backticks, quotes, parens) and whitespace end
// it; `<>{},*` stay in, because the payload cites templates, brace lists and globs.
const TOKEN_RE = /\.linear-plans(?:\/[^\s`'"()\\|]*)?/g
const TRAILING_RE = /[.,;:!?—-]+$/

/** @returns {{token: string, file: string, line: number}[]} */
export function payloadScratchTokens(files = payloadMarkdown()) {
  const found = []
  for (const file of files) {
    const lines = readFileSync(file, 'utf8').split('\n')
    lines.forEach((text, i) => {
      for (const match of text.matchAll(TOKEN_RE)) {
        let token = match[0]
        // A trailing `}` that closes a `${VAR:-default}` expansion is not part of
        // the path. Drop unbalanced closers before anything else.
        while (
          token.endsWith('}') &&
          (token.match(/\}/g) || []).length > (token.match(/\{/g) || []).length
        ) {
          token = token.slice(0, -1)
        }
        // A sentence-ending period is not part of the path, but `.json` / `.md` is.
        if (!/\.(json|md|rejected|sh|mjs)$/.test(token)) token = token.replace(TRAILING_RE, '')
        found.push({ token, file: relative(PAYLOAD_DIR, file), line: i + 1 })
      }
    })
  }
  return found
}

test('the published payload cites at least one scratch path (the scan is not empty)', () => {
  const tokens = payloadScratchTokens()
  assert.ok(
    tokens.length > 20,
    `expected the payload to cite many scratch paths, found ${tokens.length} — ` +
      'an empty scan would make this ratchet vacuous',
  )
})

test('every .linear-plans token in the published payload resolves to a declared family', () => {
  const violations = []
  for (const { token, file, line } of payloadScratchTokens()) {
    const result = planScratchToken(token)
    if (!result.ok) violations.push(`${file}:${line}: ${result.reason}`)
  }
  assert.deepEqual(
    violations,
    [],
    `undeclared scratch paths in the published boss-plan payload:\n${violations.join('\n')}`,
  )
})

test('the ratchet rejects an ad-hoc token injected into payload text', () => {
  // Non-vacuity, checked in-process: the same scan over a file carrying an
  // invented name must fail. If this passes, the assertion above proves nothing.
  const dir = mkdtempSync(join(tmpdir(), 'payload-ratchet-'))
  const file = join(dir, 'SKILL.md')
  writeFileSync(
    file,
    'Write the snapshot to `.linear-plans/run-<RUN-SCRATCH-ID>/<ISSUE-ID>.safe-orig.md` before the guard.\n',
  )
  const tokens = payloadScratchTokens([file])
  assert.equal(tokens.length, 1)
  const result = planScratchToken(tokens[0].token)
  assert.equal(result.ok, false)
  assert.match(result.reason, /no declared scratch family/)
})

test('the ratchet rejects a flat, issue-scoped token injected into payload text', () => {
  const dir = mkdtempSync(join(tmpdir(), 'payload-ratchet-flat-'))
  const file = join(dir, 'SKILL.md')
  writeFileSync(file, 'Write it to `.linear-plans/<ISSUE-ID>.precheck.json` first.\n')
  const result = planScratchToken(payloadScratchTokens([file])[0].token)
  assert.equal(result.ok, false)
  assert.match(result.reason, /must be run-scoped/)
})
