import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { runInNewContext } from 'node:vm'
import { keyValue, structuralLines } from '../dev/workflow-yaml-lib.mjs'

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const read = (name) => fs.readFileSync(path.join(root, name), 'utf8')
const snapshot = JSON.parse(read('scripts/test-infra/heavy-ci-triggers.snapshot.json'))
const workflows = Object.keys(snapshot.jobIds)
const heavyJobs = {
  'bazel.yml': ['go-test', 'go-ledger', 'sqlc', 'go-lint'],
  'test-scripts.yml': ['scripts'],
  'test-web.yml': ['web', 'web-e2e'],
  'test-web-real-stack.yml': ['real-stack'],
}

// Read mapping boundaries through the shared YAML lexer, including folded conditions and
// block scalar bodies. A shell line that resembles a YAML key cannot create a fake job.
function fields(text, indent) {
  const lines = structuralLines(text)
  const raw = text.split('\n')
  return Object.fromEntries(
    lines.flatMap((line, i) => {
      const pair = keyValue(line.text)
      if (line.indent !== indent || !pair || line.text.startsWith('- ')) return []
      const end =
        lines
          .slice(i + 1)
          .find(
            (next) =>
              next.indent < indent || (next.indent === indent && !next.text.startsWith('- ')),
          )?.line ?? raw.length + 1
      const body = raw
        .slice(line.line, end - 1)
        .join('\n')
        .trimEnd()
      const value = /^[>|]/.test(pair.value) ? body.trim().replace(/\s+/g, ' ') : pair.value
      return [
        [
          pair.key,
          {
            value,
            body,
            text: raw
              .slice(line.line - 1, end - 1)
              .join('\n')
              .trimEnd(),
          },
        ],
      ]
    }),
  )
}
const jobs = (text) => fields(fields(text, 0).jobs.body, 2)
const props = (job) => fields(job.body, 4)
const condition = (job) => props(job).if?.value ?? ''
const normalize = (text) => text.replace(/\s+/g, ' ').trim()
const draftGuard =
  "(github.base_ref == 'staging' || github.base_ref == 'production' || github.event_name != 'pull_request' || !github.event.pull_request.draft || vars.CI_DRAFT_RUNS_ON_REQUEST != 'true')"

// These conditions use the shared JavaScript/GitHub Actions boolean subset.
// Evaluate the real job expression with runnable prerequisites, so a skipped
// draft release cannot masquerade as a successful workflow for later dedup.
function jobRuns(job, { base, draft, switchValue, action }) {
  const expression = condition(job)
    .replace(/^\$\{\{\s*|\s*\}\}$/g, '')
    .replace(/needs\.([\w-]+)/g, (_, name) => `needs[${JSON.stringify(name)}]`)
  return runInNewContext(expression, {
    github: {
      event_name: 'pull_request',
      base_ref: base,
      event: { action, pull_request: { draft } },
    },
    vars: { CI_DRAFT_RUNS_ON_REQUEST: switchValue },
    needs: {
      dedup: { outputs: { skip: 'false', run: 'true' } },
      'roadmap-only': { outputs: { skip: 'false' } },
    },
    cancelled: () => false,
  })
}
function assertDraftBehavior(text, name) {
  for (const [id, job] of Object.entries(jobs(text))) {
    if (!condition(job).includes('CI_DRAFT_RUNS_ON_REQUEST')) continue
    const featureOnly = name === 'bazel.yml' && ['go-test', 'go-ledger'].includes(id)
    for (const base of ['main', 'staging', 'production']) {
      for (const switchValue of ['true', 'false', '']) {
        for (const action of ['opened', 'synchronize', 'ready_for_review']) {
          const draft = action !== 'ready_for_review'
          const release = base !== 'main'
          const expected = release ? !featureOnly : !draft || switchValue !== 'true'
          assert.equal(
            jobRuns(job, { base, draft, switchValue, action }),
            expected,
            `${name}/${id}: base=${base}, draft=${draft}, switch=${switchValue}, event=${action}`,
          )
        }
      }
    }
  }
}

function assertDispatch(text) {
  const on = fields(fields(text, 0).on.body, 2)
  const inputs = fields(fields(on.workflow_dispatch?.body ?? '', 4).inputs?.body ?? '', 6)
  for (const input of ['pr', 'base']) {
    assert.ok(inputs[input], `dispatch input ${input} missing`)
    const definition = fields(inputs[input].body, 8)
    assert.equal(definition.type?.value, 'string', `${input} must be a string`)
    assert.notEqual(definition.required?.value, 'true', `${input} must be optional`)
  }
}
function assertEvents(text) {
  const on = fields(fields(text, 0).on.body, 2)
  assert.ok(on.pull_request, 'pull_request trigger missing')
  const pr = fields(on.pull_request.body, 4)
  for (const event of ['opened', 'synchronize', 'reopened', 'ready_for_review']) {
    assert.match(pr.types?.text ?? '', new RegExp(`\\b${event}\\b`), `PR event ${event} missing`)
  }
  assert.ok(!on.push, 'feature push trigger must be removed')
  assert.ok(
    !pr.branches && !pr['branches-ignore'] && !pr.paths,
    'PR trigger must cover release and feature PRs without filtering',
  )
}
function assertDraft(job) {
  assert.ok(
    normalize(condition(job)).includes(draftGuard),
    'feature heavy job must carry the variable-controlled draft guard',
  )
}
function steps(job) {
  const body = props(job).steps?.body ?? ''
  const lines = structuralLines(body)
  const starts = lines.filter((line) => /^-\s/.test(line.text))
  if (!starts.length) return []
  const indent = starts[0].indent
  const boundaries = starts.filter((line) => line.indent === indent)
  const raw = body.split('\n')
  return boundaries.map((line, i) => {
    const text = raw
      .slice(line.line - 1, (boundaries[i + 1]?.line ?? raw.length + 1) - 1)
      .join('\n')
    // Convert the first list marker into mapping indentation for the field reader.
    return { text, fields: fields(text.replace(/^(\s*)- /, '$1  '), indent + 2) }
  })
}
function assertMergeCheckout(job) {
  const checkout = steps(job).find(
    (step) => step.fields.uses?.value === './.github/actions/checkout-pr-merge',
  )
  assert.ok(checkout, 'dispatched job must use the merge checkout composite')
  assert.match(
    checkout.fields.if?.value ?? '',
    /github\.event_name == 'workflow_dispatch'/,
    'merge checkout must be dispatch-only',
  )
}
function assertInheritance(job) {
  assert.doesNotMatch(
    condition(job),
    /needs\.dedup\.outputs\.skip/,
    'required check must inherit through steps, never job skipping',
  )
  assert.match(condition(job), /!cancelled\(\)/, 'skipped dedup must fall through')
  assert.match(props(job).needs?.value ?? '', /\bdedup\b/, 'required job must depend on dedup')
  const all = steps(job)
  let count = 0
  for (const step of all) {
    if (/checkout/.test(step.fields.uses?.value ?? '')) continue
    // The notice is the only non-checkout step allowed to run on inherited success.
    if (/needs\.dedup\.outputs\.skip == 'true'/.test(step.fields.if?.value ?? '')) {
      assert.match(
        step.fields.run?.body || step.fields.run?.value || '',
        /::notice::/,
        'inherited check must explain its successful result',
      )
      continue
    }
    count++
    assert.match(
      step.fields.if?.value ?? '',
      /needs\.dedup\.outputs\.skip != 'true'/,
      'every work step must honor inheritance',
    )
  }
  assert.ok(count > 0, 'required check must still contain work')
  assert.ok(
    all.some((step) => /::notice::/.test(step.text)),
    'required check needs an inheritance notice',
  )
}
function assertDedup(job) {
  const probe = steps(job).find(
    (step) => step.fields.uses?.value === './.github/actions/skip-if-sha-already-green',
  )
  assert.ok(probe, 'dedup probe missing')
  assert.match(
    `${condition(job)} ${probe.fields.if?.value ?? ''}`,
    /ready_for_review/,
    'dedup must run when feature PR becomes ready',
  )
  assert.match(
    probe.fields.with?.body ?? '',
    /workflow_dispatch,pull_request/,
    'ready inheritance must accept dispatched greens',
  )
}
function assertJobIds(text, name) {
  for (const id of snapshot.jobIds[name]) assert.ok(jobs(text)[id], `existing check ${id} missing`)
}
function assertRelease(text) {
  for (const [id, body] of Object.entries(snapshot.releaseJobs)) {
    assert.equal(jobs(text)[id]?.text, body, `release job ${id} changed`)
  }
}
function assertHelper(text) {
  const events = fields(fields(text, 0).inputs?.body ?? '', 2).events
  assert.equal(
    fields(events?.body ?? '', 4).default?.value,
    'pull_request',
    'release dedup default must stay pull_request',
  )
}

for (const name of workflows) {
  const text = read(`.github/workflows/${name}`)
  test(`${name}: release drafts run while feature drafts honor the switch`, () =>
    assertDraftBehavior(text, name))
  test(`${name}: optional dispatch inputs`, () => assertDispatch(text))
  test(`${name}: rejects a missing dispatch input`, () => {
    assert.throws(() => assertDispatch(text.replace(/^      base:/m, '      removed:')), /base/)
  })
  test(`${name}: PR events replace feature pushes`, () => assertEvents(text))
  test(`${name}: rejects removal of the ready event`, () => {
    assert.throws(
      () => assertEvents(text.replaceAll('ready_for_review', 'converted_to_draft')),
      /ready_for_review/,
    )
  })
  test(`${name}: rejects feature pushes`, () => {
    assert.throws(() => assertEvents(text.replace(/^on:/m, 'on:\n  push:')), /push trigger/)
  })
  test(`${name}: preserves every existing job id`, () => assertJobIds(text, name))
  test(`${name}: rejects a lost check name`, () => {
    const id = snapshot.jobIds[name][0]
    assert.throws(() => assertJobIds(text.replace(`  ${id}:`, '  renamed:'), name), /missing/)
  })
  test(`${name}: ready event probes previous dispatches`, () => assertDedup(jobs(text).dedup))
  test(`${name}: rejects PR-only inheritance`, () => {
    assert.throws(
      () =>
        assertDedup(jobs(text.replaceAll('workflow_dispatch,pull_request', 'pull_request')).dedup),
      /dispatched greens/,
    )
  })
  for (const id of heavyJobs[name]) {
    const job = jobs(text)[id]
    test(`${name}/${id}: switch controls draft runs`, () => assertDraft(job))
    test(`${name}/${id}: rejects a missing draft switch`, () => {
      assert.throws(
        () =>
          assertDraft({
            ...job,
            body: job.body.replaceAll('CI_DRAFT_RUNS_ON_REQUEST', 'OTHER_SWITCH'),
          }),
        /draft guard/,
      )
    })
    test(`${name}/${id}: dispatch uses merge checkout`, () => assertMergeCheckout(job))
    test(`${name}/${id}: rejects bare-head dispatch checkout`, () => {
      assert.throws(
        () =>
          assertMergeCheckout({
            ...job,
            body: job.body.replaceAll(
              './.github/actions/checkout-pr-merge',
              'actions/checkout@sha',
            ),
          }),
        /merge checkout/,
      )
    })
    test(`${name}/${id}: rejects unconditional merge checkout`, () => {
      assert.throws(
        () =>
          assertMergeCheckout({
            ...job,
            body: job.body.replaceAll("github.event_name == 'workflow_dispatch'", 'true'),
          }),
        /dispatch-only/,
      )
    })
  }
  for (const id of ['go-test', 'scripts'].filter((id) => heavyJobs[name].includes(id))) {
    const job = jobs(text)[id]
    test(`${name}/${id}: required check inherits successfully through steps`, () =>
      assertInheritance(job))
    test(`${name}/${id}: rejects inheritance through job skipping`, () => {
      assert.throws(
        () =>
          assertInheritance({
            ...job,
            body: job.body.replace(/^    if:/m, "    if: needs.dedup.outputs.skip != 'true' &&"),
          }),
        /never job skipping/,
      )
    })
    test(`${name}/${id}: rejects work running after inherited success`, () => {
      assert.throws(
        () =>
          assertInheritance({
            ...job,
            body: job.body.replaceAll("needs.dedup.outputs.skip != 'true'", 'true'),
          }),
        /honor inheritance/,
      )
    })
  }
}
const bazel = read('.github/workflows/bazel.yml')
test('bazel release jobs remain byte-for-byte unchanged', () => assertRelease(bazel))
test('rejects a changed release tier', () => {
  assert.throws(
    () => assertRelease(bazel.replace('  release-drift:', '  moved-release-drift:')),
    /release job/,
  )
})
const helper = read('.github/actions/skip-if-sha-already-green/action.yml')
test('release callers retain the pull_request-only dedup default', () => assertHelper(helper))
test('rejects release dedup inheriting feature dispatches by default', () => {
  assert.throws(
    () =>
      assertHelper(helper.replace(/default: ['"]?pull_request['"]?/, 'default: workflow_dispatch')),
    /release dedup default/,
  )
})
