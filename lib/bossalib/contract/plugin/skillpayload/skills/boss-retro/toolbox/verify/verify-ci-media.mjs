import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'

import {
  assertStageBlockReadable,
  DEFAULT_CONFIG,
  mergeConfig,
  validateConfig,
  withTrackerDefaults,
} from '../skill-config.mjs'
import { MEDIA_NOT_CONFIGURED_EXIT, MEDIA_ROLE } from './verify-media-hook.mjs'
import { isMainModule } from '../main-module.mjs'
import { PROOF_MEDIA_SCHEMA, validateProofMedia } from './verify-proof-media.mjs'
import { isRecord } from './verify-proof-spec.mjs'

export { MEDIA_ROLE }

export const CAPTURE_MANIFEST = 'proof-media.json'
export const CAPTURE_SIDECAR = 'proof-capture.json'
export const CAPTURE_SIDECAR_SCHEMA = 'proof-capture/1'

/** @typedef {{suite: string, workflow: string, job: string, artifact: string}} MediaSource */
/** @param {object} config @returns {MediaSource[]} */
export function mediaSources(config) {
  assertStageBlockReadable(config, 'proofDefaults')
  const sources = config?.proofDefaults?.media?.sources ?? []
  const suites = new Set()
  for (const source of sources) {
    if (suites.has(source.suite)) throw new Error('duplicate proof-media suite: ' + source.suite)
    suites.add(source.suite)
  }
  return sources
}

function validatedConfig(config, source) {
  if (!isRecord(config)) throw new Error(`${source} must be an object`)
  const merged = withTrackerDefaults(mergeConfig(DEFAULT_CONFIG, config))
  validateConfig(merged, source, { deferStageBlocks: true })
  return merged
}

/** Read only the base revision's config, never the PR checkout's file. */
export function readBaseConfig({ baseSha, cwd = process.cwd(), run = spawnSync }) {
  if (!SHA.test(baseSha ?? '')) throw new Error('baseSha is not a 40-hex sha')
  const result = run('git', ['show', baseSha + ':.boss-skills.json'], {
    cwd,
    encoding: 'utf8',
    env: { ...process.env, LC_ALL: 'C' },
    timeout: 10_000,
    maxBuffer: 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  if (result.error) throw new Error('could not read base config')
  if (result.status !== 0) {
    if (
      /path ['"]\.boss-skills\.json['"] (?:does not exist in|exists on disk, but not in)/.test(
        String(result.stderr),
      )
    )
      return {}
    throw new Error('could not read base config')
  }
  let config
  try {
    config = JSON.parse(String(result.stdout))
  } catch {
    throw new Error('base config is not readable JSON')
  }
  return validatedConfig(config, 'base .boss-skills.json')
}

const REPO = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/
const SHA = /^[0-9a-f]{40}$/

export const emptyManifest = (head) => ({
  clips: [],
  head,
  schema: PROOF_MEDIA_SCHEMA,
  transcripts: [],
})

/** Run gh with `args`: `{status, stdout}`. DEBUG and GH_DEBUG would make gh log every request. */
export function runGh(args) {
  const result = spawnSync('gh', args, {
    encoding: 'utf8',
    env: { ...process.env, DEBUG: '', GH_DEBUG: '' },
    maxBuffer: 16 * 1024 * 1024,
    stdio: ['ignore', 'pipe', 'pipe'],
  })
  return { status: result.status ?? 1, stdout: result.stdout ?? '' }
}

function ghJson(gh, endpoint) {
  const { status, stdout } = gh(['api', endpoint])
  if (status !== 0) throw new Error(`gh api ${endpoint} failed (exit ${status})`)
  try {
    return JSON.parse(stdout)
  } catch {
    throw new Error(`gh api ${endpoint} did not return JSON`)
  }
}

export function parseEnvelope(text) {
  let envelope
  try {
    envelope = JSON.parse(text)
  } catch {
    throw new Error('stdin is not a JSON envelope')
  }
  if (envelope?.role !== MEDIA_ROLE) throw new Error(`envelope role is not ${MEDIA_ROLE}`)
  if (typeof envelope.repo !== 'string' || !REPO.test(envelope.repo))
    throw new Error('envelope repo is not owner/repo')
  if (typeof envelope.baseSha !== 'string' || !SHA.test(envelope.baseSha))
    throw new Error('envelope baseSha is not a 40-hex sha')
  if (!Number.isSafeInteger(envelope.pr) || envelope.pr < 1)
    throw new Error('envelope pr is not a positive integer')
  if (typeof envelope.headSha !== 'string' || !SHA.test(envelope.headSha))
    throw new Error('envelope headSha is not a 40-hex sha')
  if (typeof envelope.evidenceDir !== 'string' || !path.isAbsolute(envelope.evidenceDir))
    throw new Error('envelope evidenceDir is not an absolute path')
  return envelope
}

function findRun({ api, head, job: jobName, pr, repo, workflow }) {
  const { workflow_runs: runs = [] } = api(
    `repos/${repo}/actions/workflows/${workflow}/runs?head_sha=${head}&event=pull_request&per_page=100`,
  )
  // A fork PR's run lists no pull_requests, so only a non-empty list can rule a run out.
  const forPr = (run) =>
    !run.pull_requests?.length || run.pull_requests.some((entry) => entry.number === pr)
  const newestFirst = runs
    .filter((run) => run.head_sha === head && forPr(run))
    .sort((a, b) => b.id - a.id)
  let skipped = false
  for (const run of newestFirst) {
    // The jobs endpoint lists the run's latest attempt.
    const { jobs = [] } = api(`repos/${repo}/actions/runs/${run.id}/jobs?per_page=100`)
    const job = jobs.find((entry) => entry.name === jobName)
    if (job?.status !== 'completed') continue
    if (job.conclusion === 'skipped') {
      skipped = true
      continue
    }
    if (job.conclusion !== 'success') return { none: 'capture-failed' }
    return { job, run: run.id }
  }
  return { none: skipped ? 'capture-skipped' : 'no-run' }
}

/**
 * What each source says for `head`, in source order: `{source, run}` for a passing job that
 * uploaded the source's artifact, else `{source, none: <why>}`, adding `no-artifact` to findRun's
 * reasons.
 */
export function findCaptures({ gh, head, pr, repo, sources }) {
  const fetched = new Map()
  const api = (endpoint) => {
    if (!fetched.has(endpoint)) fetched.set(endpoint, ghJson(gh, endpoint))
    return fetched.get(endpoint)
  }
  return sources.map((source) => {
    const found = findRun({
      api,
      head,
      job: source.job,
      pr,
      repo,
      workflow: source.workflow,
    })
    if (found.none) return { none: found.none, source }
    const name = source.artifact.replaceAll('{head}', head)
    const { artifacts = [] } = api(
      `repos/${repo}/actions/runs/${found.run}/artifacts?name=${encodeURIComponent(name)}&per_page=100`,
    )
    // The artifacts endpoint spans every attempt, so an artifact older than this attempt's job
    // came from an earlier attempt, which may have failed.
    const uploaded = artifacts.some(
      (entry) =>
        entry.name === name &&
        !entry.expired &&
        Date.parse(entry.created_at) >= Date.parse(found.job.started_at),
    )
    return uploaded ? { run: found.run, source } : { none: 'no-artifact', source }
  })
}

/** Download `name` from `run` into `dir`; gh refuses paths that escape it. */
export function download({ dir, gh, name, repo, run }) {
  const { status } = gh([
    'run',
    'download',
    String(run),
    '--repo',
    repo,
    '--name',
    name,
    '--dir',
    dir,
  ])
  if (status !== 0) throw new Error(`gh run download ${run} failed (exit ${status})`)
}

/**
 * Resolve a merged path `<suite>/<file>` to the real file in that suite's artifact directory:
 * null unless it is a regular file whose real path stays inside that directory.
 */
export function locator(dirs) {
  const roots = new Map(Object.entries(dirs).map(([suite, dir]) => [suite, fs.realpathSync(dir)]))
  return (file) => {
    const cut = file.indexOf('/')
    const root = cut > 0 ? roots.get(file.slice(0, cut)) : undefined
    if (!root) return null
    try {
      const real = fs.realpathSync(path.join(root, file.slice(cut + 1)))
      const relative = path.relative(root, real)
      if (!relative || relative.startsWith('..') || path.isAbsolute(relative)) return null
      return fs.statSync(real).isFile() ? real : null
    } catch {
      return null
    }
  }
}

function underSuite(entry, suite) {
  if (!isRecord(entry)) return entry
  const moved = { ...entry }
  for (const key of ['file', 'poster'])
    if (typeof moved[key] === 'string') moved[key] = `${suite}/${moved[key]}`
  for (const key of ['stills', 'pages'])
    if (Array.isArray(moved[key])) moved[key] = moved[key].map((item) => underSuite(item, suite))
  return moved
}

const texts = (list) => Array.isArray(list) && list.every((text) => typeof text === 'string')

const provesFiles = (entry) =>
  isRecord(entry) && typeof entry.spec === 'string' && texts(entry.files) && entry.files.length > 0

/**
 * The artifact's sidecar: `{clips, plain}`, the clips that prove a changed file as a map of id to
 * `{spec, files}` and the plain clips' ids. Null when it is missing, malformed or of another
 * schema.
 */
export function readSidecar(dir) {
  try {
    const sidecar = JSON.parse(fs.readFileSync(path.join(dir, CAPTURE_SIDECAR), 'utf8'))
    const clips = Object.entries(sidecar?.clips ?? [])
    if (
      sidecar?.schema === CAPTURE_SIDECAR_SCHEMA &&
      isRecord(sidecar.clips) &&
      clips.every(([, entry]) => provesFiles(entry)) &&
      texts(sidecar.plain)
    )
      return { clips: new Map(clips), plain: new Set(sidecar.plain) }
  } catch {}
  return null
}

function shownClips(clips, { dir, log, suite }) {
  if (clips.length === 0) return []
  const sidecar = readSidecar(dir)
  if (!sidecar) {
    log(`ci-media: ${suite}: ${CAPTURE_SIDECAR} is missing or malformed; no clip is shown\n`)
    return []
  }
  const shown = clips.flatMap((clip) => {
    const proves = sidecar.clips.get(clip?.id)
    return proves
      ? [
          {
            clip: { ...clip, files: proves.files },
            tier: Number(sidecar.plain.has(clip.id)),
          },
        ]
      : []
  })
  const left = clips.length - shown.length
  if (left > 0)
    log(
      `ci-media-fetch: ${suite}: ${left} of ${clips.length} clips prove no changed file; not shown\n`,
    )
  return shown
}

function readArtifact({ dir, head, log, suite }) {
  let raw
  try {
    raw = JSON.parse(fs.readFileSync(path.join(dir, CAPTURE_MANIFEST), 'utf8'))
  } catch {
    throw new Error(`the ${suite} artifact holds no readable ${CAPTURE_MANIFEST}`)
  }
  const { dropped, media } = validateProofMedia(raw, { head })
  if (!media)
    throw new Error(
      `the ${suite} artifact's ${CAPTURE_MANIFEST} is unusable: ${dropped[0]?.reason}`,
    )
  const list = (key) =>
    Array.isArray(raw[key]) ? raw[key].map((entry) => underSuite(entry, suite)) : []
  return {
    clips: shownClips(list('clips'), { dir, log, suite }),
    documents: list('documents'),
    source: raw.source,
    transcripts: Array.isArray(raw.transcripts) ? raw.transcripts : [],
  }
}

const filesOfClip = (clip) => [
  clip.file,
  ...(clip.poster ? [clip.poster] : []),
  ...clip.stills.map((still) => still.file),
]
const filesOfDocument = (document) => [document.file, ...document.pages.map((page) => page.file)]

/**
 * Merge artifact directories `dirs` (suite -> directory) into one manifest for `head`, and copy
 * the files that pass validation into `evidenceDir` at the manifest's paths. Returns the merged
 * manifest with every item in priority order, kept or not, so the core names what it drops.
 */
export function mergeArtifacts({ dirs, evidenceDir, head, sources, log = (text) => {} }) {
  const known = new Set(sources.map(({ suite }) => suite))
  const unknown = Object.keys(dirs).find((suite) => !known.has(suite))
  if (unknown) throw new Error(`${unknown} is not a proof-media suite`)
  const artifacts = sources
    .filter(({ suite }) => Object.hasOwn(dirs, suite) && dirs[suite])
    .map(({ suite }) => readArtifact({ dir: dirs[suite], head, log, suite }))
  if (artifacts.length === 0) return emptyManifest(head)
  const documents = artifacts.flatMap((artifact) => artifact.documents)
  const items = (artifact) => artifact.clips.length + artifact.documents.length
  const source = artifacts
    .filter((artifact) => artifact.source !== undefined)
    .reduce(
      (best, artifact) => (!best || items(artifact) > items(best) ? artifact : best),
      undefined,
    )?.source
  const manifest = {
    clips: artifacts
      .flatMap((artifact) => artifact.clips)
      .toSorted((a, b) => a.tier - b.tier)
      .map(({ clip }) => clip),
    ...(documents.length ? { documents } : {}),
    head,
    schema: PROOF_MEDIA_SCHEMA,
    ...(source === undefined ? {} : { source }),
    transcripts: artifacts.flatMap((artifact) => artifact.transcripts),
  }

  const locate = locator(dirs)
  const sizeOf = (file) => {
    const real = locate(file)
    return real ? fs.statSync(real).size : null
  }
  const { media } = validateProofMedia(manifest, { head, sizeOf })
  fs.mkdirSync(evidenceDir, { recursive: true })
  for (const file of new Set([
    ...media.clips.flatMap(filesOfClip),
    ...(media.documents ?? []).flatMap(filesOfDocument),
  ])) {
    const to = path.join(evidenceDir, file)
    fs.mkdirSync(path.dirname(to), { recursive: true })
    fs.copyFileSync(locate(file), to)
  }
  return manifest
}

/** The manifest for `envelope`, with its files staged in `evidenceDir`. */
export function fetchProofMedia(envelope, { sources, gh = runGh, log = (text) => {} }) {
  const { evidenceDir, headSha: head, pr, repo } = envelope
  const captures = findCaptures({ gh, head, pr, repo, sources })
  for (const { none, source } of captures)
    if (none) log(`ci-media: ${source.suite}: nothing to show (${none})\n`)
  const found = captures.filter(({ run }) => run)
  if (found.length === 0) return emptyManifest(head)
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'ci-media-'))
  try {
    const dirs = {}
    for (const { run, source } of found) {
      const dir = path.join(tmp, source.suite)
      fs.mkdirSync(dir)
      download({ dir, gh, name: source.artifact.replaceAll('{head}', head), repo, run })
      dirs[source.suite] = dir
    }
    return mergeArtifacts({ dirs, evidenceDir, head, sources, log })
  } finally {
    fs.rmSync(tmp, { force: true, recursive: true, maxRetries: 3 })
  }
}

/** The hook: read the envelope, print the manifest, return the exit code. */
export function mediaHookMain({
  gh = runGh,
  run = spawnSync,
  log = (text) => process.stderr.write(text),
  stdinText = fs.readFileSync(0, 'utf8'),
  write = (text) => process.stdout.write(text),
} = {}) {
  try {
    const envelope = parseEnvelope(stdinText)
    const sources = mediaSources(
      readBaseConfig({ baseSha: envelope.baseSha, cwd: process.cwd(), run }),
    )
    if (!sources.length) return MEDIA_NOT_CONFIGURED_EXIT
    const manifest = fetchProofMedia(envelope, { sources, gh, log })
    write(`${JSON.stringify(manifest)}\n`)
    return 0
  } catch (error) {
    log(`ci-media: ${error.message}\n`)
    return 1
  }
}

/**
 * `--merge-local <suite>=<dir>... --head <sha> --out <dir>`: merge local artifact directories as
 * the hook would, stage the kept files in `--out`, and write the manifest to stdout and to
 * `<out>/proof-media.json`.
 */
export function mergeLocalMain(
  argv,
  { log = (text) => process.stderr.write(text), write = (text) => process.stdout.write(text) } = {},
) {
  try {
    const { positionals, values } = parseArgs({
      args: argv,
      allowPositionals: true,
      strict: true,
      options: {
        config: { type: 'string' },
        head: { type: 'string' },
        'merge-local': { type: 'boolean' },
        out: { type: 'string' },
      },
    })
    if (!values['merge-local']) throw new Error('expected --merge-local')
    if (!SHA.test(values.head ?? '')) throw new Error('--head is not a 40-hex sha')
    if (!values.out) throw new Error('--out is required')
    if (!values.config) throw new Error('--config is required')
    const config = validatedConfig(
      JSON.parse(fs.readFileSync(values.config, 'utf8')),
      values.config,
    )
    const sources = mediaSources(config)
    const dirs = {}
    for (const pair of positionals) {
      const [suite, dir] = pair.split(/=(.*)/s)
      if (!dir) throw new Error(`${pair} is not <suite>=<dir>`)
      dirs[suite] = path.resolve(dir)
    }
    const out = path.resolve(values.out)
    const manifest = mergeArtifacts({
      dirs,
      evidenceDir: out,
      sources,
      head: values.head,
      log,
    })
    const text = `${JSON.stringify(manifest)}\n`
    fs.mkdirSync(out, { recursive: true })
    fs.writeFileSync(path.join(out, CAPTURE_MANIFEST), text)
    write(text)
    return 0
  } catch (error) {
    log(`ci-media: ${error.message}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url, { warn: () => {} })) {
  const argv = process.argv.slice(2)
  process.exitCode = argv.length ? mergeLocalMain(argv) : mediaHookMain()
}
