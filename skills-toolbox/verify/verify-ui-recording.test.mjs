import assert from 'node:assert/strict'
import fs from 'node:fs'
import path from 'node:path'
import test from 'node:test'

import {
  CAPTURE_SIDECAR as SIDECAR,
  CAPTURE_SIDECAR_SCHEMA as SIDECAR_SCHEMA,
} from './verify-ci-media.mjs'
import {
  appliesMain,
  EXTENSION,
  NOT_APPLICABLE_EXIT,
  uiFilesOf,
  cliMain,
  verifyMain,
} from './verify-ui-recording.mjs'
import { classifyVerifyOutcome } from '../skill-extensions.mjs'

const SOURCES = [
  { suite: 'mobile', workflow: 'mobile.yml', job: 'capture-mobile', artifact: 'mobile-{head}' },
  { suite: 'admin', workflow: 'admin.yml', job: 'capture-admin', artifact: 'admin-{head}' },
  { suite: 'pdf', workflow: 'pdf.yml', job: 'capture-pdf', artifact: 'pdf-{head}' },
]

const REPO = 'example/app'
const HEAD = 'a'.repeat(40)
const OTHER = 'b'.repeat(40)
const SCREEN = 'services/mobile/src/screens/ClaimListScreen.tsx'
const ROW = 'services/mobile/src/components/layout/ClaimRowCore.ts'
const PAGE = 'services/admin/src/pages/[organizationId]/claims.tsx'
const MOBILE_SPEC = 'e2e/mobile/tests/claims/claim-list.spec.ts'
const ADMIN_SPEC = 'e2e/tests/claims/claim-list.spec.ts'
const RUN = { 'admin.yml': 8, 'mobile.yml': 7 }

const clip = (id) => ({
  chapters: [{ atMs: 900, caption: 'The row shows the full name' }],
  durationMs: 9000,
  file: `${id}.mp4`,
  id,
  side: 'head',
  stills: [{ atMs: 1200, caption: 'Full name', file: `${id}-still-0.png` }],
  title: `Clip ${id}`,
})

// One artifact: its manifest, every file it names unless listed in `missing`, and its sidecar.
// `proves` maps a clip id to `{spec, files}`; `plain` lists the clips with no proof steps.
function artifact({
  clips,
  head = HEAD,
  missing = [],
  plain = [],
  proves,
  schema = SIDECAR_SCHEMA,
}) {
  const named = clips.flatMap((c) => [c.file, ...c.stills.map((s) => s.file)])
  return {
    ...Object.fromEntries(named.filter((file) => !missing.includes(file)).map((f) => [f, f])),
    'proof-media.json': JSON.stringify({
      clips,
      head,
      schema: 'proof-media/1',
      transcripts: [],
    }),
    [SIDECAR]: JSON.stringify({ clips: proves, plain, schema }),
  }
}

// A gh stand-in with one run per workflow. `suites` maps a suite to its artifact() files, and
// `conclusions` a suite to its proof job's conclusion (default success).
function fakeGh({ conclusions = {}, fail = false, suites = {}, sources = SOURCES } = {}) {
  const calls = []
  const runs = new Map()
  const files = {}
  for (const { artifact: name, job, suite, workflow } of sources) {
    const id = RUN[workflow] ?? 9
    if (!runs.has(id)) runs.set(id, { artifacts: [], id, jobs: [], workflow })
    runs.get(id).jobs.push({
      conclusion: conclusions[suite] ?? 'success',
      name: job,
      started_at: '2026-11-10T00:00:00Z',
      status: 'completed',
    })
    if (!suites[suite]) continue
    runs.get(id).artifacts.push({
      created_at: '2026-11-10T00:05:00Z',
      expired: false,
      name: name.replaceAll('{head}', HEAD),
    })
    files[name.replaceAll('{head}', HEAD)] = suites[suite]
  }
  const gh = (args) => {
    calls.push(args)
    if (fail) return { status: 1, stdout: '' }
    if (args[0] === 'run' && args[1] === 'download') {
      const dir = args[args.indexOf('--dir') + 1]
      for (const [file, content] of Object.entries(files[args[args.indexOf('--name') + 1]]))
        fs.writeFileSync(path.join(dir, file), content)
      return { status: 0, stdout: '' }
    }
    const url = new URL(args[1], 'https://api.github.com/')
    const workflow = /\/workflows\/([^/]+)\/runs$/.exec(url.pathname)?.[1]
    const body = workflow
      ? {
          workflow_runs: [...runs.values()]
            .filter((run) => run.workflow === workflow)
            .map((run) => ({
              head_sha: HEAD,
              id: run.id,
              pull_requests: [{ number: 12 }],
            })),
        }
      : (() => {
          const [, id, kind] = /\/runs\/(\d+)\/(jobs|artifacts)$/.exec(url.pathname)
          const run = runs.get(Number(id))
          return kind === 'jobs'
            ? { jobs: run.jobs }
            : {
                artifacts: run.artifacts.filter((a) => a.name === url.searchParams.get('name')),
              }
        })()
    return { status: 0, stdout: JSON.stringify(body) }
  }
  return { calls, gh }
}

const envelope = (changedPaths, more = {}) => ({
  baseSha: OTHER,
  changedPaths,
  headSha: HEAD,
  pr: { number: 12, url: `https://github.com/${REPO}/pull/12` },
  ticket: null,
  ...more,
})

const baseConfig =
  (sources = SOURCES) =>
  () => ({ status: 0, stdout: JSON.stringify({ proofDefaults: { media: { sources } } }) })

function verify(gh, changedPaths, more, run = baseConfig()) {
  let stdout = ''
  const code = verifyMain({
    gh,
    run,
    stdinText: JSON.stringify(envelope(changedPaths, more)),
    write: (text) => {
      stdout += text
    },
  })
  assert.equal(code, 0)
  assert.equal(stdout.split('\n').length, 2, 'one JSON line')
  const result = JSON.parse(stdout)
  assert.equal(result.extension, EXTENSION)
  return { outcome: classifyVerifyOutcome({ optional: true, result }), result }
}

const mobileProves = (files, more = {}) =>
  artifact({
    clips: [clip('head-0')],
    proves: { 'head-0': { files, spec: MOBILE_SPEC } },
    ...more,
  })

const assertAbstains = ({ outcome, result }, reason) => {
  assert.equal(result.verdict, 'abstain')
  assert.equal(result.reason, reason)
  assert.deepEqual(result.evidence, [])
  assert.equal(outcome.outcome, 'abstain', 'the core reads it as an abstain')
  assert.equal(outcome.recorded, undefined)
}

test('every changed UI file proven by a recorded clip with proof steps passes as a recording', () => {
  const { gh } = fakeGh({ suites: { mobile: mobileProves([SCREEN, ROW]) } })
  const { outcome, result } = verify(gh, [SCREEN, ROW, 'services/api/x.go'])
  assert.equal(result.verdict, 'pass')
  assert.deepEqual(result.evidence, [
    {
      files: [SCREEN, ROW],
      kind: 'recording',
      ref: 'mobile/head-0.mp4',
      spec: MOBILE_SPEC,
      title: `head-0 from ${MOBILE_SPEC}`,
      url: `https://github.com/${REPO}/actions/runs/7`,
    },
  ])
  assert.deepEqual(outcome, {
    outcome: 'ok',
    reason: 'pass',
    recorded: true,
    verdict: 'pass',
  })
})

test('evidence includes only clips and files proving the changed UI', () => {
  const { gh } = fakeGh({
    suites: {
      mobile: artifact({
        clips: [clip('used'), clip('unrelated')],
        proves: {
          used: { files: [SCREEN, ROW], spec: MOBILE_SPEC },
          unrelated: { files: [PAGE], spec: ADMIN_SPEC },
        },
      }),
    },
  })
  const { result } = verify(gh, [SCREEN])
  assert.equal(result.verdict, 'pass')
  assert.deepEqual(
    result.evidence.map(({ ref, files }) => ({ ref, files })),
    [{ ref: 'mobile/used.mp4', files: [SCREEN] }],
  )
})

test('configured arbitrary suites prove UI while document captures add no clips', () => {
  const sources = [
    { suite: 'browser', workflow: 'capture.yml', job: 'proof', artifact: 'proof-{head}' },
  ]
  const { gh } = fakeGh({ sources, suites: { browser: mobileProves([SCREEN]) } })
  const result = verify(gh, [SCREEN], {}, baseConfig(sources)).result
  assert.equal(result.verdict, 'pass')
  assert.equal(result.evidence[0].ref, 'browser/head-0.mp4')
  const docs = fakeGh({
    suites: { mobile: mobileProves([SCREEN]), pdf: artifact({ clips: [], proves: {} }) },
  })
  assert.equal(verify(docs.gh, [SCREEN]).result.verdict, 'pass')
})

test('core UI classification excludes tests, pure moves and Go files', () => {
  const files = [
    SCREEN,
    `${SCREEN}.test.tsx`,
    { path: 'ui/New.tsx', previousPath: 'ui/Old.tsx', status: 'renamed', changes: 0 },
    'api/server.go',
  ]
  assert.deepEqual(uiFilesOf(files), [SCREEN])
  assert.deepEqual(uiFilesOf([ROW, PAGE]), [ROW, PAGE].sort())
})

test('core-classified UI paths exclude pure renames from recording requirements', () => {
  const changedPaths = [SCREEN, 'ui/New.tsx', 'ui/Old.tsx']
  const { gh } = fakeGh({ suites: { mobile: mobileProves([SCREEN]) } })
  const { result } = verify(gh, changedPaths, { uiFiles: [SCREEN] })
  assert.equal(result.verdict, 'pass')
  assert.deepEqual(result.evidence[0].files, [SCREEN])
  assert.equal(
    appliesMain({
      stdinText: JSON.stringify(envelope(changedPaths.slice(1), { uiFiles: [] })),
      run: baseConfig(),
    }),
    NOT_APPLICABLE_EXIT,
  )
  assertAbstains(verify(gh, [SCREEN], { uiFiles: ['unlisted.tsx'] }), 'envelope-unusable')
})

test('CLI preserves PR-file rename metadata when selecting UI recording requirements', () => {
  const capture = fakeGh({ suites: { mobile: mobileProves([SCREEN]) } })
  const gh = (args) => {
    if (args[0] === 'pr')
      return {
        status: 0,
        stdout: JSON.stringify({
          baseRefOid: OTHER,
          headRefOid: HEAD,
          number: 12,
          url: `https://github.com/${REPO}/pull/12`,
        }),
      }
    if (args[1].includes('/pulls/12/files'))
      return {
        status: 0,
        stdout: JSON.stringify([
          [
            { filename: SCREEN, status: 'modified', changes: 2 },
            {
              filename: 'ui/New.tsx',
              previous_filename: 'ui/Old.tsx',
              status: 'renamed',
              changes: 0,
            },
          ],
        ]),
      }
    return capture.gh(args)
  }
  const result = JSON.parse(cliMain(['--pr', '12'], { gh, run: baseConfig() }))
  assert.equal(result.verdict, 'pass')
  assert.deepEqual(result.evidence[0].files, [SCREEN])
})

test('missing proof detail names at most five UI files', () => {
  const files = Array.from({ length: 7 }, (_, i) => `ui/Screen${i}.tsx`)
  const out = verify(fakeGh().gh, files)
  assertAbstains(out, 'ui-not-recorded')
  assert.match(out.result.detail, /Screen4\.tsx and 2 more$/)
  assert.ok(!out.result.detail.includes('Screen5.tsx'))
})

test('unconfigured base sources abstain and skip without querying CI', () => {
  const { calls, gh } = fakeGh()
  assertAbstains(verify(gh, [SCREEN], {}, baseConfig([])), 'not-configured')
  assert.equal(
    appliesMain({ stdinText: JSON.stringify(envelope([SCREEN])), run: baseConfig([]) }),
    NOT_APPLICABLE_EXIT,
  )
  assert.deepEqual(calls, [])
})

test('unreadable base config abstains but cannot skip applicability', () => {
  const run = () => ({ status: 1, stderr: 'fatal: unavailable' })
  assertAbstains(verify(fakeGh().gh, [SCREEN], {}, run), 'evidence-unreadable')
  assert.equal(appliesMain({ stdinText: JSON.stringify(envelope([SCREEN])), run }), 0)
})

test('config is read at the envelope base and CLI includes that base', () => {
  const reads = []
  const run = (command, args) => {
    reads.push([command, args])
    return baseConfig([])()
  }
  const gh = (args) =>
    args[0] === 'api'
      ? {
          status: 0,
          stdout: JSON.stringify([[{ filename: SCREEN, status: 'modified', changes: 2 }]]),
        }
      : {
          status: 0,
          stdout: JSON.stringify({
            baseRefOid: OTHER,
            headRefOid: HEAD,
            number: 12,
            url: `https://github.com/${REPO}/pull/12`,
          }),
        }
  const output = cliMain(['--pr', '12'], { gh, run })
  assert.equal(JSON.parse(output).reason, 'not-configured')
  assert.deepEqual(reads, [['git', ['show', `${OTHER}:.boss-skills.json`]]])
  assert.throws(() => cliMain(['--unknown'], { gh, run }))
})

test('UI split across suites passes only with each suite proving its own files', () => {
  const admin = artifact({
    clips: [clip('admin-0')],
    proves: { 'admin-0': { files: [PAGE], spec: ADMIN_SPEC } },
  })
  const { gh } = fakeGh({ suites: { admin, mobile: mobileProves([SCREEN]) } })
  const both = verify(gh, [SCREEN, PAGE])
  assert.equal(both.result.verdict, 'pass')
  assert.deepEqual(
    both.result.evidence.map(({ ref }) => ref),
    ['mobile/head-0.mp4', 'admin/admin-0.mp4'],
  )
  const mobileOnly = fakeGh({ suites: { mobile: mobileProves([SCREEN]) } })
  const missing = verify(mobileOnly.gh, [SCREEN, PAGE])
  assertAbstains(missing, 'ui-not-recorded')
  assert.match(missing.result.detail, /claims\.tsx/)
})

test('a UI file no clip proves abstains and names it', () => {
  const { gh } = fakeGh({ suites: { mobile: mobileProves([SCREEN]) } })
  const out = verify(gh, [SCREEN, ROW])
  assertAbstains(out, 'ui-not-recorded')
  assert.equal(out.result.detail, `no clip with proof steps proves ${ROW}`)
})

test('a plain clip, recorded from a test with no proof steps, proves nothing', () => {
  const { gh } = fakeGh({
    suites: { mobile: mobileProves([SCREEN], { plain: ['head-0'] }) },
  })
  assertAbstains(verify(gh, [SCREEN]), 'ui-not-recorded')
})

test('a clip whose video is missing from the artifact is not recorded', () => {
  const { gh } = fakeGh({
    suites: { mobile: mobileProves([SCREEN], { missing: ['head-0.mp4'] }) },
  })
  assertAbstains(verify(gh, [SCREEN]), 'ui-not-recorded')
})

test('an artifact for another head or with an older sidecar records nothing', () => {
  for (const more of [{ head: OTHER }, { schema: 'fc-proof-capture/1' }]) {
    const { gh } = fakeGh({ suites: { mobile: mobileProves([SCREEN], more) } })
    assertAbstains(verify(gh, [SCREEN]), 'ui-not-recorded')
  }
})

test('a failed proof-capture job abstains even when another suite proves every file', () => {
  const { gh } = fakeGh({
    conclusions: { admin: 'failure' },
    suites: { mobile: mobileProves([SCREEN]) },
  })
  const out = verify(gh, [SCREEN])
  assertAbstains(out, 'capture-failed')
  assert.match(out.result.detail, /admin/)
})

test('a failed configured source abstains even without UI clips', () => {
  const { gh } = fakeGh({
    conclusions: { pdf: 'failure' },
    suites: { mobile: mobileProves([SCREEN]) },
  })
  assertAbstains(verify(gh, [SCREEN]), 'capture-failed')
})

test('no capture at all abstains', () => {
  assertAbstains(verify(fakeGh().gh, [SCREEN]), 'ui-not-recorded')
})

test('a gh failure abstains with exit 0', () => {
  assertAbstains(verify(fakeGh({ fail: true }).gh, [SCREEN]), 'evidence-unreadable')
})

test('an unusable envelope abstains before any gh call', () => {
  for (const more of [
    { headSha: 'nope' },
    { pr: { number: 12, url: '' } },
    { changedPaths: 'x' },
  ]) {
    const { calls, gh } = fakeGh({ suites: { mobile: mobileProves([SCREEN]) } })
    assertAbstains(verify(gh, [SCREEN], more), 'envelope-unusable')
    assert.deepEqual(calls, [])
  }
})

test('no changed UI file abstains without reading CI', () => {
  const { calls, gh } = fakeGh()
  assertAbstains(verify(gh, ['services/api/x.go', `${SCREEN}.md`]), 'no-ui-files')
  assert.deepEqual(calls, [])
})

test('applies says not applicable only when no UI file changed, and prints no note', () => {
  const applies = (stdinText) => ({ code: appliesMain({ stdinText, run: baseConfig() }) })
  assert.deepEqual(applies(JSON.stringify(envelope(['services/api/x.go']))), {
    code: NOT_APPLICABLE_EXIT,
  })
  assert.equal(applies(JSON.stringify(envelope([ROW]))).code, 0)
  assert.equal(applies('not json').code, 0, 'an unreadable envelope still applies')
})
