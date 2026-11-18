/**
 * Self-contained Playwright JSON report to proof-media/1 and proof-capture/1.
 * Copy this module and the import-free steps helper with `export --out <dir>`.
 * Node >=24.2 uses import.meta.main so exported copies need no toolbox import.
 */
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { fileURLToPath } from 'node:url'

export const SCHEMA = 'proof-media/1'
export const MANIFEST = 'proof-media.json'

/** The relevance sidecar: the clips recorded from specs that reach UI the PR changes. */
export const SIDECAR = 'proof-capture.json'
export const SIDECAR_SCHEMA = 'proof-capture/1'

const PREFIX = /^[a-z][a-z0-9-]*$/

/** The `proof-media/1` caps, as boss-verify's validator enforces them. */
export const CAPS = Object.freeze({
  captionChars: 120,
  chapters: 12,
  clipMs: 60_000,
  clips: 6,
  fileBytes: 15 * 1024 * 1024,
  stills: 12,
  totalBytes: 60 * 1024 * 1024,
})

/** The recording starts up to this long before proof.begin() starts the clock. */
const VIDEO_LEAD_MS = 800

const IMAGE_TYPES = ['.png', '.jpg', '.webp']

/** A file's suite is titled by its path, so a walk from it starts with no titles. */
function* testsOf(suite, titles = []) {
  for (const spec of suite.specs ?? []) {
    for (const test of spec.tests ?? []) yield { spec, test, titles }
  }
  for (const child of suite.suites ?? []) yield* testsOf(child, [...titles, child.title])
}

const noteOf = (annotation) => {
  try {
    const { atMs, caption, file } = JSON.parse(annotation.description)

    return Number.isSafeInteger(atMs) && atMs >= 0 && typeof caption === 'string'
      ? { atMs, caption, file }
      : null
  } catch {
    return null
  }
}

/** A path with its symlinks resolved, or as given when nothing is there. */
export const realPath = (file) => {
  try {
    return fs.realpathSync(file)
  } catch {
    return file
  }
}

/**
 * The passing tests of a Playwright JSON report, as `{title, video, spec, plain, endShot,
 * chapters: [{atMs, caption}], stills: [{atMs, caption, file}]}`, and why each other test was left
 * out. `plain` is a test with no proof steps, and `endShot` the screenshot Playwright took as it
 * ended. `spec` is the spec file's real path: the report names it relative to the config's
 * testDir, which it records as `config.rootDir`.
 */
function proofTests(report) {
  const tests = []
  const skipped = []
  const rootDir = report.config?.rootDir ?? ''

  for (const suite of report.suites ?? []) {
    for (const { spec, test } of testsOf(suite)) {
      const result = test.results?.at(-1)
      // The report repeats every result annotation on the test, so reading both doubles them.
      const notes = (result?.annotations ?? []).filter(({ type }) =>
        ['proof-chapter', 'proof-still'].includes(type),
      )

      if (!result) continue

      if (result.status !== 'passed') {
        skipped.push({ reason: `the test ${result.status}`, title: spec.title })
        continue
      }

      const video = result.attachments?.find(({ name }) => name === 'video')
      if (!video?.path) {
        skipped.push({ reason: 'no video was recorded', title: spec.title })
        continue
      }

      const parsed = (type) =>
        notes
          .filter((note) => note.type === type)
          .map(noteOf)
          .filter(Boolean)

      tests.push({
        chapters: parsed('proof-chapter').map(({ atMs, caption }) => ({
          atMs,
          caption,
        })),
        endShot: result.attachments.findLast(
          ({ name, path: file }) => name === 'screenshot' && file,
        )?.path,
        line: spec.line,
        plain: notes.length === 0,
        spec: realPath(path.resolve(rootDir, spec.file ?? '')),
        stills: parsed('proof-still').filter(({ file }) => file),
        title: spec.title,
        video: video.path,
      })
    }
  }

  return { skipped, tests }
}

const capped = (text) =>
  text.length <= CAPS.captionChars ? text : `${text.slice(0, CAPS.captionChars - 1)}…`

/** A plain test's one still: its end screenshot, at the clip's last moment. */
const endStills = (test, durationMs) =>
  test.plain && test.endShot
    ? [
        {
          atMs: Math.max(0, durationMs - 1),
          caption: capped(`End of: ${test.title}`),
          file: test.endShot,
        },
      ]
    : test.stills

/**
 * One test's clip at `durationMs`, with the stills to copy as `[from, to]` pairs (`to` relative
 * to the output directory) and what was dropped. `sizeOf(file)` is a still's size in bytes.
 */
function clipOf(test, { durationMs, id, sizeOf }) {
  const dropped = []
  const drop = (what, reason) => dropped.push(`${id} ${what}: ${reason}`)
  const captionFits = (what, caption) => {
    if (caption.length <= CAPS.captionChars) return true
    drop(what, `caption over ${CAPS.captionChars} characters`)
    return false
  }

  const chapters = []
  for (const { atMs: begunAt, caption } of [...test.chapters].sort((a, b) => a.atMs - b.atMs)) {
    const what = `chapter "${caption}"`
    const atMs = Math.max(0, begunAt - VIDEO_LEAD_MS)

    if (!captionFits(what, caption)) continue
    if (atMs >= durationMs) drop(what, 'starts after the clip ends')
    else if (atMs <= (chapters.at(-1)?.atMs ?? -1)) drop(what, 'starts with the chapter before it')
    else if (chapters.length === CAPS.chapters) drop(what, `over ${CAPS.chapters} chapters`)
    else chapters.push({ atMs, caption })
  }

  const stills = []
  const copies = []
  for (const { atMs, caption, file } of endStills(test, durationMs)) {
    const what = `still "${caption}"`
    const ext = path.extname(file).toLowerCase()

    if (!captionFits(what, caption)) continue
    if (!IMAGE_TYPES.includes(ext)) drop(what, `${ext} is not an image type`)
    else if (atMs >= durationMs) drop(what, 'taken after the clip ends')
    else if (sizeOf(file) > CAPS.fileBytes) drop(what, `over ${CAPS.fileBytes} bytes`)
    else if (stills.length === CAPS.stills) drop(what, `over ${CAPS.stills} stills`)
    else {
      const name = `${id}-still-${stills.length}${ext}`
      stills.push({ atMs, caption, file: name })
      copies.push([file, name])
    }
  }

  return {
    clip: {
      chapters,
      durationMs,
      file: `${id}.mp4`,
      id,
      // The last still is the state the clip ends on.
      ...(stills.length > 0 ? { poster: stills.at(-1).file } : {}),
      side: 'head',
      stills,
      title: capped(test.title),
    },
    copies,
    dropped,
  }
}

/** The media tools, ffmpeg and ffprobe on PATH. */
export const ffmpeg = {
  durationMs: (file) =>
    Math.round(
      Number(
        execFileSync(
          'ffprobe',
          ['-v', 'error', '-show_entries', 'format=duration', '-of', 'csv=p=0', file],
          { encoding: 'utf8' },
        ).trim(),
      ) * 1000,
    ),
  transcode: (from, to) =>
    execFileSync(
      'ffmpeg',
      [
        '-y',
        '-loglevel',
        'error',
        '-i',
        from,
        '-an',
        '-c:v',
        'libx264',
        '-pix_fmt',
        'yuv420p',
        '-movflags',
        '+faststart',
        // yuv420p needs even dimensions.
        '-vf',
        'scale=trunc(iw/2)*2:trunc(ih/2)*2',
        to,
      ],
      { stdio: ['ignore', 'ignore', 'inherit'] },
    ),
}

/** Empties `outDir` when it holds an earlier run's output, and refuses any other non-empty one. */
function prepareOutDir(outDir) {
  if (fs.existsSync(outDir) && fs.readdirSync(outDir).length > 0) {
    if (!fs.existsSync(path.join(outDir, MANIFEST))) {
      throw new Error(
        `${outDir} is not empty and holds no ${MANIFEST}; pass a new or earlier output directory`,
      )
    }
    fs.rmSync(outDir, { force: true, recursive: true, maxRetries: 3 })
  }
  fs.mkdirSync(outDir, { recursive: true })
}

/** The `[{spec, files, lines?}]` the repo’s selector wrote to `file`. */
export function readReached(file) {
  const reached = JSON.parse(fs.readFileSync(file, 'utf8'))
  if (!Array.isArray(reached)) throw new Error(`${file} is not a JSON list`)
  return reached
}

/** The reached specs by real path, each `{spec, files, lines?, rank}`. */
export function reachIndex(reached, repoRoot) {
  return new Map(
    reached.map((entry, rank) => {
      const file = path.resolve(repoRoot, entry.spec)
      if (!fs.existsSync(file)) {
        throw new Error(
          `the reached spec ${entry.spec} is not a file under ${repoRoot}; spec paths are relative to the repo root`,
        )
      }
      return [fs.realpathSync(file), { ...entry, rank }]
    }),
  )
}

/**
 * The tests, each `{spec: <real path>, line}`, that the change touches: a test runs from its line
 * to the line before the next test of its file, and is touched when a range its reached spec's
 * `lines` names falls inside.
 */
export function touchedTests(tests, reach) {
  const touched = new Set()
  const bySpec = new Map()
  for (const test of tests) {
    if (!bySpec.has(test.spec)) bySpec.set(test.spec, [])
    bySpec.get(test.spec).push(test)
  }
  for (const [spec, inSpec] of bySpec) {
    const lines = reach.get(spec)?.lines ?? []
    const starts = [...new Set(inSpec.map((test) => test.line))].toSorted((a, b) => a - b)
    const lastOf = new Map(starts.map((line, index) => [line, (starts[index + 1] ?? Infinity) - 1]))
    inSpec.forEach((test) => {
      const last = lastOf.get(test.line)
      if (lines.some(([from, to]) => from <= last && to >= test.line)) touched.add(test)
    })
  }
  return touched
}

/**
 * Writes `<outDir>/proof-media.json` and its files from the proof run in `resultsDir`, and
 * returns the manifest with what was dropped and skipped. Clip ids and file names start with
 * `prefix`.
 */
export function buildMedia({
  head,
  outDir,
  prefix = 'head',
  reached = undefined,
  repoRoot = process.cwd(),
  resultsDir,
  sourceUrl = undefined,
  tools = ffmpeg,
}) {
  if (!PREFIX.test(prefix)) {
    throw new Error(`the id prefix must be lower-case letters, digits and hyphens: ${prefix}`)
  }

  const report = JSON.parse(fs.readFileSync(path.join(resultsDir, 'report.json'), 'utf8'))
  const { skipped, tests } = proofTests(report)
  const reach = reachIndex(reached ?? [], repoRoot)
  const touched = touchedTests(tests, reach)
  const rankOf = (test) => reach.get(test.spec)?.rank ?? reach.size
  const ordered = tests.toSorted(
    (a, b) =>
      Number(!touched.has(a)) - Number(!touched.has(b)) ||
      rankOf(a) - rankOf(b) ||
      Number(a.plain) - Number(b.plain),
  )
  const sizeOf = (file) => fs.statSync(path.resolve(resultsDir, file)).size
  const clips = []
  const reachedClips = {}
  const plainIds = []
  const dropped = []
  let totalBytes = 0

  prepareOutDir(outDir)

  try {
    for (const test of ordered) {
      const reject = (reason) => dropped.push(`"${test.title}": ${reason}`)

      if (clips.length === CAPS.clips) {
        reject(`over ${CAPS.clips} clips`)
        continue
      }

      const id = `${prefix}-${clips.length}`
      const video = path.join(outDir, `${id}.mp4`)

      tools.transcode(path.resolve(resultsDir, test.video), video)

      const durationMs = tools.durationMs(video)
      const videoBytes = fs.statSync(video).size
      const {
        clip,
        copies,
        dropped: inClip,
      } = clipOf(test, {
        durationMs,
        id,
        sizeOf,
      })
      const clipBytes = videoBytes + copies.reduce((sum, [from]) => sum + sizeOf(from), 0)
      const problem =
        durationMs <= 0 || durationMs >= CAPS.clipMs
          ? `runs ${durationMs} ms, not under ${CAPS.clipMs} ms`
          : videoBytes > CAPS.fileBytes
            ? `the mp4 is over ${CAPS.fileBytes} bytes`
            : totalBytes + clipBytes > CAPS.totalBytes
              ? `takes the media over ${CAPS.totalBytes} bytes`
              : null

      if (problem) {
        fs.rmSync(video)
        reject(problem)
        continue
      }

      for (const [from, to] of copies) {
        fs.copyFileSync(path.resolve(resultsDir, from), path.join(outDir, to))
      }
      totalBytes += clipBytes
      dropped.push(...inClip)
      clips.push(clip)
      const entry = reach.get(test.spec)
      if (entry) reachedClips[id] = { files: entry.files, spec: entry.spec }
      if (test.plain) plainIds.push(id)
    }

    const manifest = {
      clips,
      head,
      schema: SCHEMA,
      ...(sourceUrl ? { source: { kind: 'ci-artifact', url: sourceUrl } } : {}),
      transcripts: [],
    }

    const writeJson = (name, value) =>
      fs.writeFileSync(path.join(outDir, name), `${JSON.stringify(value, null, 2)}\n`)

    writeJson(MANIFEST, manifest)

    const sidecar = reached
      ? { clips: reachedClips, plain: plainIds, schema: SIDECAR_SCHEMA }
      : null
    if (sidecar) writeJson(SIDECAR, sidecar)

    return { dropped, manifest, sidecar, skipped }
  } catch (error) {
    fs.rmSync(outDir, { force: true, recursive: true, maxRetries: 3 })
    throw error
  }
}

export function cliMain(argv, { tools = ffmpeg } = {}) {
  const { values, positionals } = parseArgs({
    args: argv,
    strict: true,
    allowPositionals: true,
    options: {
      head: { type: 'string' },
      out: { type: 'string' },
      prefix: { type: 'string' },
      reached: { type: 'string' },
      results: { type: 'string' },
      'source-url': { type: 'string' },
    },
  })

  if (positionals.length) {
    if (
      positionals.length !== 1 ||
      positionals[0] !== 'export' ||
      !values.out ||
      Object.keys(values).some((key) => key !== 'out')
    )
      throw new Error('usage: verify-proof-capture export --out <dir>')
    fs.mkdirSync(values.out, { recursive: true })
    const names = ['verify-proof-capture.mjs', 'verify-proof-steps.mjs']
    for (const name of names) {
      const source = fileURLToPath(new URL(name, import.meta.url))
      const destination = path.resolve(values.out, name)
      if (realPath(source) !== realPath(destination)) fs.copyFileSync(source, destination)
    }
    return `${JSON.stringify({ exported: names })}\n`
  }

  if (!values.results || !values.out || !values.head) {
    throw new Error(
      'usage: verify-proof-capture --results <dir> --out <dir> --head <sha> [--prefix <id prefix>] [--reached <file>] [--source-url <url>]',
    )
  }
  if (!/^[0-9a-f]{40}$/.test(values.head)) {
    throw new Error(`--head must be a 40-character commit sha: ${values.head}`)
  }
  if (values['source-url'] && !/^https:\/\//.test(values['source-url'])) {
    throw new Error(`--source-url must be an https URL: ${values['source-url']}`)
  }

  const reached = values.reached ? readReached(values.reached) : undefined

  const { dropped, manifest, sidecar, skipped } = buildMedia({
    head: values.head,
    outDir: values.out,
    prefix: values.prefix,
    reached,
    resultsDir: values.results,
    sourceUrl: values['source-url'],
    tools,
  })
  const summary = {
    clips: manifest.clips.length,
    dropped,
    ...(sidecar ? { reached: Object.keys(sidecar.clips) } : {}),
    skipped,
  }

  return `${JSON.stringify(summary, null, 2)}\n`
}

if (import.meta.main) {
  try {
    process.stdout.write(cliMain(process.argv.slice(2)))
  } catch (error) {
    process.stderr.write(`verify-proof-capture: ${error.message}\n`)
    process.exitCode = 1
  }
}
