import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import { parseArgs } from 'node:util'

import {
  CAPTURE_MANIFEST,
  download,
  findCaptures,
  locator,
  readSidecar,
  runGh,
  mediaSources,
  readBaseConfig,
} from './verify-ci-media.mjs'
import { detectAreas } from './verify-areas.mjs'
import { readChangedFiles } from './verify-range.mjs'
import { APPLIES_NOT_APPLICABLE_EXIT } from './verify-applies.mjs'
import { isMainModule } from '../main-module.mjs'
import { validateProofMedia } from './verify-proof-media.mjs'

export const EXTENSION = 'boss-verify-ui-recording'

/** The verify gate's applies exit code for "this extension does not apply". */
export const NOT_APPLICABLE_EXIT = APPLIES_NOT_APPLICABLE_EXIT

const SHA = /^[0-9a-f]{40}$/
const REPO = /^[A-Za-z0-9._-]+\/[A-Za-z0-9._-]+$/
const MAX_LISTED = 5

const result = (body) => ({
  extension: EXTENSION,
  findings: [],
  ok: true,
  role: 'verify',
  ...body,
})

const abstain = (reason, detail) => result({ detail, evidence: [], reason, verdict: 'abstain' })

const listed = (files) =>
  files.length > MAX_LISTED
    ? `${files.slice(0, MAX_LISTED).join(', ')} and ${files.length - MAX_LISTED} more`
    : files.join(', ')

export const uiFilesOf = (changedPaths) =>
  detectAreas(changedPaths).find(({ area }) => area === 'ui')?.paths ?? []

function envelopeUiFiles(envelope) {
  if (envelope.uiFiles === undefined) return uiFilesOf(envelope.changedPaths)
  if (
    !Array.isArray(envelope.uiFiles) ||
    !envelope.uiFiles.every(
      (file) => typeof file === 'string' && file.length > 0 && envelope.changedPaths.includes(file),
    )
  )
    throw new Error('envelope uiFiles is not a list of changed paths')
  return [...new Set(envelope.uiFiles)]
}

/**
 * The verify envelope's facts this extension reads: `{pr, repo, baseSha, headSha, uiFiles}`. The repo comes
 * from `pr.url` unless `repo` is given. Throws on an envelope that lacks them.
 * @param {string} text
 * @param {{repo?: string}} [options]
 */
export function parseEnvelope(text, { repo } = {}) {
  let envelope
  try {
    envelope = JSON.parse(text)
  } catch {
    throw new Error('stdin is not a JSON envelope')
  }
  const pr = envelope?.pr?.number
  if (!Number.isInteger(pr) || pr < 1)
    throw new Error('envelope pr.number is not a positive integer')
  if (typeof envelope.baseSha !== 'string' || !SHA.test(envelope.baseSha))
    throw new Error('envelope baseSha is not a 40-hex sha')
  if (typeof envelope.headSha !== 'string' || !SHA.test(envelope.headSha))
    throw new Error('envelope headSha is not a 40-hex sha')
  if (
    !Array.isArray(envelope.changedPaths) ||
    !envelope.changedPaths.every((file) => typeof file === 'string')
  )
    throw new Error('envelope changedPaths is not a list of paths')
  const fromUrl = /^https:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/\d+$/.exec(
    envelope.pr.url ?? '',
  )?.[1]
  const slug = repo ?? fromUrl
  if (!slug || !REPO.test(slug)) throw new Error('envelope pr.url names no owner/repo')
  return {
    baseSha: envelope.baseSha,
    headSha: envelope.headSha,
    pr,
    repo: slug,
    uiFiles: envelopeUiFiles(envelope),
  }
}

/**
 * The clips one downloaded artifact recorded for `head` that its sidecar maps to changed files:
 * `[{suite, id, file, spec, files, instrumented}]`. A clip whose video is missing from the
 * artifact is not recorded, and an artifact without a usable manifest or sidecar records none.
 */
export function recordedClips({ dir, head, suite }) {
  let raw
  try {
    raw = JSON.parse(fs.readFileSync(path.join(dir, CAPTURE_MANIFEST), 'utf8'))
  } catch {
    return []
  }
  const locate = locator({ [suite]: dir })
  const { media } = validateProofMedia(raw, {
    head,
    sizeOf: (file) => {
      const real = locate(`${suite}/${file}`)
      return real ? fs.statSync(real).size : null
    },
  })
  const sidecar = readSidecar(dir)
  if (!media || !sidecar) return []
  return media.clips.flatMap((clip) => {
    const proves = sidecar.clips.get(clip.id)
    return proves
      ? [
          {
            file: clip.file,
            files: proves.files,
            id: clip.id,
            instrumented: !sidecar.plain.has(clip.id),
            spec: proves.spec,
            suite,
          },
        ]
      : []
  })
}

/**
 * The verdict, from the changed UI files and what each clip suite's capture says for the head,
 * `[{suite, none?, url?, clips}]` (`none` the fetcher's reason a suite has nothing). Pure.
 */
export function judgeRecordings({ captures, uiFiles }) {
  if (uiFiles.length === 0) return abstain('no-ui-files', 'the PR changes no UI file')
  const failed = captures.filter(({ none }) => none === 'capture-failed')
  if (failed.length > 0)
    return abstain(
      'capture-failed',
      `the ${failed.map(({ suite }) => suite).join(' and ')} proof-capture job failed on the head`,
    )
  const clips = captures.flatMap((capture) =>
    capture.clips
      .filter(({ instrumented }) => instrumented)
      .map((clip) => ({ ...clip, url: capture.url })),
  )
  const unproven = uiFiles.filter((file) => !clips.some(({ files }) => files.includes(file)))
  if (unproven.length > 0)
    return abstain('ui-not-recorded', `no clip with proof steps proves ${listed(unproven)}`)
  const used = clips.filter(({ files }) => files.some((file) => uiFiles.includes(file)))
  return result({
    evidence: used.map(({ file, files, id, spec, suite, url }) => ({
      files: files.filter((changed) => uiFiles.includes(changed)),
      kind: 'recording',
      ref: `${suite}/${file}`,
      spec,
      title: `${id} from ${spec}`,
      url,
    })),
    summary: `every changed UI file (${uiFiles.length}) is proven by a clip with proof steps recorded on the head (${used.length} in all)`,
    verdict: 'pass',
  })
}

/** Each clip suite's capture for the head, its artifact downloaded and read. */
export function readCaptures({ gh, head, pr, repo, sources }) {
  const captures = findCaptures({ gh, head, pr, repo, sources })
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'ui-recording-'))
  try {
    return captures.map(({ none, run, source }) => {
      if (none) return { clips: [], none, suite: source.suite }
      const dir = path.join(tmp, source.suite)
      fs.mkdirSync(dir)
      download({ dir, gh, name: source.artifact.replaceAll('{head}', head), repo, run })
      return {
        clips: recordedClips({ dir, head, suite: source.suite }),
        suite: source.suite,
        url: `https://github.com/${repo}/actions/runs/${run}`,
      }
    })
  } finally {
    fs.rmSync(tmp, { force: true, recursive: true, maxRetries: 3 })
  }
}

/** The extension: one verify result for the envelope, whatever goes wrong.
 * @param {string} text
 * @param {{gh?: typeof runGh, run?: typeof spawnSync, repo?: string}} [options]
 */
export function verifyEnvelope(text, { gh = runGh, run = spawnSync, repo } = {}) {
  let facts
  try {
    facts = parseEnvelope(text, { repo })
  } catch (error) {
    return abstain('envelope-unusable', error.message)
  }
  try {
    if (facts.uiFiles.length === 0) return judgeRecordings({ captures: [], uiFiles: [] })
    const sources = mediaSources(readBaseConfig({ baseSha: facts.baseSha, run }))
    if (!sources.length)
      return abstain('not-configured', 'no proof-media sources configured at the base')
    return judgeRecordings({
      captures: readCaptures({
        gh,
        head: facts.headSha,
        pr: facts.pr,
        repo: facts.repo,
        sources,
      }),
      uiFiles: facts.uiFiles,
    })
  } catch (error) {
    return abstain('evidence-unreadable', error.message)
  }
}

export function verifyMain({
  gh = runGh,
  run = spawnSync,
  stdinText = fs.readFileSync(0, 'utf8'),
  write = (text) => process.stdout.write(text),
} = {}) {
  write(`${JSON.stringify(verifyEnvelope(stdinText, { gh, run }))}\n`)
  return 0
}

/**
 * The gate's applicability check: exit NOT_APPLICABLE_EXIT when the envelope's changed paths hold
 * no UI file or no configured sources; exit 0 otherwise, unreadable inputs included. It prints no note: the core shows
 * a not-applicable note on the proof page, and "no UI file changed" tells a reviewer nothing.
 */
export function appliesMain({ stdinText = fs.readFileSync(0, 'utf8'), run = spawnSync } = {}) {
  try {
    const envelope = JSON.parse(stdinText)
    if (!Array.isArray(envelope?.changedPaths)) return 0
    if (envelopeUiFiles(envelope).length === 0) return NOT_APPLICABLE_EXIT
    const sources = mediaSources(readBaseConfig({ baseSha: envelope.baseSha, run }))
    return sources.length ? 0 : NOT_APPLICABLE_EXIT
  } catch {
    return 0
  }
}

/** `--pr <number> [--repo <owner/repo>]`: build the envelope from gh and print the result. */
export function cliMain(argv, { gh = runGh, run = spawnSync } = {}) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    options: { pr: { type: 'string' }, repo: { type: 'string' } },
  })
  const pr = Number(values.pr)
  if (!Number.isInteger(pr) || pr < 1)
    throw new Error('usage: verify-ui-recording --pr <number> [--repo <owner/repo>]')
  const view = gh([
    'pr',
    'view',
    String(pr),
    ...(values.repo ? ['--repo', values.repo] : []),
    '--json',
    'baseRefOid,headRefOid,number,url',
  ])
  if (view.status !== 0) throw new Error(`gh pr view ${pr} failed`)
  const { baseRefOid, headRefOid, number, url } = JSON.parse(view.stdout)
  const repo = values.repo ?? /^https:\/\/github\.com\/([^/]+\/[^/]+)\/pull\/\d+$/.exec(url)?.[1]
  const files = readChangedFiles(
    (_command, args) => {
      const response = gh(args)
      return {
        ok: response.status === 0,
        payload: response.status === 0 ? JSON.parse(response.stdout) : null,
      }
    },
    pr,
    repo,
  )
  if (files === null) throw new Error(`gh pr files ${pr} unreadable or truncated`)
  const envelope = {
    changedPaths: files.flatMap((file) => [file.filename, file.previous_filename].filter(Boolean)),
    uiFiles: uiFilesOf(
      files.map((file) => ({
        path: file.filename,
        previousPath: file.previous_filename,
        status: file.status,
        changes: file.changes,
        patch: file.patch,
      })),
    ),
    baseSha: baseRefOid,
    headSha: headRefOid,
    pr: { number, url },
  }
  return `${JSON.stringify(verifyEnvelope(JSON.stringify(envelope), { gh, run, repo: values.repo }))}\n`
}

if (isMainModule(import.meta.url, { warn: () => {} })) {
  try {
    const argv = process.argv.slice(2)
    if (argv.length) process.stdout.write(cliMain(argv))
    else process.exitCode = verifyMain()
  } catch (error) {
    process.stderr.write(`verify-ui-recording: ${error.message}\n`)
    process.exitCode = 1
  }
}
