// The guard registry: one record per guard under scripts/guards/registry/<id>.mjs, each holding
// `export default { … }`. Many tickets register guards in parallel, so a directory of records
// merges cleanly where a single array file would conflict on every rebase.
//
// A record declares the files its guard reads (`scans`). Guards built on the registry read their
// file list through scannedFiles(repoRoot, record.scans), so the declared scans and the real
// scans cannot drift apart, and check-registry.mjs can prove every guard scans at least one file
// and that its CI job triggers on every file it scans.

import fs from 'node:fs'
import path from 'node:path'
import { pathToFileURL } from 'node:url'

import { globToRegExp } from '../../skills-toolbox/skill-config.mjs'
import { trackedFiles } from '../tree-scan-lib.mjs'

export const REGISTRY_DIR = 'scripts/guards/registry'

const RECORD_KEYS = ['id', 'rule', 'command', 'wiring', 'scans', 'baseline', 'proof']
const ID_PATTERN = /^[a-z0-9]+(?:-[a-z0-9]+)*$/

/** @type {(value: unknown) => value is Record<string, any>} */
const isPlainObject = (value) =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
const isNonEmptyString = (value) => typeof value === 'string' && value.trim() !== ''

function checkKeys(value, keys, label, fail) {
  for (const key of Object.keys(value)) {
    if (!keys.includes(key)) fail(`${label} has unknown field ${key}`)
  }
}

/**
 * Structural errors in one record. Reads only structure, never the wording of the rule.
 *
 * @param {unknown} record The module's default export.
 * @param {string} file Repo-relative path of the record file.
 * @returns {string[]}
 */
export function validateRecord(record, file) {
  if (!isPlainObject(record)) return [`${file}: must \`export default\` a record object`]
  const errors = []
  const fail = (message) => errors.push(`${file}: ${message}`)
  checkKeys(record, RECORD_KEYS, 'record', fail)
  for (const key of RECORD_KEYS) {
    if (!Object.hasOwn(record, key)) fail(`missing field ${key}`)
  }
  const expectedId = path.basename(file, '.mjs')
  if (!isNonEmptyString(record.id) || !ID_PATTERN.test(record.id)) {
    fail('id must be a kebab-case string')
  } else if (record.id !== expectedId) {
    fail(`id ${record.id} must equal the filename (${expectedId})`)
  }
  if (!isNonEmptyString(record.rule) || /[\r\n]/.test(record.rule)) {
    fail('rule must be a single non-empty line')
  }
  if (!isNonEmptyString(record.command)) fail('command must be a non-empty string')
  if (!isPlainObject(record.wiring)) {
    if (Object.hasOwn(record, 'wiring')) fail('wiring must be an object { make, ci }')
  } else {
    // A gate reached only from CI (a release-tier job, the mirror push) has no make target, so
    // `make` may be null; the CI job is always required.
    checkKeys(record.wiring, ['make', 'ci'], 'wiring', fail)
    if (record.wiring.make !== null && !isNonEmptyString(record.wiring.make)) {
      fail('wiring.make must be a make target name, or null for a CI-only gate')
    }
    if (!isPlainObject(record.wiring.ci)) {
      fail('wiring.ci must be an object { workflow, job }')
    } else {
      checkKeys(record.wiring.ci, ['workflow', 'job'], 'wiring.ci', fail)
      if (!isNonEmptyString(record.wiring.ci.workflow)) fail('wiring.ci.workflow must be a string')
      if (!isNonEmptyString(record.wiring.ci.job)) fail('wiring.ci.job must be a string')
    }
  }
  if (
    !Array.isArray(record.scans) ||
    record.scans.length === 0 ||
    !record.scans.every(isNonEmptyString)
  ) {
    if (Object.hasOwn(record, 'scans')) fail('scans must be a non-empty array of globs')
  }
  if (record.baseline !== null && !isNonEmptyString(record.baseline)) {
    if (Object.hasOwn(record, 'baseline')) fail('baseline must be null or a repo-relative path')
  }
  if (!isPlainObject(record.proof)) {
    if (Object.hasOwn(record, 'proof')) fail('proof must be an object { file, test }')
  } else {
    checkKeys(record.proof, ['file', 'test'], 'proof', fail)
    if (!isNonEmptyString(record.proof.file)) fail('proof.file must be a repo-relative path')
    if (!isNonEmptyString(record.proof.test)) fail('proof.test must name a test')
  }
  return errors
}

/**
 * Import every record under scripts/guards/registry/, sorted by id.
 *
 * @param {string} repoRoot Absolute repository root.
 * @returns {Promise<{records: object[], errors: string[]}>} Records that failed validation are
 *   left out of `records` and reported in `errors`.
 */
export async function loadRegistry(repoRoot) {
  const dir = path.join(repoRoot, ...REGISTRY_DIR.split('/'))
  let names
  try {
    names = fs
      .readdirSync(dir)
      .filter((name) => name.endsWith('.mjs'))
      .sort()
  } catch (error) {
    return { records: [], errors: [`cannot read ${REGISTRY_DIR}: ${error.message}`] }
  }
  const records = []
  const errors = []
  const seen = new Map()
  for (const name of names) {
    const file = `${REGISTRY_DIR}/${name}`
    let record
    try {
      record = (await import(pathToFileURL(path.join(dir, name)).href)).default
    } catch (error) {
      errors.push(`${file}: cannot import: ${error.message}`)
      continue
    }
    const problems = validateRecord(record, file)
    if (problems.length > 0) {
      errors.push(...problems)
      continue
    }
    if (seen.has(record.id)) {
      errors.push(`${file}: id ${record.id} is already registered by ${seen.get(record.id)}`)
      continue
    }
    seen.set(record.id, file)
    records.push(record)
  }
  records.sort((a, b) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0))
  return { records, errors }
}

/**
 * The tracked files matching any of `globs`, as sorted repo-relative paths. Globs use the same
 * matcher (skills-toolbox/skill-config.mjs globToRegExp) the registry check uses, so a guard's
 * declared scans and its real scans resolve the same way.
 *
 * @param {string} repoRoot Absolute repository root.
 * @param {string[]} globs
 * @param {{tracked?: string[]}} [options] A precomputed `git ls-files` list.
 * @returns {string[]}
 */
export function scannedFiles(repoRoot, globs, { tracked = trackedFiles(repoRoot) } = {}) {
  const patterns = globs.map(globToRegExp)
  return tracked.filter((file) => patterns.some((pattern) => pattern.test(file))).sort()
}
