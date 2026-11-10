// `verify-gate.mjs proof-bundle`: everything the proof-author agent reads for one parked head,
// gathered with zero tokens. It writes `bundle.json` (the PR, per-file line counts, the judge's
// areas, confidence and park reasons, notes, CI checks, commits, whether the repo can auto-merge,
// and a summary of the media), runs the base tree's `proof-media` extension into `<out>/media`
// and writes the validated manifest as `<out>/media.json`. `post --proof-media <out>` reads it.
import { mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import path from 'node:path'
import { classifyEntry, normalizeRollup } from '../pr-check-state.mjs'
import {
  MEDIA_DIR,
  MEDIA_MANIFEST,
  collectProofMedia,
  mediaSummary,
  writeMediaManifest,
} from './verify-media-hook.mjs'
import { buildProofRecord } from './verify-proof-build.mjs'
import { gitChangedFiles } from './verify-proof-snippet.mjs'

export const BUNDLE_SCHEMA = 'proof-bundle/1'
export const BUNDLE_FILE = 'bundle.json'
export const MAX_BUNDLE_COMMITS = 50
export const MAX_BUNDLE_CHECKS = 50
// How long the skill waits for the proof-author agent before posting the deterministic page.
export const PROOF_AUTHOR_HANG_GUARD_MS = 10 * 60 * 1000

/** @param {unknown} value @returns {value is Record<string, any>} */
const isObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
/** @param {unknown} list */
const objects = (list) => (Array.isArray(list) ? list.filter(isObject) : [])

/**
 * The bundle for a parked head. `areas` are the proof record's (their `id` is the name a spec
 * cites), each with the judge area's describer facts. Pure: every read happens in the caller.
 * @param {{judged: Record<string, any>, record: {proof: any, areas: any[]}, pr: Record<string, any>,
 *   repo: string, files: unknown[] | null, commits: {at: string, subject: string}[],
 *   canAutoMerge: boolean | null, media: ReturnType<typeof mediaSummary>}} input
 */
export function proofBundle({ judged, record, pr, repo, files, commits, canAutoMerge, media }) {
  const judgeAreas = new Map(objects(judged.facts?.areas).map((area) => [area.area, area]))
  const { proof } = record
  return {
    schema: BUNDLE_SCHEMA,
    repo,
    pr: {
      number: proof.pr,
      title: String(pr.title ?? ''),
      body: String(pr.body ?? ''),
      url: String(pr.url ?? ''),
      baseRef: String(pr.baseRefName ?? ''),
      base: proof.base,
      head: proof.head,
    },
    verdict: judged.verdict,
    reasons: Array.isArray(judged.reasons) ? judged.reasons : [],
    ...(typeof judged.label === 'string' ? { label: judged.label } : {}),
    confidence: proof.confidence,
    files: files ?? [],
    areas: record.areas.map((entry) => {
      const area = judgeAreas.get(entry.id) ?? {}
      const recorded = proof.areas.find((a) => a.name === entry.name) ?? {}
      return {
        id: entry.name,
        label: recorded.label ?? entry.name,
        ask: entry.ask,
        paths: entry.paths,
        ...(area.defaultAsk ? { defaultAsk: area.defaultAsk } : {}),
        items: recorded.items ?? [],
        ...(recorded.change ? { change: recorded.change } : {}),
        ...(recorded.changeReasons ? { changeReasons: recorded.changeReasons } : {}),
        undisclosed: recorded.unmentioned ?? [],
      }
    }),
    blockers: proof.review.blockers,
    notes: proof.review.notes,
    ci: normalizeRollup(pr.statusCheckRollup)
      .slice(0, MAX_BUNDLE_CHECKS)
      .map((check) => ({ name: check.name, state: classifyEntry(check) })),
    commits,
    repoSettings: { canAutoMerge },
    media,
  }
}

/** @param {string} file */
function readJson(file) {
  try {
    return JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

/**
 * The `proof-bundle` verb: `--pr <n> --head <sha> --judge <file> --out <dir> [--repo o/r]`. Writes
 * `<out>/bundle.json`, `<out>/media/` and, when the extension returned valid media,
 * `<out>/media.json`, and returns their paths: `{written: true, bundle, media, proofMedia, spec, hangGuardMs}`:
 * `proofMedia` is the directory `post --proof-media` takes and `spec` where the agent writes its
 * `proof-spec/1`. `{written: false, reason}` when the
 * judge output is unreadable or for another head, or the PR moved. Never writes to the forge.
 */
export function proofBundleVerb(options, deps, actions) {
  const { UsageError, writeAwareReader, commandReader, repoFor, readPr } = actions
  const headSha = String(options.head ?? '').toLowerCase()
  if (!/^[0-9a-f]{40}$/.test(headSha) || options.judge === undefined || options.out === undefined)
    throw new UsageError(
      'verify-gate: proof-bundle needs --pr <n> --head <40-hex sha> --judge <file> --out <dir>',
    )
  const judged = readJson(options.judge)
  if (!isObject(judged)) return { written: false, reason: 'judge-unreadable' }
  if (String(judged.headSha ?? '').toLowerCase() !== headSha)
    return { written: false, reason: 'judge-other-head' }
  const read = writeAwareReader(deps.read ?? commandReader(deps))
  const repo = repoFor(read, options.repo)
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return { written: false, reason: 'pr-unreadable' }
  const pr = view.payload ?? {}
  if (String(pr.headRefOid ?? '').toLowerCase() !== headSha)
    return { written: false, reason: 'head-moved' }
  const record = buildProofRecord({ judged, repo, pr: Number(options.pr) })
  if (!record.ok) return { written: false, reason: record.reason }
  const { base, head } = record.proof
  /** @param {string[]} args */
  const git = (args) => {
    const out = read('git', args, { raw: true })
    return out?.ok ? String(out.output ?? '') : null
  }
  const log = git([
    'log',
    `--max-count=${MAX_BUNDLE_COMMITS}`,
    '--format=%cI%x09%s',
    `${base}..${head}`,
  ])
  const commits = (log ?? '')
    .split('\n')
    .filter((line) => line.includes('\t'))
    .map((line) => {
      const [at, ...subject] = line.split('\t')
      return { at, subject: subject.join('\t') }
    })
  const policy = actions.readBasePolicy?.(read, repo, base)
  // A merge rule on the base is what lets verify merge a head with no person; unknown is null.
  const canAutoMerge =
    policy?.state === 'ok'
      ? Boolean(policy.block?.merge)
      : policy?.state === 'absent'
        ? false
        : null
  const out = path.resolve(options.out)
  const mediaDir = path.join(out, MEDIA_DIR)
  mkdirSync(mediaDir, { recursive: true })
  const outcome = (actions.collectMedia ?? collectProofMedia)({
    repo,
    pr: Number(options.pr),
    headSha,
    baseSha: base,
    dir: mediaDir,
    cwd: actions.cwd,
    read,
    env: actions.env,
    trustedTree: actions.trustedTree,
    ...(actions.discover ? { discover: actions.discover } : {}),
  })
  const manifest = writeMediaManifest(outcome, out)
  const bundle = proofBundle({
    judged,
    record: /** @type {{proof: any, areas: any[]}} */ (record),
    pr,
    repo,
    files: gitChangedFiles({ base, head, run: (args) => git(['--literal-pathspecs', ...args]) }),
    commits,
    canAutoMerge,
    media: mediaSummary(outcome),
  })
  const bundlePath = path.join(out, BUNDLE_FILE)
  writeFileSync(bundlePath, `${JSON.stringify(bundle, null, 2)}\n`)
  return {
    written: true,
    bundle: bundlePath,
    media: manifest ? path.join(out, MEDIA_MANIFEST) : null,
    proofMedia: out,
    spec: path.join(out, 'spec.json'),
    hangGuardMs: PROOF_AUTHOR_HANG_GUARD_MS,
    ...(outcome.reason ? { mediaReason: outcome.reason } : {}),
  }
}
