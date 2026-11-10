import { createHash } from 'node:crypto'
import * as defaultFs from 'node:fs'
import { dirname, isAbsolute, parse, relative, resolve, sep } from 'node:path'
import { TextDecoder } from 'node:util'

/**
 * @typedef {{state: 'none'} | {state: 'ok', path: string, dependencies?: string[], blob: string, text: string} | {state: 'unreadable', path: string, why: string}} MergePolicy
 * @typedef {{ask?: string, decision: 'auto'|'human', rule: string, reason: string, path: string, verified: boolean}} PolicyDecision
 * @typedef {{path?: string, filename?: string, previousPath?: string, previous_filename?: string}} PolicyFile
 * @typedef {{state: MergePolicy['state'], decisions: PolicyDecision[], stale: boolean}} ProseFacts
 */
export const MERGE_POLICY_LIMITS = Object.freeze({
  maxBytes: 32768,
  maxDecisions: 16,
  maxRuleLength: 400,
  minRuleLength: 16,
  maxReasonLength: 200,
})

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)
/** @param {unknown} value @param {number} max @returns {value is string} */
const line = (value, max) =>
  typeof value === 'string' &&
  value.trim().length > 0 &&
  value.length <= max &&
  !/[\r\n\u2028\u2029]/.test(value)
export const JUDGE_LIMITS = Object.freeze({ maxAreas: 8, maxLabelLength: 60, maxReasonLength: 200 })
/** @param {unknown} value @returns {value is {label: string, path: string, reason: string}} */
export const validJudgeEntry = (value) =>
  object(value) &&
  line(value.label, JUDGE_LIMITS.maxLabelLength) &&
  typeof value.path === 'string' &&
  value.path.length > 0 &&
  line(value.reason, JUDGE_LIMITS.maxReasonLength)

/** @param {string} path */
const safePath = (path) =>
  path.length > 0 &&
  path.length <= 200 &&
  path.endsWith('.md') &&
  !isAbsolute(path) &&
  !/[\\\0]/.test(path) &&
  !/^[A-Za-z]:/.test(path) &&
  !path.split('/').some((part) => part === '.' || part === '..')
/** @param {string} root @param {string} path */
const inside = (root, path) => {
  const rel = relative(root, path)
  return rel !== '..' && !rel.startsWith('../') && !isAbsolute(rel)
}

/** @param {Uint8Array} bytes */
export function gitBlobSha(bytes) {
  return createHash('sha1').update(`blob ${bytes.byteLength}\0`).update(bytes).digest('hex')
}

/** @param {string} root @param {string} path @param {typeof defaultFs} fs */
function policyDependencies(root, path, fs) {
  const dependencies = new Set([path])
  const pending = path.split('/')
  let current = root,
    links = 0
  while (pending.length) {
    const part = pending.shift()
    if (!part || part === '.') continue
    current = part === '..' ? dirname(current) : resolve(current, part)
    if (!fs.lstatSync(current).isSymbolicLink()) continue
    if (++links > 40) throw new Error('symlink-loop')
    if (inside(root, current)) dependencies.add(relative(root, current).split(sep).join('/'))
    let target = fs.readlinkSync(current)
    current = dirname(current)
    if (isAbsolute(target)) {
      current = parse(target).root
      target = target.slice(current.length)
    }
    pending.unshift(...target.split(sep))
  }
  dependencies.add(relative(root, current).split(sep).join('/'))
  return [...dependencies]
}

/** Read only the trusted base snapshot; invalid or inaccessible input always holds.
 * @param {unknown} block @param {string|null} root @param {typeof defaultFs} [fs] @returns {MergePolicy}
 */
export function readMergePolicy(block, root, fs = defaultFs) {
  const path = object(block) && object(block.merge) ? block.merge.policy : undefined
  if (path === undefined) return { state: 'none' }
  /** @param {string} why @returns {MergePolicy} */
  const unreadable = (why) => ({
    state: 'unreadable',
    path: typeof path === 'string' ? path : '',
    why,
  })
  if (typeof path !== 'string' || !safePath(path)) return unreadable('unsafe-path')
  if (!root) return unreadable('no-trusted-tree')
  try {
    const realRoot = fs.realpathSync(root)
    const realPath = fs.realpathSync(resolve(root, path))
    if (!inside(realRoot, realPath)) return unreadable('outside-tree')
    const dependencies = policyDependencies(realRoot, path, fs)
    const stat = fs.statSync(realPath)
    if (!stat.isFile()) return unreadable('not-a-file')
    if (stat.size > MERGE_POLICY_LIMITS.maxBytes) return unreadable('too-large')
    const bytes = fs.readFileSync(realPath)
    if (bytes.byteLength > MERGE_POLICY_LIMITS.maxBytes) return unreadable('too-large')
    let text
    try {
      text = new TextDecoder('utf-8', { fatal: true }).decode(bytes)
    } catch {
      return unreadable('not-utf8')
    }
    if (!text.trim()) return unreadable('empty')
    return {
      state: 'ok',
      path,
      blob: gitBlobSha(bytes),
      text,
      ...(dependencies.length > 1 ? { dependencies } : {}),
    }
  } catch {
    return unreadable('missing')
  }
}

/** @param {Record<string, unknown>|null|undefined} block @param {MergePolicy} policy */
export function fingerprintBlock(block, policy) {
  if (policy.state === 'none') return block
  return {
    ...block,
    merge: {
      ...(object(block?.merge) ? block.merge : {}),
      policyBlob: policy.state === 'ok' ? policy.blob : `unreadable:${policy.why}`,
    },
  }
}

/** @param {string} text */
export const normalizeRule = (text) => text.replace(/\s+/gu, ' ').trim()
/** @param {string} rule @param {string} text */
export function verifyRule(rule, text) {
  const quote = normalizeRule(rule),
    source = normalizeRule(text)
  if (quote.length < MERGE_POLICY_LIMITS.minRuleLength) return false
  let start = source.indexOf(quote)
  while (start !== -1) {
    const before = Array.from(source.slice(Math.max(0, start - 2), start)).at(-1) ?? ''
    const after = Array.from(source.slice(start + quote.length, start + quote.length + 2))[0] ?? ''
    if (!/[\p{L}\p{N}]/u.test(before) && !/[\p{L}\p{N}]/u.test(after)) return true
    start = source.indexOf(quote, start + 1)
  }
  return false
}

/** Canonical judge area id, shared by quoted decisions and the judge's added areas.
 * @param {string} label
 */
export const slug = (label) =>
  label
    .toLowerCase()
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-|-$/g, '') || encodeURIComponent(label.toLowerCase())

/** @param {unknown} value @param {boolean} [stored] */
function validDecision(value, stored = false) {
  return (
    object(value) &&
    Object.keys(value).every((key) =>
      ['ask', 'decision', 'rule', 'reason', 'path', ...(stored ? ['verified'] : [])].includes(key),
    ) &&
    (value.decision === 'auto' || value.decision === 'human') &&
    line(value.rule, MERGE_POLICY_LIMITS.maxRuleLength) &&
    line(value.reason, MERGE_POLICY_LIMITS.maxReasonLength) &&
    typeof value.path === 'string' &&
    value.path.length > 0 &&
    (value.ask === undefined || line(value.ask, 200)) &&
    (value.decision !== 'auto' || value.ask !== undefined) &&
    (!stored || typeof value.verified === 'boolean')
  )
}

/** @param {unknown} raw @param {{policy: MergePolicy, files: readonly PolicyFile[], judgeLabels?: readonly string[]}} context
 * @returns {{decisions: PolicyDecision[], health: string[]}}
 */
export function validateDecisions(raw, { policy, files, judgeLabels = [] }) {
  if (raw === undefined) return { decisions: [], health: [] }
  if (policy.state !== 'ok') return { decisions: [], health: ['judge-decisions-ignored'] }
  if (!Array.isArray(raw)) return { decisions: [], health: ['judge-decision-rejected'] }
  const changed = new Set(
    files
      .flatMap((file) => [file.path, file.filename, file.previousPath, file.previous_filename])
      .filter(Boolean),
  )
  const health = new Set()
  /** @type {PolicyDecision[]} */
  const decisions = []
  if (raw.length > MERGE_POLICY_LIMITS.maxDecisions) health.add('judge-decisions-truncated')
  for (const value of raw.slice(0, MERGE_POLICY_LIMITS.maxDecisions)) {
    if (!validDecision(value) || !changed.has(value.path)) {
      health.add('judge-decision-rejected')
      continue
    }
    const verified = verifyRule(value.rule, policy.text)
    const ask = judgeLabels.includes(value.ask) ? `judge:${slug(value.ask)}` : value.ask
    decisions.push({
      ...(ask === undefined ? {} : { ask }),
      decision: verified ? value.decision : 'human',
      rule: value.rule,
      reason: value.reason,
      path: value.path,
      verified,
    })
  }
  return { decisions, health: [...health] }
}

/** Decode the complete record atomically so neither half of a malformed record is trusted.
 * @param {unknown} value @returns {{areas: {label: string, path: string, reason: string}[], policyBlob?: string, decisions?: PolicyDecision[]}|null}
 */
export function decodePolicyRecord(value) {
  if (typeof value !== 'string' || !/^[A-Za-z0-9_-]+$/.test(value)) return null
  try {
    const bytes = Buffer.from(value, 'base64url')
    if (bytes.toString('base64url') !== value) return null
    const decoded = JSON.parse(bytes.toString('utf8'))
    const areas = Array.isArray(decoded) ? decoded : decoded?.areas
    if (
      !Array.isArray(areas) ||
      areas.length > JUDGE_LIMITS.maxAreas ||
      !areas.every(
        (entry) =>
          validJudgeEntry(entry) &&
          Object.keys(entry).every((key) => ['label', 'path', 'reason'].includes(key)),
      )
    )
      return null
    if (Array.isArray(decoded)) return { areas }
    if (
      !object(decoded) ||
      Object.keys(decoded).some((key) => !['areas', 'policyBlob', 'decisions'].includes(key)) ||
      typeof decoded.policyBlob !== 'string' ||
      !Array.isArray(decoded.decisions) ||
      decoded.decisions.length > MERGE_POLICY_LIMITS.maxDecisions ||
      !decoded.decisions.every((entry) => validDecision(entry, true))
    )
      return null
    return { areas, policyBlob: decoded.policyBlob, decisions: decoded.decisions }
  } catch {
    return null
  }
}

/** Records have already been filtered to this head and trusted authors by the gate.
 * @param {MergePolicy} policy @param {readonly string[]} records @param {{headSha: string, files: readonly PolicyFile[]}} context @returns {ProseFacts}
 */
export function proseFacts(policy, records, context) {
  if (policy.state !== 'ok') return { state: policy.state, decisions: [], stale: false }
  /** @type {PolicyDecision[]} */
  const decisions = []
  let current = false
  for (const record of records) {
    const decoded = decodePolicyRecord(record)
    if (!decoded || decoded.policyBlob !== policy.blob) continue
    current = true
    const raw = (decoded.decisions ?? []).map(({ verified: _verified, ...decision }) => decision)
    decisions.push(
      ...validateDecisions(raw, {
        policy,
        files: context.files,
        judgeLabels: decoded.areas.map((area) => area.label),
      }).decisions,
    )
  }
  return { state: 'ok', decisions, stale: !current }
}

/** @param {readonly (string|PolicyFile)[]} changedPaths @param {MergePolicy} policy */
export function touchesMergePolicy(changedPaths, policy) {
  if (policy.state === 'none') return false
  const paths = new Set([
    policy.path,
    ...(policy.state === 'ok' ? (policy.dependencies ?? []) : []),
  ])
  return changedPaths.some((file) =>
    (typeof file === 'string'
      ? [file]
      : [file.path, file.filename, file.previousPath, file.previous_filename]
    ).some((path) => typeof path === 'string' && paths.has(path)),
  )
}
