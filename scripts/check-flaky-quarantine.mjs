#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'

// The flaky-test quarantine. scripts/bazel/flaky-tests.json lists every test CI is allowed to retry,
// one entry per Bazel go_test target, each with an owner, a tracking reference and an expiry. The
// list is the only licence for a retry: Bazel retries a target only when its BUILD rule carries
// `flaky = True`, and scripts/bazel/run-ledger.mjs retries a native ledger row only when its label is
// listed here. This gate keeps the two in step and makes every entry temporary.
//
// Entry shape: {"label": "//pkg:target_test", "tests": ["TestName"], "owner": "@handle",
// "tracking": "<issue URL>", "expires": "YYYY-MM-DD", "reason": "<what races>"}. An empty list is
// the goal state: fix the flake, then delete its entry and its `flaky = True`.
//
// It fails on:
//   - an entry missing its label, tests, owner, tracking reference or expiry;
//   - an expired entry (today is after `expires`), or one expiring more than MAX_HORIZON_DAYS out;
//   - a listed test function that no longer exists in the target's package;
//   - a listed Bazel target whose BUILD rule lacks `flaky = True` (ledger-only `manual` rows are
//     retried by run-ledger.mjs and need no attribute);
//   - a `flaky = True` on any BUILD rule the list does not name.

export const QUARANTINE_FILE = 'scripts/bazel/flaky-tests.json'
export const LEDGER_FILE = 'scripts/bazel/ledger.json'
export const MAX_HORIZON_DAYS = 60

const REQUIRED_TEXT_FIELDS = ['label', 'owner', 'tracking', 'expires', 'reason']
const LABEL_RE = /^\/\/([A-Za-z0-9_\-./]+):([A-Za-z0-9_\-.]+)$/
const DATE_RE = /^\d{4}-\d{2}-\d{2}$/
const SKIP_DIRS = new Set(['.git', 'node_modules', '.claude', 'dist'])

export function parseLabel(label) {
  const match = LABEL_RE.exec(label ?? '')
  return match ? { pkg: match[1], target: match[2] } : null
}

function validDate(text) {
  if (!DATE_RE.test(text ?? '')) return false
  const parsed = new Date(`${text}T00:00:00Z`)
  return !Number.isNaN(parsed.getTime()) && parsed.toISOString().slice(0, 10) === text
}

function daysBetween(from, to) {
  return Math.round((Date.parse(`${to}T00:00:00Z`) - Date.parse(`${from}T00:00:00Z`)) / 86_400_000)
}

/**
 * Parse scripts/bazel/flaky-tests.json text into its entry array.
 *
 * @param {string} text
 * @returns {object[]}
 */
export function parseQuarantine(text) {
  const parsed = JSON.parse(text)
  if (!Array.isArray(parsed)) throw new Error(`${QUARANTINE_FILE} must be a JSON array of entries`)
  return parsed
}

/** Labels the native ledger runner may retry, read from the repo's quarantine file. */
export function quarantinedLabels(text) {
  return new Set(
    parseQuarantine(text)
      .map((entry) => entry?.label)
      .filter(Boolean),
  )
}

/**
 * Every rule in one BUILD file: its name, whether it carries `flaky = True`, and its first line.
 * Rules are gazelle-formatted (`kind(` at column 0, `)` closing at column 0).
 *
 * @param {string} text BUILD.bazel contents.
 * @returns {{kind: string, name: string|null, flaky: boolean, line: number}[]}
 */
export function parseBuildRules(text) {
  const rules = []
  let current = null
  text.split('\n').forEach((raw, index) => {
    const start = /^([A-Za-z_][\w]*)\($/.exec(raw)
    if (start) {
      current = { kind: start[1], name: null, flaky: false, line: index + 1 }
      return
    }
    if (!current) return
    if (raw === ')') {
      rules.push(current)
      current = null
      return
    }
    const name = /^\s+name\s*=\s*"([^"]+)"/.exec(raw)
    if (name) current.name = name[1]
    if (/^\s+flaky\s*=\s*True\b/.test(raw)) current.flaky = true
  })
  return rules
}

/**
 * The pure core: every problem with the quarantine, given its inputs.
 *
 * @param {object} input
 * @param {object[]} input.entries Parsed quarantine entries.
 * @param {Map<string, string>} input.buildFiles Repo-relative BUILD.bazel path -> contents, for every BUILD file in the tree.
 * @param {(pkg: string) => string[]} input.testSources Contents of each `*_test.go` in a package dir.
 * @param {Set<string>} input.ledgerLabels Labels of native ledger rows (bazel `manual` targets).
 * @param {string} input.today YYYY-MM-DD.
 * @returns {string[]}
 */
export function findQuarantineProblems({ entries, buildFiles, testSources, ledgerLabels, today }) {
  const problems = []
  const listed = new Map()

  entries.forEach((entry, index) => {
    const where = `entry ${index} (${entry?.label ?? 'no label'})`
    for (const field of REQUIRED_TEXT_FIELDS) {
      if (typeof entry?.[field] !== 'string' || entry[field].trim() === '') {
        problems.push(`${where}: missing ${field}`)
      }
    }
    if (!Array.isArray(entry?.tests) || entry.tests.length === 0) {
      problems.push(`${where}: missing tests (the test function names this entry quarantines)`)
    }
    if (typeof entry?.expires === 'string' && entry.expires !== '') {
      if (!validDate(entry.expires)) {
        problems.push(`${where}: expires "${entry.expires}" is not a YYYY-MM-DD date`)
      } else if (entry.expires < today) {
        problems.push(
          `${where}: EXPIRED on ${entry.expires} — fix the flake and delete the entry, or renew it with a new expiry and a reason`,
        )
      } else if (daysBetween(today, entry.expires) > MAX_HORIZON_DAYS) {
        problems.push(`${where}: expires ${entry.expires}, more than ${MAX_HORIZON_DAYS} days out`)
      }
    }

    const parsed = parseLabel(entry?.label)
    if (entry?.label && !parsed) {
      problems.push(`${where}: label is not a //package:target Bazel label`)
      return
    }
    if (!parsed) return
    if (listed.has(entry.label)) problems.push(`${where}: duplicate label; merge the entries`)
    listed.set(entry.label, entry)

    const sources = testSources(parsed.pkg)
    for (const name of Array.isArray(entry.tests) ? entry.tests : []) {
      const declared = new RegExp(`^func ${name}\\(t \\*testing\\.T\\)`, 'm')
      if (!sources.some((source) => declared.test(source))) {
        problems.push(`${where}: test ${name} not found in ${parsed.pkg}/*_test.go`)
      }
    }

    if (ledgerLabels.has(entry.label)) return
    const buildPath = `${parsed.pkg}/BUILD.bazel`
    const rule = parseBuildRules(buildFiles.get(buildPath) ?? '').find(
      (candidate) => candidate.name === parsed.target,
    )
    if (!rule) {
      problems.push(`${where}: no rule named ${parsed.target} in ${buildPath}`)
    } else if (!rule.flaky) {
      problems.push(
        `${where}: ${buildPath} rule ${parsed.target} lacks \`flaky = True,\` (gazelle keeps the attribute but sorts it after \`embed\`; run \`bazel run //:gazelle\` to place it)`,
      )
    }
  })

  for (const [buildPath, text] of buildFiles) {
    const pkg = path.posix.dirname(buildPath)
    for (const rule of parseBuildRules(text)) {
      if (!rule.flaky) continue
      const label = `//${pkg}:${rule.name}`
      if (!listed.has(label)) {
        problems.push(
          `${buildPath}:${rule.line} ${label} has flaky = True but no ${QUARANTINE_FILE} entry (owner, tracking, expiry)`,
        )
      }
    }
  }

  return problems
}

function listBuildFiles(repoRoot, fsImpl, dir = '', out = new Map()) {
  for (const entry of fsImpl.readdirSync(path.join(repoRoot, dir), { withFileTypes: true })) {
    const relative = dir ? `${dir}/${entry.name}` : entry.name
    if (entry.isDirectory()) {
      if (SKIP_DIRS.has(entry.name) || entry.name.startsWith('bazel-')) continue
      listBuildFiles(repoRoot, fsImpl, relative, out)
    } else if (entry.name === 'BUILD.bazel') {
      out.set(relative, fsImpl.readFileSync(path.join(repoRoot, relative), 'utf8'))
    }
  }
  return out
}

export function gatherRepoInputs(repoRoot, { fs: fsImpl = fs } = {}) {
  const read = (relative) => fsImpl.readFileSync(path.join(repoRoot, relative), 'utf8')
  return {
    entries: parseQuarantine(read(QUARANTINE_FILE)),
    buildFiles: listBuildFiles(repoRoot, fsImpl),
    testSources: (pkg) => {
      const dir = path.join(repoRoot, pkg)
      if (!fsImpl.existsSync(dir)) return []
      return fsImpl
        .readdirSync(dir)
        .filter((name) => name.endsWith('_test.go'))
        .map((name) => fsImpl.readFileSync(path.join(dir, name), 'utf8'))
    },
    ledgerLabels: new Set(JSON.parse(read(LEDGER_FILE)).map((row) => row.label)),
  }
}

function main() {
  const { values } = parseArgs({
    options: { today: { type: 'string' } },
    strict: true,
    allowPositionals: false,
  })
  const today = values.today ?? new Date().toISOString().slice(0, 10)
  if (!validDate(today)) {
    console.error(`check-flaky-quarantine: --today "${today}" is not a YYYY-MM-DD date`)
    process.exit(2)
  }
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const inputs = gatherRepoInputs(repoRoot)
  const problems = findQuarantineProblems({ ...inputs, today })
  if (problems.length > 0) {
    console.error(`Flaky-test quarantine problems (${QUARANTINE_FILE}, as of ${today}):`)
    for (const problem of problems) console.error(`  - ${problem}`)
    process.exit(1)
  }
  console.log(
    `Flaky-test quarantine OK: ${inputs.entries.length} entr${inputs.entries.length === 1 ? 'y' : 'ies'}, none expired, ` +
      `${inputs.buildFiles.size} BUILD.bazel files agree with ${QUARANTINE_FILE} (as of ${today}).`,
  )
}

if (isMainModule(import.meta.url)) main()
