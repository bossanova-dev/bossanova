// Ported behavior from freshclaim/fresh-claim commit 7f64d735e124eea3195cfd43950e3af877fc03a3.
import assert from 'node:assert/strict'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import test from 'node:test'

import {
  emptyManifest,
  findCaptures,
  mediaHookMain,
  mergeLocalMain,
  mediaSources,
} from './verify-ci-media.mjs'
import {
  CAPTURE_SIDECAR as SIDECAR,
  CAPTURE_SIDECAR_SCHEMA as SIDECAR_SCHEMA,
} from './verify-ci-media.mjs'
import { DEFAULT_CONFIG } from '../skill-config.mjs'
const CONFIG = {
  ...structuredClone(DEFAULT_CONFIG),
  ...JSON.parse(
    fs.readFileSync(new URL('./testdata/ci-media/sources.json', import.meta.url), 'utf8'),
  ),
}
const SOURCES = mediaSources(CONFIG)
import { validateProofMedia } from './verify-proof-media.mjs'

const REPO = 'freshclaim/fresh-claim'
const HEAD = 'a'.repeat(40)
const OTHER = 'b'.repeat(40)
const SECRET = 'ghp_SECRETTOKENVALUE123'
const MiB = 1024 * 1024
const MANIFEST_NAME = 'proof-media.json'
const MOBILE = 'mobile-e2e-test.yml'
const WEB = 'e2e-test.yml'
const PDF = 'pdf-test.yml'
const RUN_IDS = { [MOBILE]: 7, [PDF]: 9, [WEB]: 8 }
const source = (suite) => SOURCES.find((entry) => entry.suite === suite)
const nameOf = (suite) => source(suite).artifact.replaceAll('{head}', HEAD)
const runUrl = (id) => `https://github.com/${REPO}/actions/runs/${id}`
const CHANGED = 'services/mobile/src/components/PhotoCard.tsx'
const ids = (list) => list.map(({ id }) => id)
const SPEC = 'e2e/mobile/tests/stickers/sticker-card.spec.ts'

function tempDir(t, prefix) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), prefix))
  t.after(() => fs.rmSync(dir, { force: true, recursive: true, maxRetries: 3 }))
  return dir
}

const clip = (id, more = {}) => ({
  chapters: [{ atMs: 900, caption: 'The card is compact' }],
  durationMs: 9000,
  file: `${id}.mp4`,
  id,
  side: 'head',
  stills: [{ atMs: 1200, caption: 'Compact', file: `${id}-still-0.png` }],
  title: `Clip ${id}`,
  ...more,
})

const doc = (id, more = {}) => ({
  file: `${id}.pdf`,
  id,
  pageCount: 1,
  pages: [{ caption: 'Page 1', file: `${id}-p1.png` }],
  side: 'head',
  title: 'Doctor monthly statement',
  ...more,
})

const manifest = ({ clips = [], documents, head = HEAD, run = 7 } = {}) => ({
  clips,
  ...(documents ? { documents } : {}),
  head,
  schema: 'proof-media/1',
  source: { kind: 'ci-artifact', url: runUrl(run) },
  transcripts: [],
})

// An artifact's files: its manifest, every file it names (content = the name, or `sizes[name]`
// bytes), its sidecar and any `extra` entries. The sidecar names the clips `proves` lists, every
// clip by default, each proving CHANGED, and the `plain` ones; `proves: null` writes none.
function artifactFiles({ extra = {}, plain = [], proves, sizes = {}, ...spec }) {
  const media = manifest(spec)
  const named = [
    ...media.clips.flatMap((c) => [
      c.file,
      ...(c.poster ? [c.poster] : []),
      ...c.stills.map((s) => s.file),
    ]),
    ...(media.documents ?? []).flatMap((d) => [d.file, ...d.pages.map((p) => p.file)]),
  ]
  return {
    ...Object.fromEntries(named.map((file) => [file, sizes[file] ? { size: sizes[file] } : file])),
    [MANIFEST_NAME]: JSON.stringify(media),
    ...(proves === null
      ? {}
      : {
          [SIDECAR]: JSON.stringify({
            clips: Object.fromEntries(
              (proves ?? ids(media.clips)).map((id) => [id, { files: [CHANGED], spec: SPEC }]),
            ),
            plain,
            schema: SIDECAR_SCHEMA,
          }),
        }),
    ...extra,
  }
}
const STARTED = '2026-11-10T00:00:00Z'
const job = (name, conclusion, status = 'completed') => ({
  conclusion: status === 'completed' ? conclusion : null,
  name,
  started_at: STARTED,
  status,
})
const artifact = (name, more = {}) => ({
  created_at: '2026-11-10T00:05:00Z',
  expired: false,
  id: 1,
  name,
  ...more,
})

// One run per workflow with each source's job and artifact. `suites` maps a suite to its
// artifactFiles spec; `conclusions` maps a suite to its job's conclusion (default success).
function ci(suites, { conclusions = {} } = {}) {
  const runs = {}
  const files = {}
  for (const { artifact: name, job: jobName, suite, workflow } of SOURCES) {
    runs[workflow] ??= [{ artifacts: [], id: RUN_IDS[workflow], jobs: [] }]
    runs[workflow][0].jobs.push(job(jobName, conclusions[suite] ?? 'success'))
    if (!suites[suite]) continue
    runs[workflow][0].artifacts.push(artifact(name.replaceAll('{head}', HEAD)))
    files[name.replaceAll('{head}', HEAD)] = artifactFiles({
      run: RUN_IDS[workflow],
      ...suites[suite],
    })
  }
  return fakeGh({ files, runs })
}

// A gh stand-in. `runs` maps a workflow file to its runs (`{id, jobs, artifacts, head?, prs?}`);
// `files` maps an artifact name to what `gh run download` unpacks for it.
function fakeGh({ fail, files = {}, runs = {} } = {}) {
  const calls = []
  const allRuns = Object.values(runs).flat()
  const gh = (args) => {
    calls.push(args)
    if (fail) return { status: 1, stdout: SECRET }
    if (args[0] === 'run' && args[1] === 'download') {
      const [, , id, , repo, , name, , dir] = args
      assert.equal(repo, REPO)
      assert.ok(allRuns.some((run) => run.id === Number(id)))
      for (const [file, content] of Object.entries(files[name] ?? {})) {
        const to = path.join(dir, file)
        fs.mkdirSync(path.dirname(to), { recursive: true })
        if (content?.symlink) fs.symlinkSync(content.symlink, to)
        else if (content?.size) {
          fs.writeFileSync(to, '')
          fs.truncateSync(to, content.size)
        } else fs.writeFileSync(to, content)
      }
      return { status: 0, stdout: '' }
    }
    assert.equal(args[0], 'api')
    const url = new URL(args[1], 'https://api.github.com/')
    const listed = /^\/repos\/freshclaim\/fresh-claim\/actions\/workflows\/([^/]+)\/runs$/.exec(
      url.pathname,
    )
    if (listed) {
      assert.equal(url.searchParams.get('head_sha'), HEAD)
      assert.equal(url.searchParams.get('event'), 'pull_request')
      return {
        status: 0,
        stdout: JSON.stringify({
          workflow_runs: (runs[listed[1]] ?? []).map((run) => ({
            head_sha: run.head ?? HEAD,
            id: run.id,
            pull_requests: (run.prs ?? [12]).map((number) => ({ number })),
          })),
        }),
      }
    }
    const match = /^\/repos\/freshclaim\/fresh-claim\/actions\/runs\/(\d+)\/(jobs|artifacts)$/.exec(
      url.pathname,
    )
    assert.ok(match, `unexpected gh api ${args[1]}`)
    const run = allRuns.find((entry) => entry.id === Number(match[1]))
    if (match[2] === 'jobs') return { status: 0, stdout: JSON.stringify({ jobs: run.jobs ?? [] }) }
    const name = url.searchParams.get('name')
    return {
      status: 0,
      stdout: JSON.stringify({
        artifacts: (run.artifacts ?? []).filter((a) => a.name === name),
      }),
    }
  }
  return { calls, gh }
}

function run(t, gh, envelope = {}) {
  const evidenceDir = path.join(tempDir(t, 'fc-proof-media-evidence-'), 'media')
  let stdout = ''
  let stderr = ''
  const code = mediaHookMain({
    gh,
    run: () => ({ status: 0, stdout: JSON.stringify(CONFIG) }),
    log: (text) => {
      stderr += text
    },
    stdinText: JSON.stringify({
      baseSha: OTHER,
      evidenceDir,
      extension: 'boss-verify-fc-proof-media',
      headSha: HEAD,
      pr: 12,
      repo: REPO,
      role: 'proof-media',
      ...envelope,
    }),
    write: (text) => {
      stdout += text
    },
  })
  return {
    code,
    evidenceDir,
    manifest: stdout ? JSON.parse(stdout) : null,
    stderr,
    stdout,
  }
}

/** Every file under `dir`, relative, sorted; [] when it does not exist. */
function tree(dir) {
  if (!fs.existsSync(dir)) return []
  return fs
    .readdirSync(dir, { recursive: true })
    .filter((file) => fs.statSync(path.join(dir, file)).isFile())
    .sort()
}

/** What the core keeps from the printed manifest, sizing files in evidenceDir as it does. */
function coreKeeps(out) {
  return validateProofMedia(out.manifest, {
    head: HEAD,
    sizeOf: (file) => {
      try {
        return fs.statSync(path.join(out.evidenceDir, file)).size
      } catch {
        return null
      }
    },
  })
}

test('a passing mobile capture puts its manifest on stdout, each clip tagged with the files it proves, and its files under mobile/', (t) => {
  const out = run(t, ci({ mobile: { clips: [clip('head-0')] } }).gh)
  assert.equal(out.code, 0)
  assert.deepEqual(
    out.manifest,
    manifest({
      clips: [
        clip('head-0', {
          file: 'mobile/head-0.mp4',
          files: [CHANGED],
          stills: [
            {
              atMs: 1200,
              caption: 'Compact',
              file: 'mobile/head-0-still-0.png',
            },
          ],
        }),
      ],
    }),
  )
  assert.deepEqual(tree(out.evidenceDir), ['mobile/head-0-still-0.png', 'mobile/head-0.mp4'])
  assert.equal(
    fs.readFileSync(path.join(out.evidenceDir, 'mobile/head-0.mp4'), 'utf8'),
    'head-0.mp4',
  )
  assert.deepEqual(
    coreKeeps(out).media.clips[0].files,
    [CHANGED],
    'the core keeps the files the page filters on',
  )
})

test('mobile, admin and pdf merge into one manifest, staged per suite', (t) => {
  const out = run(
    t,
    ci({
      admin: { clips: [clip('admin-0')] },
      mobile: { clips: [clip('head-0')] },
      pdf: { documents: [doc('pdf-statement')] },
    }).gh,
  )
  assert.equal(out.code, 0)
  assert.deepEqual(ids(out.manifest.clips), ['head-0', 'admin-0'])
  assert.deepEqual(out.manifest.documents, [
    doc('pdf-statement', {
      file: 'pdf/pdf-statement.pdf',
      pages: [{ caption: 'Page 1', file: 'pdf/pdf-statement-p1.png' }],
    }),
  ])
  assert.deepEqual(out.manifest.source, {
    kind: 'ci-artifact',
    url: runUrl(7),
  })
  assert.deepEqual(tree(out.evidenceDir), [
    'admin/admin-0-still-0.png',
    'admin/admin-0.mp4',
    'mobile/head-0-still-0.png',
    'mobile/head-0.mp4',
    'pdf/pdf-statement-p1.png',
    'pdf/pdf-statement.pdf',
  ])
  assert.deepEqual(ids(coreKeeps(out).media.clips), ['head-0', 'admin-0'])
  assert.match(out.stderr, /dashboard: nothing to show \(no-artifact\)/)
})

test('a clip the sidecar leaves out, as it leaves out those only the label recorded, never reaches the manifest', (t) => {
  const out = run(
    t,
    ci({
      admin: {
        clips: [clip('admin-0'), clip('admin-1')],
        proves: ['admin-1'],
      },
      dashboard: { clips: [clip('dashboard-0')] },
      mobile: { clips: [clip('head-0')] },
    }).gh,
  )
  assert.deepEqual(ids(out.manifest.clips), ['head-0', 'admin-1', 'dashboard-0'])
  assert.ok(!tree(out.evidenceDir).some((file) => file.includes('admin-0')))
  assert.match(out.stderr, /admin: 1 of 2 clips prove no changed file/)
})

test('plain clips follow the proof clips of their tier, in every suite', (t) => {
  const out = run(
    t,
    ci({
      admin: {
        clips: [clip('admin-0'), clip('admin-1'), clip('admin-2')],
        plain: ['admin-1', 'admin-2'],
      },
      mobile: {
        clips: [clip('head-0'), clip('head-1')],
        plain: ['head-0'],
      },
    }).gh,
  )
  assert.deepEqual(ids(out.manifest.clips), ['head-1', 'admin-0', 'head-0', 'admin-1', 'admin-2'])
})

for (const [what, sidecar] of [
  ['a missing sidecar', null],
  ['an unparsable sidecar', '{"schema":'],
  [
    'a sidecar whose clip proves no file',
    JSON.stringify({
      clips: { 'admin-0': { files: [], spec: SPEC } },
      plain: [],
      schema: SIDECAR_SCHEMA,
    }),
  ],
  [
    'a sidecar of the older schema, which names no files',
    JSON.stringify({
      plain: [],
      reached: ['admin-0', 'admin-1'],
      schema: 'fc-proof-capture/1',
    }),
  ],
]) {
  test(`${what} shows none of its artifact's clips, and says so`, (t) => {
    const out = run(
      t,
      ci({
        admin: {
          clips: [clip('admin-0'), clip('admin-1')],
          ...(sidecar === null ? { proves: null } : { extra: { [SIDECAR]: sidecar } }),
        },
        dashboard: { clips: [clip('dashboard-0')] },
      }).gh,
    )
    assert.equal(out.code, 0)
    assert.deepEqual(ids(out.manifest.clips), ['dashboard-0'])
    assert.ok(!tree(out.evidenceDir).some((file) => file.startsWith('admin/')))
    assert.match(out.stderr, /admin: proof-capture\.json is missing or malformed; no clip is shown/)
  })
}

test('over 6 clips, the lowest-priority clip is the one dropped and never staged', (t) => {
  const out = run(
    t,
    ci({
      admin: {
        clips: [clip('admin-0'), clip('admin-1'), clip('admin-2')],
        plain: ['admin-0'],
      },
      dashboard: { clips: [clip('dashboard-0'), clip('dashboard-1')] },
      mobile: { clips: [clip('head-0'), clip('head-1')] },
    }).gh,
  )
  const { dropped, media } = coreKeeps(out)
  assert.deepEqual(ids(media.clips), [
    'head-0',
    'head-1',
    'admin-1',
    'admin-2',
    'dashboard-0',
    'dashboard-1',
  ])
  assert.deepEqual(dropped, [{ at: 'clips[6]', reason: 'over the cap of 6' }])
  assert.equal(out.manifest.clips[6].id, 'admin-0')
  assert.ok(!tree(out.evidenceDir).some((file) => file.includes('admin-0')))
})

test('a skipped pdf job contributes nothing while mobile still does', (t) => {
  const out = run(
    t,
    ci(
      {
        mobile: { clips: [clip('head-0')] },
        pdf: { documents: [doc('pdf-statement')] },
      },
      { conclusions: { pdf: 'skipped' } },
    ).gh,
  )
  assert.equal(out.code, 0)
  assert.deepEqual(ids(out.manifest.clips), ['head-0'])
  assert.equal('documents' in out.manifest, false)
  assert.ok(!tree(out.evidenceDir).some((file) => file.startsWith('pdf/')))
  assert.match(out.stderr, /pdf: nothing to show \(capture-skipped\)/)
})

test('a failed admin capture drops only admin, even uploaded; dashboard stays', (t) => {
  const { calls, gh } = ci(
    {
      admin: { clips: [clip('admin-0')] },
      dashboard: { clips: [clip('dashboard-0')] },
    },
    { conclusions: { admin: 'failure' } },
  )
  const out = run(t, gh)
  assert.equal(out.code, 0)
  assert.deepEqual(ids(out.manifest.clips), ['dashboard-0'])
  assert.deepEqual(tree(out.evidenceDir), [
    'dashboard/dashboard-0-still-0.png',
    'dashboard/dashboard-0.mp4',
  ])
  assert.match(out.stderr, /admin: nothing to show \(capture-failed\)/)
  const downloads = calls.filter((args) => args[0] === 'run')
  assert.deepEqual(
    downloads.map((args) => args[6]),
    [nameOf('dashboard')],
  )
})

test('the manifest source is the run that contributed the most items', (t) => {
  const most = run(
    t,
    ci({
      mobile: { clips: [clip('head-0')] },
      pdf: { documents: [doc('pdf-a'), doc('pdf-b')] },
    }).gh,
  )
  assert.deepEqual(most.manifest.source, {
    kind: 'ci-artifact',
    url: runUrl(RUN_IDS[PDF]),
  })
  const tie = run(
    t,
    ci({
      dashboard: { clips: [clip('dashboard-0')] },
      pdf: { documents: [doc('pdf-a')] },
    }).gh,
  )
  assert.deepEqual(
    tie.manifest.source,
    { kind: 'ci-artifact', url: runUrl(RUN_IDS[WEB]) },
    'a tie goes to the earlier suite',
  )
})

test('configured artifact names keep query characters as part of the name', () => {
  const configured = { ...source('mobile'), artifact: 'proof&clips #+-{head}' }
  const name = configured.artifact.replaceAll('{head}', HEAD)
  const { gh } = fakeGh({
    runs: {
      [MOBILE]: [{ id: 7, jobs: [job(configured.job, 'success')], artifacts: [artifact(name)] }],
    },
  })
  assert.deepEqual(findCaptures({ gh, head: HEAD, pr: 12, repo: REPO, sources: [configured] }), [
    { run: 7, source: configured },
  ])
})

test('admin and dashboard share one runs and one jobs lookup', (t) => {
  const { calls, gh } = ci({
    admin: { clips: [clip('admin-0')] },
    dashboard: { clips: [clip('dashboard-0')] },
  })
  const out = run(t, gh)
  assert.deepEqual(ids(out.manifest.clips), ['admin-0', 'dashboard-0'])
  const endpoints = calls
    .filter((args) => args[0] === 'api')
    .map((args) => new URL(args[1], 'https://api.github.com/').pathname)
  const count = (suffix) => endpoints.filter((endpoint) => endpoint.endsWith(suffix)).length
  assert.equal(count(`/workflows/${WEB}/runs`), 1)
  assert.equal(count(`/runs/${RUN_IDS[WEB]}/jobs`), 1)
})

test('two artifacts holding the same file name never overwrite each other', (t) => {
  const out = run(
    t,
    ci({
      admin: { clips: [clip('admin-0', { file: 'clip.mp4' })] },
      mobile: { clips: [clip('head-0', { file: 'clip.mp4' })] },
    }).gh,
  )
  assert.equal(fs.readFileSync(path.join(out.evidenceDir, 'mobile/clip.mp4'), 'utf8'), 'clip.mp4')
  assert.equal(fs.existsSync(path.join(out.evidenceDir, 'clip.mp4')), false)
  assert.equal(fs.existsSync(path.join(out.evidenceDir, 'admin/clip.mp4')), false)
  // The core names the collision on the page instead of serving one file as both.
  assert.deepEqual(coreKeeps(out).dropped, [
    {
      at: 'clips[1]',
      reason: 'file: media/clip.mp4 is already published from mobile/clip.mp4',
    },
  ])
})

test('a base and head document pair is staged with its pages', (t) => {
  const head = doc('pdf-statement')
  const base = doc('pdf-statement-base', {
    pairs: 'pdf-statement',
    side: 'base',
  })
  const out = run(t, ci({ pdf: { documents: [head, base] } }).gh)
  assert.deepEqual(
    out.manifest.documents.map(({ file, id, pairs }) => [id, file, pairs]),
    [
      ['pdf-statement', 'pdf/pdf-statement.pdf', undefined],
      ['pdf-statement-base', 'pdf/pdf-statement-base.pdf', 'pdf-statement'],
    ],
  )
  assert.deepEqual(out.manifest.clips, [])
  assert.deepEqual(out.manifest.source, {
    kind: 'ci-artifact',
    url: runUrl(9),
  })
  assert.deepEqual(tree(out.evidenceDir), [
    'pdf/pdf-statement-base-p1.png',
    'pdf/pdf-statement-base.pdf',
    'pdf/pdf-statement-p1.png',
    'pdf/pdf-statement.pdf',
  ])
})

test('an invalid document is listed but not staged, and the rest stay', (t) => {
  const out = run(
    t,
    ci({
      pdf: {
        documents: [
          doc('pdf-bad', { pageCount: 0 }),
          doc('pdf-good'),
          doc('pdf-orphan', { pairs: 'pdf-missing', side: 'base' }),
        ],
      },
    }).gh,
  )
  assert.deepEqual(ids(out.manifest.documents), ['pdf-bad', 'pdf-good', 'pdf-orphan'])
  assert.deepEqual(tree(out.evidenceDir), ['pdf/pdf-good-p1.png', 'pdf/pdf-good.pdf'])
})

test('clips and documents share the 60 MiB total, clips counted first', (t) => {
  const big = (id) => clip(id, { stills: [] })
  const clips = ['head-0', 'head-1', 'head-2', 'head-3'].map(big)
  const out = run(
    t,
    ci({
      mobile: {
        clips,
        sizes: Object.fromEntries(clips.map((c) => [c.file, 14 * MiB])),
      },
      pdf: {
        documents: [doc('pdf-large', { pages: [] }), doc('pdf-small', { pages: [] })],
        sizes: { 'pdf-large.pdf': 5 * MiB, 'pdf-small.pdf': MiB },
      },
    }).gh,
  )
  assert.deepEqual(tree(out.evidenceDir), [
    'mobile/head-0.mp4',
    'mobile/head-1.mp4',
    'mobile/head-2.mp4',
    'mobile/head-3.mp4',
    'pdf/pdf-small.pdf',
  ])
})

test('a PDF over 10 MiB and a page past the cap of 8 are dropped, never staged', (t) => {
  const pages = (id, count) =>
    Array.from({ length: count }, (_, i) => ({
      caption: `Page ${i + 1} of ${count}: changed`,
      file: `${id}-p${i + 1}.png`,
    }))
  const out = run(
    t,
    ci({
      pdf: {
        documents: [
          doc('pdf-big', { pages: pages('pdf-big', 1) }),
          doc('pdf-long', { pageCount: 9, pages: pages('pdf-long', 9) }),
        ],
        sizes: { 'pdf-big.pdf': 10 * MiB + 1 },
      },
    }).gh,
  )
  assert.deepEqual(tree(out.evidenceDir), [
    ...[1, 2, 3, 4, 5, 6, 7, 8].map((n) => `pdf/pdf-long-p${n}.png`),
    'pdf/pdf-long.pdf',
  ])
})

test('every source with nothing to show prints the empty manifest and exits 0', async (t) => {
  const failing = Object.fromEntries(SOURCES.map(({ suite }) => [suite, 'failure']))
  const cases = {
    'capture-failed': ci(
      {
        admin: { clips: [clip('admin-0')] },
        mobile: { clips: [clip('head-0')] },
      },
      { conclusions: failing },
    ),
    'no-artifact': ci({}),
    'no-run': fakeGh(),
  }
  for (const [why, { calls, gh }] of Object.entries(cases)) {
    await t.test(why, (t) => {
      const out = run(t, gh)
      assert.equal(out.code, 0)
      assert.deepEqual(out.manifest, emptyManifest(HEAD))
      for (const { suite } of SOURCES)
        assert.match(out.stderr, new RegExp(`${suite}: nothing to show \\(${why}\\)`))
      assert.equal(fs.existsSync(out.evidenceDir), false)
      assert.ok(!calls.some((args) => args[0] === 'run'), 'nothing downloaded')
    })
  }
})

test('the mobile run selection keeps its rules', async (t) => {
  const files = {
    [nameOf('mobile')]: artifactFiles({ clips: [clip('head-0')] }),
  }
  const name = nameOf('mobile')
  const success = job('proof-capture', 'success')
  const cases = {
    'capture-skipped': [{ id: 7, jobs: [job('proof-capture', 'skipped')] }],
    'no-artifact (expired)': [
      {
        artifacts: [artifact(name, { expired: true })],
        id: 7,
        jobs: [success],
      },
    ],
    'no-artifact (from an earlier attempt)': [
      {
        artifacts: [artifact(name, { created_at: '2026-11-09T23:55:00Z' })],
        id: 7,
        jobs: [success],
      },
    ],
    'no-run (another PR)': [{ artifacts: [artifact(name)], id: 7, jobs: [success], prs: [99] }],
    'no-run (another head)': [{ artifacts: [artifact(name)], head: OTHER, id: 7, jobs: [success] }],
    'no-run (capture still running)': [
      { id: 7, jobs: [job('proof-capture', null, 'in_progress')] },
    ],
  }
  for (const [why, runs] of Object.entries(cases)) {
    await t.test(why, (t) => {
      const out = run(t, fakeGh({ files, runs: { [MOBILE]: runs } }).gh)
      assert.equal(out.code, 0)
      assert.deepEqual(out.manifest, emptyManifest(HEAD))
      assert.match(out.stderr, new RegExp(`mobile: nothing to show \\(${why.split(' ')[0]}\\)`))
    })
  }
})

test('the newest finished capture decides, passing over one still running', (t) => {
  const files = {
    [nameOf('mobile')]: artifactFiles({ clips: [clip('head-0')] }),
  }
  const passed = {
    artifacts: [artifact(nameOf('mobile'))],
    id: 7,
    jobs: [job('proof-capture', 'success')],
  }
  const queued = fakeGh({
    files,
    runs: {
      // A fork PR's run lists no pull_requests.
      [MOBILE]: [
        { id: 9, jobs: [job('proof-capture', null, 'queued')] },
        { ...passed, prs: [] },
      ],
    },
  })
  assert.deepEqual(ids(run(t, queued.gh).manifest.clips), ['head-0'])

  // A label event starts a run for the same head that skips the capture.
  const skippedLater = fakeGh({
    files,
    runs: {
      [MOBILE]: [passed, { id: 8, jobs: [job('proof-capture', 'skipped')] }],
    },
  })
  assert.deepEqual(ids(run(t, skippedLater.gh).manifest.clips), ['head-0'])

  const failedLater = fakeGh({
    files,
    runs: {
      [MOBILE]: [passed, { id: 8, jobs: [job('proof-capture', 'failure')] }],
    },
  })
  assert.deepEqual(run(t, failedLater.gh).manifest, emptyManifest(HEAD))
})

test('a broken artifact in any source exits 1 with stdout empty', async (t) => {
  const good = artifactFiles({ clips: [clip('head-0')] })
  const cases = {
    'another head': {
      [MANIFEST_NAME]: JSON.stringify(manifest({ head: OTHER })),
    },
    'another schema': {
      [MANIFEST_NAME]: JSON.stringify({
        ...manifest(),
        schema: 'proof-media/2',
      }),
    },
    'no manifest': { 'admin-0.mp4': 'mp4 bytes' },
    'not JSON': { [MANIFEST_NAME]: '{' },
  }
  for (const [name, files] of Object.entries(cases)) {
    await t.test(name, (t) => {
      const { gh } = fakeGh({
        files: { [nameOf('admin')]: files, [nameOf('mobile')]: good },
        runs: {
          [MOBILE]: [
            {
              artifacts: [artifact(nameOf('mobile'))],
              id: 7,
              jobs: [job('proof-capture', 'success')],
            },
          ],
          [WEB]: [
            {
              artifacts: [artifact(nameOf('admin'))],
              id: 8,
              jobs: [job(source('admin').job, 'success')],
            },
          ],
        },
      })
      const out = run(t, gh)
      assert.equal(out.code, 1)
      assert.equal(out.stdout, '')
      assert.match(out.stderr, /the admin artifact/)
    })
  }
})

test('a file that leaves its artifact is dropped, never copied', (t) => {
  const outside = path.join(tempDir(t, 'fc-proof-media-outside-'), 'secret.mp4')
  fs.writeFileSync(outside, 'not from the artifact')
  const out = run(
    t,
    ci({
      mobile: {
        clips: [clip('head-0')],
        extra: { 'head-0.mp4': { symlink: outside } },
      },
    }).gh,
  )
  assert.equal(out.code, 0)
  assert.deepEqual(tree(out.evidenceDir), [])
  const { dropped, media } = coreKeeps(out)
  assert.deepEqual(media.clips, [])
  assert.deepEqual(dropped, [{ at: 'clips[0]', reason: 'file: mobile/head-0.mp4 is missing' }])
})

test('a gh failure exits 1 and never echoes what gh printed', (t) => {
  const out = run(t, fakeGh({ fail: true }).gh)
  assert.equal(out.code, 1)
  assert.equal(out.stdout, '')
  assert.ok(!out.stderr.includes(SECRET))
})

test('an unusable envelope exits 1 before any gh call', async (t) => {
  const cases = {
    'evidenceDir relative': { evidenceDir: 'media' },
    'headSha short': { headSha: 'abc123' },
    'repo not owner/repo': { repo: 'fresh-claim' },
    'role wrong': { role: 'proof-publish' },
  }
  for (const [name, envelope] of Object.entries(cases)) {
    await t.test(name, (t) => {
      const { calls, gh } = fakeGh()
      const out = run(t, gh, envelope)
      assert.equal(out.code, 1)
      assert.equal(out.stdout, '')
      assert.deepEqual(calls, [])
    })
  }
})

test('--merge-local merges local artifact directories the same way', (t) => {
  const root = tempDir(t, 'fc-proof-media-local-')
  const unpack = (suite, spec) => {
    const dir = path.join(root, suite)
    for (const [file, content] of Object.entries(artifactFiles(spec))) {
      fs.mkdirSync(path.dirname(path.join(dir, file)), { recursive: true })
      fs.writeFileSync(path.join(dir, file), content)
    }
    return dir
  }
  const admin = unpack('admin', {
    clips: [clip('admin-0'), clip('admin-1')],
    plain: ['admin-0'],
  })
  const pdf = unpack('pdf', { documents: [doc('pdf-statement')] })
  const out = path.join(root, 'merged')
  let stdout = ''
  const configPath = path.join(root, 'config.json')
  fs.writeFileSync(configPath, JSON.stringify(CONFIG))
  const code = mergeLocalMain(
    [
      '--merge-local',
      '--config',
      configPath,
      `admin=${admin}`,
      `pdf=${pdf}`,
      '--head',
      HEAD,
      '--out',
      out,
    ],
    {
      log: () => {},
      write: (text) => {
        stdout += text
      },
    },
  )
  assert.equal(code, 0)
  const merged = JSON.parse(stdout)
  assert.deepEqual(ids(merged.clips), ['admin-1', 'admin-0'])
  assert.deepEqual(ids(merged.documents), ['pdf-statement'])
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(out, MANIFEST_NAME), 'utf8')), merged)
  assert.deepEqual(tree(out), [
    'admin/admin-0-still-0.png',
    'admin/admin-0.mp4',
    'admin/admin-1-still-0.png',
    'admin/admin-1.mp4',
    'pdf/pdf-statement-p1.png',
    'pdf/pdf-statement.pdf',
    MANIFEST_NAME,
  ])

  const bad = mergeLocalMain(
    [
      '--merge-local',
      '--config',
      configPath,
      `web=${admin}`,
      '--head',
      HEAD,
      '--out',
      out,
      '--config',
      configPath,
    ],
    { log: () => {}, write: () => assert.fail('nothing on stdout') },
  )
  assert.equal(bad, 1)
})

import './verify-ci-media-parity.test.mjs'
