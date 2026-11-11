import { mkdirSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { readMergePolicy, safeMergePolicyPath } from './verify-merge-prose.mjs'

/** The caller owns the scratch directory and removes it when replay ends.
 * @param {string} scratch @param {string|undefined} path @param {Buffer|undefined} bytes
 */
export function candidatePolicyReader(scratch, path, bytes) {
  let privateRoot
  /** @type {typeof readMergePolicy} */
  return (block, root) => {
    if (!root || bytes === undefined || !path || !safeMergePolicyPath(path))
      return readMergePolicy(block, root)
    if (privateRoot === undefined) {
      privateRoot = mkdtempSync(join(scratch, 'policy-'))
      const file = join(privateRoot, path)
      mkdirSync(dirname(file), { recursive: true })
      writeFileSync(file, bytes)
    }
    const draft = readMergePolicy(block, privateRoot)
    if (draft.state !== 'ok') return draft
    // The draft replaces the text, while the base tree still owns the policy's path dependencies.
    const base = readMergePolicy(block, root)
    return base.state === 'ok' && base.dependencies
      ? { ...draft, dependencies: base.dependencies }
      : draft
  }
}

/** @param {import('./verify-merge-prose.mjs').MergePolicy} policy */
export function policySummary(policy) {
  if (policy.state === 'ok') return { state: policy.state, path: policy.path, blob: policy.blob }
  return policy
}

/** @param {string} name @param {number} pr @param {import('./verify-merge-prose.mjs').MergePolicy} policy */
export function replayStem(name, pr, policy) {
  const key = policy.state === 'ok' ? policy.blob.slice(0, 12) : 'none'
  return `${name}.pr${pr}.${key}`
}

/** @param {string} dir @param {string} stem @param {string} headSha */
export function replayJudgeOutput(dir, stem, headSha) {
  const file = join(dir, `${stem}.judge.json`)
  try {
    const output = JSON.parse(readFileSync(file, 'utf8'))
    return output?.head === headSha ? { file } : { invalid: true }
  } catch (error) {
    return error.code === 'ENOENT' ? {} : { invalid: true }
  }
}

/** @param {string} dir @param {string} stem @param {string} json */
export function writeReplayEnvelope(dir, stem, json) {
  const envelope = `${stem}.envelope.json`
  mkdirSync(dir, { recursive: true, mode: 0o700 })
  const staging = mkdtempSync(join(dir, '.replay-'))
  try {
    const file = join(staging, 'envelope.json')
    writeFileSync(file, json, { flag: 'wx', mode: 0o600 })
    renameSync(file, join(dir, envelope))
  } finally {
    rmSync(staging, { recursive: true, force: true, maxRetries: 3 })
  }
  return envelope
}

/** @param {import('./verify-merge-prose.mjs').PolicyDecision|null} decision */
export function replayDecision(decision) {
  if (!decision) return null
  return { ...decision, ...(!decision.verified ? { quoteCheck: 'failed' } : {}) }
}

/** @param {string} quote */
export function quoteSpan(quote) {
  const fence = '`'.repeat(Math.max(0, ...(quote.match(/`+/g) ?? []).map((run) => run.length)) + 1)
  return `${fence} ${quote} ${fence}`
}
