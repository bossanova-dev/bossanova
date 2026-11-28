#!/usr/bin/env node

// guard-id: feature-setup-checks

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { parseFrontmatter } from '../../.claude/skills/verify/lib/features.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { compareBaseline, readBaseline, splitBaselineEntry } from './baseline.mjs'
import record from './registry/feature-setup-checks.mjs'
import { scannedFiles } from './registry.mjs'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../..')
const IDS_PATH = 'services/bossd/internal/setupcheck/ids.json'
const BASELINE_PATH = record.baseline

/**
 * @typedef {{
 *   file: string,
 *   id: string,
 *   setupChecks?: unknown,
 *   setupExempt?: unknown,
 * }} FeatureSetupEntry
 */

/**
 * @param {unknown} value
 * @returns {{ ids: string[], error: string | null }}
 */
function validateSetupCheckIds(value) {
  if (!Array.isArray(value) || value.length === 0) {
    return { ids: [], error: 'ids must be a non-empty array of strings' }
  }
  /** @type {string[]} */
  const ids = []
  /** @type {Set<string>} */
  const seen = new Set()
  for (const entry of value) {
    if (typeof entry !== 'string' || entry.trim() === '') {
      return { ids: [], error: 'ids must contain only non-empty strings' }
    }
    if (seen.has(entry)) {
      return { ids: [], error: `ids contains duplicate entry ${entry}` }
    }
    seen.add(entry)
    ids.push(entry)
  }
  return { ids, error: null }
}

/**
 * @param {string} root
 * @returns {{ ids: string[], errors: string[] }}
 */
function readSetupCheckIds(root) {
  let raw
  try {
    raw = fs.readFileSync(path.join(root, IDS_PATH), 'utf8')
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    return {
      ids: [],
      errors: [`${IDS_PATH}: setup-check-ids-unreadable: cannot read file (${message})`],
    }
  }
  /** @type {unknown} */
  let parsed
  try {
    parsed = JSON.parse(raw)
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error)
    return {
      ids: [],
      errors: [`${IDS_PATH}: setup-check-ids-unreadable: invalid JSON (${message})`],
    }
  }
  if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
    return {
      ids: [],
      errors: [`${IDS_PATH}: setup-check-ids-unreadable: expected a JSON object with an ids array`],
    }
  }
  const idsField = /** @type {{ ids?: unknown }} */ (parsed).ids
  const validated = validateSetupCheckIds(idsField)
  if (validated.error) {
    return {
      ids: [],
      errors: [`${IDS_PATH}: setup-check-ids-unreadable: ${validated.error}`],
    }
  }
  return { ids: validated.ids, errors: [] }
}

/**
 * Evaluate setup check declarations for parsed feature files against valid check ids and a
 * shrink-only baseline.
 *
 * @param {{
 *   features: FeatureSetupEntry[],
 *   ids: string[],
 *   baseline?: string[],
 *   baselinePath?: string,
 * }} input
 * @returns {string[]}
 */
export function setupCheckFailures({ features, ids, baseline = [], baselinePath = BASELINE_PATH }) {
  const validated = validateSetupCheckIds(ids)
  if (validated.error) {
    return [`${IDS_PATH}: setup-check-ids-unreadable: ${validated.error}`]
  }
  const validIds = new Set(validated.ids)
  /** @type {string[]} */
  const failures = []
  /** @type {string[]} */
  const baselineKeys = []
  /** @type {Set<string>} */
  const seenBaselineKeys = new Set()

  for (const entry of baseline) {
    const { key } = splitBaselineEntry(entry)
    if (key.trim() === '') {
      failures.push(`${baselinePath}: baseline entry ${JSON.stringify(entry)} has an empty key`)
    } else if (seenBaselineKeys.has(key)) {
      failures.push(`${baselinePath}: duplicate baseline key ${key}`)
    } else {
      seenBaselineKeys.add(key)
      baselineKeys.push(key)
    }
  }

  /** @type {Map<string, string[]>} */
  const undeclaredById = new Map()

  for (const feature of features) {
    const hasChecks = feature.setupChecks !== undefined
    const hasExempt = feature.setupExempt !== undefined

    if (hasChecks && hasExempt) {
      failures.push(
        `${feature.file}: setup-both-declared: declare either setupChecks or setupExempt, not both`,
      )
      continue
    }

    if (!hasChecks && !hasExempt) {
      const files = undeclaredById.get(feature.id) ?? []
      files.push(feature.file)
      undeclaredById.set(feature.id, files)
      continue
    }

    if (hasChecks) {
      const checks = feature.setupChecks
      if (
        !Array.isArray(checks) ||
        checks.length === 0 ||
        checks.some((item) => typeof item !== 'string' || item.trim() === '')
      ) {
        failures.push(
          `${feature.file}: setup-empty-list: setupChecks must be a non-empty list of setup check ids`,
        )
        continue
      }
      /** @type {Set<string>} */
      const seen = new Set()
      /** @type {Set<string>} */
      const reportedDup = new Set()
      for (const checkId of checks) {
        if (seen.has(checkId)) {
          if (!reportedDup.has(checkId)) {
            reportedDup.add(checkId)
            failures.push(
              `${feature.file}: setup-duplicate-id: ${checkId} appears more than once in setupChecks`,
            )
          }
          continue
        }
        seen.add(checkId)
        if (!validIds.has(checkId)) {
          failures.push(
            `${feature.file}: setup-unknown-id: ${checkId} is not in ${IDS_PATH}; run make setup-check-ids after registering it`,
          )
        }
      }
      continue
    }

    const exempt = feature.setupExempt
    if (typeof exempt !== 'string' || exempt.trim() === '' || /[\r\n]/.test(exempt)) {
      failures.push(
        `${feature.file}: setup-exempt-reason: setupExempt must be a non-empty single-line reason`,
      )
    }
  }

  const comparison = compareBaseline({
    baselinePath,
    entries: baselineKeys,
    offenders: undeclaredById.keys(),
    remedy: 'setup-undeclared',
  })

  for (const id of comparison.newViolations) {
    for (const file of undeclaredById.get(id) ?? []) {
      failures.push(
        `${file}: setup-undeclared: declare setupChecks or a reasoned setupExempt in frontmatter`,
      )
    }
  }
  for (const entry of comparison.staleEntries) {
    failures.push(`${baselinePath}: ${entry} no longer occurs — remove it`)
  }

  return failures
}

/**
 * `parseFrontmatter` drops indented continuations and treats block scalar markers (`|`, `>`) as
 * single-line strings. Inspect raw frontmatter lines so multiline or non-scalar `setupExempt`
 * declarations still reach `setupCheckFailures` as invalid (`null`) rather than passing.
 *
 * @param {string} text
 * @param {unknown} parsedValue
 * @returns {unknown}
 */
function resolveSetupExempt(text, parsedValue) {
  const frontmatter = /^---\r?\n([\s\S]*?)\r?\n---[ \t]*(?:\r?\n|$)/.exec(text)?.[1]
  if (frontmatter === undefined) return parsedValue
  const lines = frontmatter.split(/\r?\n/)
  /** @type {{ index: number, value: string }[]} */
  const matches = []
  for (let i = 0; i < lines.length; i++) {
    const match = /^setupExempt:(?:[ \t]+(.*)|[ \t]*)$/.exec(lines[i])
    if (match) matches.push({ index: i, value: (match[1] ?? '').trim() })
  }
  if (matches.length === 0) return parsedValue
  if (matches.length > 1) return null
  const { index, value } = matches[0]
  if (value === '' || /^[|>[{]/.test(value)) return null
  if (value.startsWith("'") && !/^'(?:[^']|'')*'$/.test(value)) return null
  if (value.startsWith('"') && !/^"(?:[^"\\]|\\.)*"$/.test(value)) return null
  let next = index + 1
  while (next < lines.length && lines[next].trim() === '') next++
  if (next < lines.length && /^\s+\S/.test(lines[next])) return null
  return parsedValue
}

/**
 * Run the feature-setup-checks guard against `root`.
 *
 * @param {string} [root]
 * @returns {{ ok: boolean, failures: string[], checked: number }}
 */
export function run(root = ROOT) {
  const resolvedRoot = path.resolve(root)
  const idResult = readSetupCheckIds(resolvedRoot)
  if (idResult.errors.length > 0) {
    return { ok: false, failures: idResult.errors, checked: 0 }
  }

  const baseline = readBaseline(resolvedRoot, BASELINE_PATH)
  const files = scannedFiles(resolvedRoot, record.scans).filter(
    (file) => file.startsWith('docs/features/') && file.endsWith('.md'),
  )

  /** @type {FeatureSetupEntry[]} */
  const features = []
  for (const file of files) {
    const abs = path.join(resolvedRoot, file)
    if (!fs.existsSync(abs)) continue
    const text = fs.readFileSync(abs, 'utf8')
    /** @type {Record<string, unknown>} */
    const data = parseFrontmatter(text).data
    if (typeof data.id !== 'string' || data.id.trim() === '') continue
    features.push({
      file,
      id: data.id,
      setupChecks: data.setupChecks,
      setupExempt: resolveSetupExempt(text, data.setupExempt),
    })
  }

  const failures = [
    ...baseline.errors,
    ...setupCheckFailures({
      features,
      ids: idResult.ids,
      baseline: baseline.entries,
      baselinePath: BASELINE_PATH,
    }),
  ]
  return { ok: failures.length === 0, failures, checked: features.length }
}

if (isMainModule(import.meta.url)) {
  try {
    const { values } = parseArgs({
      strict: true,
      allowPositionals: false,
      options: {
        check: { type: 'boolean', default: false },
        root: { type: 'string', default: ROOT },
      },
    })
    if (!values.check) {
      throw new Error('usage: feature-setup-checks.mjs --check [--root <path>]')
    }
    const result = run(values.root)
    for (const failure of result.failures) console.error(failure)
    if (result.ok) console.log(`Feature setup checks OK (${result.checked} features)`)
    process.exitCode = result.ok ? 0 : 1
  } catch (error) {
    console.error(`feature-setup-checks: ${error instanceof Error ? error.message : String(error)}`)
    process.exitCode = 1
  }
}
