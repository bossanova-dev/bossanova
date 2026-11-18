import assert from 'node:assert/strict'
import { createHash } from 'node:crypto'
import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { test, after } from 'node:test'
import { fileURLToPath } from 'node:url'
import * as mediaContract from './verify-proof-media.mjs'
import * as captureContract from './verify-ci-media.mjs'
import { createProof } from './verify-proof-steps.mjs'

import {
  buildMedia,
  CAPS,
  cliMain,
  MANIFEST,
  SCHEMA,
  SIDECAR_SCHEMA,
  SIDECAR,
} from './verify-proof-capture.mjs'

const HEAD = 'a'.repeat(40)
const MB = 1024 * 1024

const scratchDirs = []
const scratch = () => {
  const dir = fs.mkdtempSync(path.join(tmpdir(), 'proof-capture-'))
  scratchDirs.push(dir)
  return dir
}
after(() => {
  for (const dir of scratchDirs) fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
})

/**
 * A proof-results directory as Playwright writes it: one folder per test with a video and its
 * stills, and a report.json that, like the real reporter, repeats every result annotation on the
 * test. Each test is `{title, status, file, chapters, stills, videoBytes, endShot}`, with
 * `endShot` adding the screenshot the proof configs take as each test ends, and chapters and
 * stills as `{atMs, caption}` and `file` its spec relative to the testDir. The directory is also
 * the repo root: the testDir is its `e2e/tests`, recorded as the report's `config.rootDir`, and
 * each spec file exists there.
 */
function proofRun(tests) {
  const dir = scratch()
  const testDir = path.join(dir, 'e2e', 'tests')
  const titleOf = new Map()
  const specs = tests.map((t, index) => {
    const folder = path.join(dir, `test-${index}`)
    fs.mkdirSync(folder)
    const video = path.join(folder, 'video.webm')
    fs.writeFileSync(video, '')
    fs.truncateSync(video, t.videoBytes ?? 1000)
    titleOf.set(video, t.title)
    const annotations = [
      ...(t.chapters ?? []).map(({ atMs, caption }) => ({
        description: JSON.stringify({ atMs, caption }),
        type: 'proof-chapter',
      })),
      ...(t.stills ?? []).map(({ atMs, caption, bytes }, n) => {
        const file = path.join(folder, `still-${n}.png`)
        fs.writeFileSync(file, `still ${index}-${n}`)
        if (bytes) fs.truncateSync(file, bytes)
        return {
          description: JSON.stringify({ atMs, caption, file }),
          type: 'proof-still',
        }
      }),
    ]
    const endShot = path.join(folder, 'test-finished-1.png')
    if (t.endShot) fs.writeFileSync(endShot, `end of ${index}`)
    const file = t.file ?? 'stickers/sticker-card.spec.ts'
    fs.mkdirSync(path.dirname(path.join(testDir, file)), { recursive: true })
    fs.writeFileSync(path.join(testDir, file), '')
    return {
      file,
      line: t.line ?? 10 * (index + 1),
      tests: [
        {
          annotations,
          results: [
            {
              annotations,
              attachments: [
                ...(t.noVideo ? [] : [{ contentType: 'video/webm', name: 'video', path: video }]),
                ...(t.endShot
                  ? [
                      {
                        contentType: 'image/png',
                        name: 'screenshot',
                        path: endShot,
                      },
                    ]
                  : []),
              ],
              status: t.status ?? 'passed',
            },
          ],
          status: t.status === 'failed' ? 'unexpected' : 'expected',
        },
      ],
      title: t.title,
    }
  })
  fs.writeFileSync(
    path.join(dir, 'report.json'),
    JSON.stringify({
      config: { rootDir: testDir },
      suites: [{ specs: [], suites: [{ specs }] }],
    }),
  )
  return { dir, titleOf }
}

/** Stands in for ffmpeg: copies the video's size, and reports the duration given per title. */
const fakeTools = (titleOf, durations = {}) => {
  const sourceOf = new Map()
  return {
    durationMs: (file) => durations[titleOf.get(sourceOf.get(file))] ?? 20_000,
    transcode: (from, to) => {
      sourceOf.set(to, from)
      fs.writeFileSync(to, '')
      fs.truncateSync(to, fs.statSync(from).size)
    },
  }
}

const run = (tests, { durations, prefix, reached } = {}) => {
  const { dir: resultsDir, titleOf } = proofRun(tests)
  const outDir = path.join(scratch(), 'media')
  const built = buildMedia({
    head: HEAD,
    outDir,
    prefix,
    reached,
    repoRoot: resultsDir,
    resultsDir,
    tools: fakeTools(titleOf, durations),
  })
  return { ...built, files: fs.readdirSync(outDir).sort(), outDir }
}

const sticker = {
  chapters: [
    { atMs: 1942, caption: 'The empty card is compact' },
    { atMs: 7591, caption: 'A photo fills the card, with Remove' },
  ],
  stills: [
    { atMs: 2508, caption: 'Empty: half height, no heading' },
    { atMs: 8211, caption: 'Photo taken: full height' },
  ],
  title: 'shows the photo with Remove',
}

test('a passing test becomes one clip: chapters 0.8 s earlier, stills copied beside the mp4', () => {
  const { files, manifest, outDir } = run([sticker])

  assert.deepEqual(manifest, {
    clips: [
      {
        chapters: [
          { atMs: 1142, caption: 'The empty card is compact' },
          { atMs: 6791, caption: 'A photo fills the card, with Remove' },
        ],
        durationMs: 20_000,
        file: 'head-0.mp4',
        id: 'head-0',
        poster: 'head-0-still-1.png',
        side: 'head',
        stills: [
          {
            atMs: 2508,
            caption: 'Empty: half height, no heading',
            file: 'head-0-still-0.png',
          },
          {
            atMs: 8211,
            caption: 'Photo taken: full height',
            file: 'head-0-still-1.png',
          },
        ],
        title: 'shows the photo with Remove',
      },
    ],
    head: HEAD,
    schema: 'proof-media/1',
    transcripts: [],
  })
  assert.deepEqual(files, ['head-0-still-0.png', 'head-0-still-1.png', 'head-0.mp4', MANIFEST])
  assert.equal(fs.readFileSync(path.join(outDir, 'head-0-still-1.png'), 'utf8'), 'still 0-1')
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(outDir, MANIFEST), 'utf8')), manifest)
})

test('annotations the report repeats on the test are read once, from the result', () => {
  const { manifest } = run([sticker])

  assert.equal(manifest.clips[0].chapters.length, 2)
  assert.equal(manifest.clips[0].stills.length, 2)
})

// The proof configs retry once, and the report keeps every attempt of a test.
test('a test that passed on its retry is clipped from the passing attempt only', () => {
  const { dir: resultsDir, titleOf } = proofRun([{ ...sticker, videoBytes: 2000 }])
  const reportFile = path.join(resultsDir, 'report.json')
  const report = JSON.parse(fs.readFileSync(reportFile, 'utf8'))
  const theTest = report.suites[0].suites[0].specs[0].tests[0]
  const failedVideo = path.join(resultsDir, 'failed-attempt.webm')
  const failedStill = path.join(resultsDir, 'failed-still.png')
  fs.writeFileSync(failedVideo, '')
  fs.truncateSync(failedVideo, 500)
  fs.writeFileSync(failedStill, 'still of the failed attempt')
  titleOf.set(failedVideo, sticker.title)
  theTest.results.unshift({
    annotations: [
      {
        description: JSON.stringify({ atMs: 100, caption: 'From the failure' }),
        type: 'proof-chapter',
      },
      {
        description: JSON.stringify({
          atMs: 200,
          caption: 'Failed attempt',
          file: failedStill,
        }),
        type: 'proof-still',
      },
    ],
    attachments: [{ contentType: 'video/webm', name: 'video', path: failedVideo }],
    status: 'failed',
  })
  theTest.status = 'flaky'
  fs.writeFileSync(reportFile, JSON.stringify(report))
  const outDir = path.join(scratch(), 'media')

  const { manifest } = buildMedia({
    head: HEAD,
    outDir,
    repoRoot: resultsDir,
    resultsDir,
    tools: fakeTools(titleOf),
  })

  assert.equal(manifest.clips.length, 1)
  assert.deepEqual(
    manifest.clips[0].chapters.map(({ caption }) => caption),
    sticker.chapters.map(({ caption }) => caption),
  )
  assert.equal(fs.statSync(path.join(outDir, 'head-0.mp4')).size, 2000)
  assert.equal(fs.readFileSync(path.join(outDir, 'head-0-still-0.png'), 'utf8'), 'still 0-0')
})

test('a failed test contributes no clip and no file', () => {
  const { files, manifest, outDir, skipped } = run([
    { ...sticker, status: 'failed', title: 'fails half way' },
    sticker,
  ])

  assert.deepEqual(
    manifest.clips.map(({ id, title }) => [id, title]),
    [['head-0', 'shows the photo with Remove']],
  )
  assert.deepEqual(skipped, [{ reason: 'the test failed', title: 'fails half way' }])
  assert.deepEqual(files, ['head-0-still-0.png', 'head-0-still-1.png', 'head-0.mp4', MANIFEST])
  assert.equal(
    fs.readFileSync(path.join(outDir, 'head-0-still-0.png'), 'utf8'),
    'still 1-0',
    'the passing test is the second one, so its stills are the copies',
  )

  const onlyFailed = run([{ ...sticker, status: 'failed' }])
  assert.deepEqual(onlyFailed.manifest.clips, [])
  assert.deepEqual(onlyFailed.files, [MANIFEST])
})

test('a test with no proof steps is a plain clip after the proof clips: no chapters, one end still', () => {
  const { dropped, files, manifest, outDir, skipped } = run([
    { endShot: true, title: 'a plain test' },
    { ...sticker, endShot: true, title: 'proof' },
    { title: 'no screenshot' },
    { endShot: true, status: 'failed', title: 'plain and failed' },
  ])

  assert.deepEqual(
    manifest.clips.map(({ id, title }) => `${id} ${title}`),
    ['head-0 proof', 'head-1 a plain test', 'head-2 no screenshot'],
  )
  assert.equal(
    manifest.clips[0].stills.length,
    2,
    'a test with proof steps keeps its own stills, not the end screenshot',
  )
  assert.deepEqual(manifest.clips[1], {
    chapters: [],
    durationMs: 20_000,
    file: 'head-1.mp4',
    id: 'head-1',
    poster: 'head-1-still-0.png',
    side: 'head',
    stills: [
      {
        atMs: 19_999,
        caption: 'End of: a plain test',
        file: 'head-1-still-0.png',
      },
    ],
    title: 'a plain test',
  })
  assert.equal(fs.readFileSync(path.join(outDir, 'head-1-still-0.png'), 'utf8'), 'end of 0')
  assert.deepEqual(manifest.clips[2].stills, [])
  assert.ok(files.includes('head-2.mp4'))
  assert.deepEqual(skipped, [{ reason: 'the test failed', title: 'plain and failed' }])
  assert.deepEqual(dropped, [])
})

test('an end still caption over the cap is cut with an ellipsis', () => {
  const title = 't'.repeat(CAPS.captionChars)
  const { manifest } = run([{ endShot: true, title }])
  const [still] = manifest.clips[0].stills

  assert.equal(still.caption.length, CAPS.captionChars)
  assert.ok(still.caption.startsWith('End of: ttt'))
  assert.ok(still.caption.endsWith('…'))
})

test('chapters stay sorted, distinct and inside the clip; captions and counts stay under the caps', () => {
  const long = 'x'.repeat(CAPS.captionChars + 1)
  const { dropped, manifest } = run(
    [
      {
        chapters: [
          { atMs: 9000, caption: 'Third' },
          { atMs: 300, caption: 'Clamped to the start' },
          { atMs: 600, caption: 'Also clamped to the start' },
          { atMs: 5000, caption: long },
          { atMs: 4000, caption: 'Second' },
          { atMs: 12_000, caption: 'After the end' },
        ],
        stills: [
          ...Array.from({ length: CAPS.stills + 1 }, (_, n) => ({
            atMs: 1000 + n,
            caption: `Still ${n}`,
          })),
          { atMs: 2000, caption: long },
          { atMs: 10_500, caption: 'After the end' },
        ],
        title: `${'t'.repeat(CAPS.captionChars)}tail`,
      },
    ],
    { durations: { [`${'t'.repeat(CAPS.captionChars)}tail`]: 10_000 } },
  )
  const [clip] = manifest.clips

  assert.deepEqual(clip.chapters, [
    { atMs: 0, caption: 'Clamped to the start' },
    { atMs: 3200, caption: 'Second' },
    { atMs: 8200, caption: 'Third' },
  ])
  assert.equal(clip.stills.length, CAPS.stills)
  assert.equal(clip.stills.at(-1).caption, `Still ${CAPS.stills - 1}`)
  assert.equal(clip.poster, clip.stills.at(-1).file)
  assert.equal(clip.title.length, CAPS.captionChars)
  assert.ok(clip.title.endsWith('…'))
  assert.deepEqual(dropped, [
    'head-0 chapter "Also clamped to the start": starts with the chapter before it',
    `head-0 chapter "${long}": caption over ${CAPS.captionChars} characters`,
    'head-0 chapter "After the end": starts after the clip ends',
    `head-0 still "Still ${CAPS.stills}": over ${CAPS.stills} stills`,
    `head-0 still "${long}": caption over ${CAPS.captionChars} characters`,
    'head-0 still "After the end": taken after the clip ends',
  ])
})

test('clips stay under the count, length, file and total caps', () => {
  const big = { ...sticker, videoBytes: 14 * MB }
  const { dropped, files, manifest } = run(
    [
      { ...sticker, title: 'a minute long' },
      { ...sticker, title: 'too big', videoBytes: CAPS.fileBytes + 1 },
      {
        ...sticker,
        stills: [{ atMs: 100, bytes: CAPS.fileBytes + 1, caption: 'Huge' }],
        title: 'huge still',
      },
      { ...big, title: 'big 1' },
      { ...big, title: 'big 2' },
      { ...big, title: 'big 3' },
      { ...big, title: 'big 4' },
      { ...big, title: 'big 5' },
      ...[1, 2, 3].map((n) => ({ ...sticker, title: `small ${n}` })),
    ],
    { durations: { 'a minute long': CAPS.clipMs } },
  )

  assert.deepEqual(
    manifest.clips.map(({ id, title }) => `${id} ${title}`),
    [
      'head-0 huge still',
      'head-1 big 1',
      'head-2 big 2',
      'head-3 big 3',
      'head-4 big 4',
      'head-5 small 1',
    ],
  )
  assert.equal(manifest.clips[0].stills.length, 0)
  assert.equal(manifest.clips[0].poster, undefined)
  assert.deepEqual(dropped, [
    `"a minute long": runs ${CAPS.clipMs} ms, not under ${CAPS.clipMs} ms`,
    `"too big": the mp4 is over ${CAPS.fileBytes} bytes`,
    `head-0 still "Huge": over ${CAPS.fileBytes} bytes`,
    `"big 5": takes the media over ${CAPS.totalBytes} bytes`,
    `"small 2": over ${CAPS.clips} clips`,
    `"small 3": over ${CAPS.clips} clips`,
  ])
  assert.deepEqual(
    files.filter((file) => file.endsWith('.mp4')),
    ['head-0.mp4', 'head-1.mp4', 'head-2.mp4', 'head-3.mp4', 'head-4.mp4', 'head-5.mp4'],
  )
})

test('a rerun into its own output converges, and a foreign directory is refused', () => {
  const { dir: resultsDir, titleOf } = proofRun([sticker, { ...sticker, title: 'second' }])
  const outDir = path.join(scratch(), 'media')
  const tools = fakeTools(titleOf)
  const clean = () => fs.readdirSync(outDir)

  buildMedia({ head: HEAD, outDir, resultsDir, tools })
  assert.equal(clean().length, 7)

  fs.writeFileSync(
    path.join(resultsDir, 'report.json'),
    JSON.stringify({
      config: { rootDir: path.join(resultsDir, 'e2e', 'tests') },
      suites: [
        {
          specs: [],
          suites: [
            {
              specs: JSON.parse(
                fs.readFileSync(path.join(resultsDir, 'report.json'), 'utf8'),
              ).suites[0].suites[0].specs.slice(0, 1),
            },
          ],
        },
      ],
    }),
  )
  buildMedia({ head: HEAD, outDir, resultsDir, tools })
  assert.deepEqual(clean().sort(), [
    'head-0-still-0.png',
    'head-0-still-1.png',
    'head-0.mp4',
    MANIFEST,
  ])

  const foreign = scratch()
  fs.writeFileSync(path.join(foreign, 'notes.txt'), 'keep me')
  assert.throws(
    () => buildMedia({ head: HEAD, outDir: foreign, resultsDir, tools }),
    /is not empty and holds no proof-media\.json/,
  )
  assert.equal(fs.readFileSync(path.join(foreign, 'notes.txt'), 'utf8'), 'keep me')
})

test('a failed media build can be retried into the same output directory', () => {
  for (const failure of ['transcode', 'duration']) {
    const { dir: resultsDir, titleOf } = proofRun([sticker])
    const outDir = path.join(scratch(), 'media')
    const tools = fakeTools(titleOf)
    const args = { head: HEAD, outDir, resultsDir, tools }
    const failingTools = {
      durationMs: (file) => {
        if (failure === 'duration') throw new Error('media tool failed')
        return tools.durationMs(file)
      },
      transcode: (from, to) => {
        tools.transcode(from, to)
        if (failure === 'transcode') throw new Error('media tool failed')
      },
    }

    assert.throws(() => buildMedia({ ...args, tools: failingTools }), /media tool failed/)
    const { manifest } = buildMedia(args)
    assert.equal(manifest.clips.length, 1)
    assert.equal(manifest.clips[0].title, sticker.title)
    assert.ok(fs.existsSync(path.join(outDir, MANIFEST)))
  }
})

test('the CLI requires a full head sha and an https source URL, and records the source', () => {
  const { dir: resultsDir, titleOf } = proofRun([sticker])
  const out = path.join(scratch(), 'media')
  const tools = fakeTools(titleOf)

  assert.throws(
    () =>
      cliMain(['--results', resultsDir, '--out', out, '--head', 'abc123'], {
        tools,
      }),
    /--head must be a 40-character commit sha/,
  )
  assert.throws(
    () =>
      cliMain(
        ['--results', resultsDir, '--out', out, '--head', HEAD, '--source-url', 'http://x/'],
        { tools },
      ),
    /--source-url must be an https URL/,
  )

  const url = 'https://github.com/example/app/actions/runs/42'
  const summary = JSON.parse(
    cliMain(['--results', resultsDir, '--out', out, '--head', HEAD, '--source-url', url], {
      tools,
    }),
  )
  assert.deepEqual(summary, { clips: 1, dropped: [], skipped: [] })
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(out, MANIFEST), 'utf8')).source, {
    kind: 'ci-artifact',
    url,
  })
})

test('a prefix names every clip id and every file, and a bad prefix is refused', () => {
  const { files, manifest } = run([sticker, { ...sticker, title: 'second' }], {
    prefix: 'admin',
  })

  assert.deepEqual(
    manifest.clips.map(({ file, id, poster, stills }) => ({
      file,
      id,
      poster,
      stills: stills.map(({ file: still }) => still),
    })),
    [
      {
        file: 'admin-0.mp4',
        id: 'admin-0',
        poster: 'admin-0-still-1.png',
        stills: ['admin-0-still-0.png', 'admin-0-still-1.png'],
      },
      {
        file: 'admin-1.mp4',
        id: 'admin-1',
        poster: 'admin-1-still-1.png',
        stills: ['admin-1-still-0.png', 'admin-1-still-1.png'],
      },
    ],
  )
  assert.deepEqual(
    files.filter((file) => file !== MANIFEST && !file.startsWith('admin-')),
    [],
    'every media file starts with the prefix',
  )
  assert.equal(files.length, 7)

  assert.throws(
    () => run([sticker], { prefix: 'Admin 1' }),
    /the id prefix must be lower-case letters, digits and hyphens/,
  )
})

const userList = {
  ...sticker,
  file: 'users/user-list.spec.ts',
  title: 'filters the user list',
}
const referrers = {
  ...sticker,
  file: 'referrers/referrer-crud.spec.ts',
  title: 'adds a referrer',
}

const REFERRER_PAGE = 'services/admin/src/pages/referrers.tsx'
const USER_PAGE = 'services/admin/src/pages/users.tsx'

/** One entry of the repo's selector's --reached list, for a spec under the fixture's e2e/tests. */
const reach = (file, files = [REFERRER_PAGE], lines) => ({
  files,
  ...(lines ? { lines } : {}),
  spec: `e2e/tests/${file}`,
})
const REFERRERS_REACHED = reach('referrers/referrer-crud.spec.ts')

test('clips of reached specs come first, and the sidecar maps exactly their ids to the spec and its files', () => {
  const { files, manifest, outDir, sidecar } = run(
    [
      userList,
      { ...referrers, status: 'failed', title: 'fails' },
      referrers,
      { ...userList, title: 'filters again' },
    ],
    { prefix: 'admin', reached: [REFERRERS_REACHED] },
  )

  assert.deepEqual(
    manifest.clips.map(({ id, title }) => `${id} ${title}`),
    ['admin-0 adds a referrer', 'admin-1 filters the user list', 'admin-2 filters again'],
  )
  assert.deepEqual(sidecar, {
    clips: {
      'admin-0': {
        files: [REFERRER_PAGE],
        spec: 'e2e/tests/referrers/referrer-crud.spec.ts',
      },
    },
    plain: [],
    schema: 'proof-capture/1',
  })
  assert.ok(files.includes(SIDECAR))
  assert.deepEqual(JSON.parse(fs.readFileSync(path.join(outDir, SIDECAR), 'utf8')), sidecar)
})

test('reached clips win the clip cap over clips recorded only for proof-capture-all', () => {
  const others = Array.from({ length: CAPS.clips }, (_, n) => ({
    ...userList,
    title: `other ${n}`,
  }))
  const { dropped, manifest, sidecar } = run([...others, referrers], {
    reached: [REFERRERS_REACHED],
  })

  assert.equal(manifest.clips[0].title, 'adds a referrer')
  assert.deepEqual(Object.keys(sidecar.clips), ['head-0'])
  assert.deepEqual(dropped, [`"other ${CAPS.clips - 1}": over ${CAPS.clips} clips`])
})

test('reached proof clips, then reached plain ones, then the rest in the same order; the sidecar names the plain ids', () => {
  const plain = (spec, title) => ({ endShot: true, file: spec, title })
  const { dropped, manifest, sidecar } = run(
    [
      plain('users/user-list.spec.ts', 'unreached plain'),
      userList,
      plain('referrers/referrer-crud.spec.ts', 'reached plain'),
      referrers,
      ...Array.from({ length: CAPS.clips }, (_, n) =>
        plain('users/user-list.spec.ts', `more plain ${n}`),
      ),
    ],
    { reached: [REFERRERS_REACHED] },
  )

  assert.deepEqual(
    manifest.clips.map(({ id, title }) => `${id} ${title}`),
    [
      'head-0 adds a referrer',
      'head-1 reached plain',
      'head-2 filters the user list',
      'head-3 unreached plain',
      'head-4 more plain 0',
      'head-5 more plain 1',
    ],
  )
  assert.deepEqual(sidecar.plain, ['head-1', 'head-3', 'head-4', 'head-5'])
  assert.deepEqual(Object.keys(sidecar.clips), ['head-0', 'head-1'])
  assert.equal(dropped.length, CAPS.clips - 2)
})

test('reached clips follow the reached list, a plain spec ranked first before a proof spec after it', () => {
  const plain = (spec, title) => ({ endShot: true, file: spec, title })
  const { manifest, sidecar } = run(
    [
      referrers,
      plain('surgeons/surgeon-crud.spec.ts', 'unreached plain'),
      {
        ...userList,
        file: 'surgeons/surgeon-crud.spec.ts',
        title: 'unreached proof',
      },
      plain('users/user-list.spec.ts', 'narrowest plain'),
      userList,
    ],
    {
      reached: [reach('users/user-list.spec.ts', [USER_PAGE]), REFERRERS_REACHED],
    },
  )

  assert.deepEqual(
    manifest.clips.map(({ id, title }) => `${id} ${title}`),
    [
      'head-0 filters the user list',
      'head-1 narrowest plain',
      'head-2 adds a referrer',
      'head-3 unreached proof',
      'head-4 unreached plain',
    ],
  )
  assert.deepEqual(sidecar.clips, {
    'head-0': { files: [USER_PAGE], spec: 'e2e/tests/users/user-list.spec.ts' },
    'head-1': { files: [USER_PAGE], spec: 'e2e/tests/users/user-list.spec.ts' },
    'head-2': {
      files: [REFERRER_PAGE],
      spec: 'e2e/tests/referrers/referrer-crud.spec.ts',
    },
  })
})

test('a test the change touches is never cut by the clip cap, whatever its spec ranks', () => {
  const formsTest = (n) => ({
    ...userList,
    line: 10 * (n + 1),
    title: `form test ${n}`,
  })
  const added = { ...referrers, line: 70, title: 'the new referrer test' }
  const { dropped, manifest } = run(
    [
      ...Array.from({ length: CAPS.clips }, (_, n) => formsTest(n)),
      { ...referrers, line: 10, title: 'an old referrer test' },
      added,
    ],
    {
      reached: [
        reach('users/user-list.spec.ts', [USER_PAGE]),
        reach('referrers/referrer-crud.spec.ts', [REFERRER_PAGE], [[72, 80]]),
      ],
    },
  )

  assert.equal(manifest.clips[0].title, 'the new referrer test')
  assert.deepEqual(dropped, [
    `"form test ${CAPS.clips - 1}": over ${CAPS.clips} clips`,
    '"an old referrer test": over 6 clips',
  ])
})

test('all project variants of an edited test win the clip cap', () => {
  const { dir: resultsDir, titleOf } = proofRun([
    ...Array.from({ length: CAPS.clips }, (_, n) => ({ ...userList, title: `other ${n}` })),
    { ...referrers, line: 10, title: 'edited test' },
    { ...referrers, line: 10, title: 'edited test second project' },
    { ...referrers, line: 40, title: 'next test' },
  ])
  const reportFile = path.join(resultsDir, 'report.json')
  const report = JSON.parse(fs.readFileSync(reportFile, 'utf8'))
  const specs = report.suites[0].suites[0].specs
  specs[CAPS.clips].tests.push(...specs.splice(CAPS.clips + 1, 1)[0].tests)
  fs.writeFileSync(reportFile, JSON.stringify(report))

  const { manifest } = buildMedia({
    head: HEAD,
    outDir: path.join(scratch(), 'media'),
    reached: [
      reach('users/user-list.spec.ts', [USER_PAGE]),
      reach('referrers/referrer-crud.spec.ts', [REFERRER_PAGE], [[20, 22]]),
    ],
    repoRoot: resultsDir,
    resultsDir,
    tools: fakeTools(titleOf),
  })

  assert.deepEqual(
    manifest.clips.slice(0, 2).map(({ title }) => title),
    ['edited test', 'edited test'],
  )
  assert.equal(manifest.clips.length, CAPS.clips)
})

test('an empty reached list writes a sidecar that lists no clip', () => {
  const { files, manifest, sidecar } = run([userList], { reached: [] })

  assert.equal(manifest.clips.length, 1)
  assert.deepEqual(sidecar, {
    clips: {},
    plain: [],
    schema: 'proof-capture/1',
  })
  assert.ok(files.includes(SIDECAR))
})

test('without reached specs no sidecar is written, and a rerun drops an earlier one', () => {
  const { dir: resultsDir, titleOf } = proofRun([referrers])
  const outDir = path.join(scratch(), 'media')
  const tools = fakeTools(titleOf)
  const build = (reached) =>
    buildMedia({
      head: HEAD,
      outDir,
      reached,
      repoRoot: resultsDir,
      resultsDir,
      tools,
    })

  build([REFERRERS_REACHED])
  assert.ok(fs.existsSync(path.join(outDir, SIDECAR)))

  const { sidecar } = build(undefined)
  assert.equal(sidecar, null)
  assert.ok(!fs.existsSync(path.join(outDir, SIDECAR)))
  assert.ok(fs.existsSync(path.join(outDir, MANIFEST)))
})

test('a reached spec that names no file is refused rather than marking every clip unreached', () => {
  assert.throws(
    () =>
      run([referrers], {
        reached: [
          {
            files: [REFERRER_PAGE],
            spec: 'tests/referrers/referrer-crud.spec.ts',
          },
        ],
      }),
    /the reached spec tests\/referrers\/referrer-crud\.spec\.ts is not a file under/,
  )
})

test('the CLI reads --prefix and the --reached JSON file', () => {
  const { dir: resultsDir, titleOf } = proofRun([userList, referrers])
  const out = path.join(scratch(), 'media')
  const reachedFile = path.join(scratch(), 'reached.json')
  fs.writeFileSync(reachedFile, JSON.stringify([REFERRERS_REACHED]))
  const cwd = process.cwd()

  process.chdir(resultsDir)
  try {
    const summary = JSON.parse(
      cliMain(
        [
          '--results',
          resultsDir,
          '--out',
          out,
          '--head',
          HEAD,
          '--prefix',
          'dashboard',
          '--reached',
          reachedFile,
        ],
        { tools: fakeTools(titleOf) },
      ),
    )
    assert.deepEqual(summary, {
      clips: 2,
      dropped: [],
      reached: ['dashboard-0'],
      skipped: [],
    })
  } finally {
    process.chdir(cwd)
  }
  assert.deepEqual(
    JSON.parse(fs.readFileSync(path.join(out, MANIFEST), 'utf8')).clips.map(
      ({ id, title }) => `${id} ${title}`,
    ),
    ['dashboard-0 adds a referrer', 'dashboard-1 filters the user list'],
  )
})

test('missing video is skipped; chapters stop at twelve', () => {
  const { manifest, skipped, dropped } = run([
    { title: 'not recorded', noVideo: true },
    {
      title: 'many claims',
      chapters: Array.from({ length: 13 }, (_, n) => ({
        atMs: 1000 + n * 100,
        caption: `Claim ${n}`,
      })),
    },
  ])
  assert.deepEqual(skipped, [{ title: 'not recorded', reason: 'no video was recorded' }])
  assert.equal(manifest.clips.length, 1)
  assert.equal(manifest.clips[0].chapters.length, 12)
  assert.equal(manifest.clips[0].chapters.at(-1).caption, 'Claim 11')
  assert.deepEqual(dropped, ['head-0 chapter "Claim 12": over 12 chapters'])
})

test('builder literals agree with consumer contracts and output validates without drops', () => {
  assert.equal(SCHEMA, mediaContract.PROOF_MEDIA_SCHEMA)
  assert.equal(MANIFEST, captureContract.CAPTURE_MANIFEST)
  assert.equal(SIDECAR, captureContract.CAPTURE_SIDECAR)
  assert.equal(SIDECAR_SCHEMA, captureContract.CAPTURE_SIDECAR_SCHEMA)
  assert.deepEqual(CAPS, {
    captionChars: mediaContract.MAX_CAPTION_CHARS,
    chapters: mediaContract.MAX_CHAPTERS,
    clipMs: mediaContract.MAX_CLIP_MS,
    clips: mediaContract.MAX_CLIPS,
    fileBytes: mediaContract.MAX_FILE_BYTES,
    stills: mediaContract.MAX_STILLS,
    totalBytes: mediaContract.MAX_TOTAL_BYTES,
  })
  const { manifest, outDir, sidecar } = run(
    [referrers, { endShot: true, file: referrers.file, title: 'plain' }],
    { reached: [REFERRERS_REACHED] },
  )
  assert.deepEqual(
    mediaContract.validateProofMedia(manifest, {
      head: HEAD,
      sizeOf: (name) => fs.statSync(path.join(outDir, name)).size,
    }).dropped,
    [],
  )
  const read = captureContract.readSidecar(outDir)
  assert.deepEqual(Object.fromEntries(read.clips), sidecar.clips)
  assert.deepEqual([...read.plain], sidecar.plain)
  // Retain small sample artifacts for the caller's PR description.
  if (process.env.PROOF_CAPTURE_SAMPLE_DIR) {
    fs.mkdirSync(process.env.PROOF_CAPTURE_SAMPLE_DIR, { recursive: true })
    for (const name of [MANIFEST, SIDECAR])
      fs.copyFileSync(
        path.join(outDir, name),
        path.join(process.env.PROOF_CAPTURE_SAMPLE_DIR, name),
      )
  }
})

test('export is byte-identical, node-only, runnable directly and through a symlink, and inert on import', async () => {
  const capture = fileURLToPath(new URL('./verify-proof-capture.mjs', import.meta.url))
  const steps = fileURLToPath(new URL('./verify-proof-steps.mjs', import.meta.url))
  const out = scratch()
  const exported = spawnSync(process.execPath, [capture, 'export', '--out', out], {
    encoding: 'utf8',
  })
  assert.equal(exported.status, 0, exported.stderr)
  const digest = async (file) => {
    const hash = createHash('sha256')
    for await (const bytes of fs.createReadStream(file)) hash.update(bytes)
    return hash.digest('hex')
  }
  for (const file of [capture, steps])
    assert.equal(await digest(path.join(out, path.basename(file))), await digest(file))
  const copied = path.join(out, path.basename(capture))
  // Execute the copied modules with a loader that refuses dependencies outside their contract.
  const importsOnlyBuiltins = `
    import { registerHooks } from 'node:module'
    import { pathToFileURL } from 'node:url'
    const [capture, steps] = process.argv.slice(1).map((file) => pathToFileURL(file).href)
    registerHooks({
      resolve(specifier, context, nextResolve) {
        if (context.parentURL === steps ||
            (context.parentURL === capture && !specifier.startsWith('node:')))
          throw new Error('unexpected dependency: ' + specifier)
        return nextResolve(specifier, context)
      },
    })
    await import(capture)
    await import(steps)
  `
  const dependencyCheck = spawnSync(
    process.execPath,
    [
      '--input-type=module',
      '-e',
      importsOnlyBuiltins,
      copied,
      path.join(out, path.basename(steps)),
    ],
    { encoding: 'utf8' },
  )
  assert.equal(dependencyCheck.status, 0, dependencyCheck.stderr)
  assert.equal(dependencyCheck.stdout, '')
  const link = path.join(out, 'capture-link.mjs')
  fs.symlinkSync(copied, link)
  const viaLink = spawnSync(process.execPath, [link, 'export', '--out', scratch()], {
    encoding: 'utf8',
  })
  assert.equal(viaLink.status, 0, viaLink.stderr)
  const imported = spawnSync(
    process.execPath,
    ['--input-type=module', '-e', 'await import(process.argv[1])', copied],
    { encoding: 'utf8' },
  )
  assert.equal(imported.status, 0, imported.stderr)
  assert.equal(imported.stdout, '')
  const { dir: resultsDir } = proofRun([])
  const built = spawnSync(
    process.execPath,
    [copied, '--results', resultsDir, '--out', path.join(scratch(), 'media'), '--head', HEAD],
    { encoding: 'utf8' },
  )
  assert.equal(built.status, 0, built.stderr)
  assert.deepEqual(JSON.parse(built.stdout), { clips: 0, dropped: [], skipped: [] })
  assert.throws(() => cliMain(['--unknown']), /Unknown option/)
  assert.throws(() => cliMain(['export', '--out', out, '--head', HEAD]), /usage:/)
})

test('steps annotations survive a Playwright result into builder chapters and stills', async () => {
  const { dir: resultsDir, titleOf } = proofRun([{ title: 'proof from steps' }])
  const annotations = []
  const proof = createProof({
    test: { info: () => ({ annotations, outputPath: (name) => path.join(resultsDir, name) }) },
    enabled: true,
    pacing: { leadInMs: 0, holdStateMs: 0 },
  })
  const page = {
    waitForTimeout: async () => {},
    evaluate: async () => {},
    screenshot: async ({ path: file }) => fs.writeFileSync(file, 'still'),
  }
  const locator = {
    scrollIntoViewIfNeeded: async () => {},
    boundingBox: async () => ({ x: 0, y: 0, width: 1, height: 1 }),
  }
  proof.begin()
  await proof.chapter(page, 'Claim')
  await proof.show(page, locator, 'Result')
  const file = path.join(resultsDir, 'report.json')
  const report = JSON.parse(fs.readFileSync(file, 'utf8'))
  report.suites[0].suites[0].specs[0].tests[0].results[0].annotations = annotations
  fs.writeFileSync(file, JSON.stringify(report))
  const { manifest } = buildMedia({
    head: HEAD,
    resultsDir,
    outDir: path.join(scratch(), 'media'),
    tools: fakeTools(titleOf),
  })
  assert.deepEqual(manifest.clips[0].chapters, [{ atMs: 0, caption: 'Claim' }])
  assert.equal(manifest.clips[0].stills[0].caption, 'Result')
})
