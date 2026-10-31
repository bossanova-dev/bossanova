#!/usr/bin/env node

// The guard registry check. Fails, one line per problem, when a registered guard:
//   1. has a malformed record, or names a command script, make target or workflow that does not
//      exist;
//   2. names a proof test that is not a tracked `.test.mjs` / `_test.go` / `test_*.py` file declaring that test;
//   3. has a `scans` glob matching zero tracked files (each glob on its own);
//   4. scans a tracked file its CI workflow's push `paths:` filter misses;
//   5. has a baseline that fails the shared format (baseline.mjs);
// when scripts/guards/budgets.json is malformed or budgets an unregistered guard, or (when
// the golangci-uncapped guard is registered) .golangci.yml caps golangci output; and (check 8)
// when a gate exists that no record registers:
//   - a tracked scripts/check-*.mjs or scripts/check-*.sh that no record's command names and no
//     record's proof.file tests (its sibling scripts/check-*.test.mjs);
//   - a tracked scripts/check-*.test.mjs with no sibling script that no record's proof.file names;
//   - a `guard-id: <id>` marker comment whose <id> no record registers.
// Gates no filename pattern can find (Go tests, golangci rules, toolbox sources) carry that marker
// on a comment line of their own; a record with no pattern-found file must keep its marker in one
// of the files it names (command, proof.file, scans), so the link cannot drop from either side.
//
// The proof check is static: it confirms the test exists and is named. The proof tests run in
// `make test-scripts` (node), the Go suites, or `make test-warehouse` (Python).
// scripts/prove-red.mjs is the manual tool to show a node proof is not vacuous.

// guard-id: guard-registry

import { readRootMakefile } from '../dev/root-makefile-lib.mjs'
import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { globToRegExp } from '../../skills-toolbox/skill-config.mjs'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'
import { trackedFiles } from '../dev/tree-scan-lib.mjs'
import { changePaths, WORKFLOW_DIR } from '../dev/workflow-yaml-lib.mjs'
import { readBaseline } from './baseline.mjs'
import { BUDGETS_PATH, readBudgets } from './budgets.mjs'
import { checkGolangciCaps } from './golangci-caps.mjs'
import { loadRegistry, scannedFiles } from './registry.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const GOLANGCI_GUARD = 'golangci-uncapped'

const GATE_SCRIPT = /^scripts\/(?:guards\/)?check-[^/]+\.(?:mjs|sh)$/
const GATE_TEST = /^scripts\/(?:guards\/)?check-[^/]+\.test\.mjs$/
// A marker is a whole comment line: `// guard-id: x`, `# guard-id: x`, ` * guard-id: x`.
const MARKER_LINE =
  /^\s*(?:\/\/|#|\*|\/\*)\s*guard-id:\s*([a-z0-9]+(?:-[a-z0-9]+)*)\s*(?:\*\/)?\s*$/

const escapeRegExp = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')

function readText(repoRoot, relPath) {
  try {
    return fs.readFileSync(path.join(repoRoot, relPath), 'utf8')
  } catch {
    return null
  }
}

/**
 * The script a `node` or `bash` command runs: its first argument that is not a flag.
 *
 * @param {string} command
 * @returns {string | null}
 */
export function commandScript(command) {
  return /^(?:node|bash)\s+(?:-\S+\s+)*([^\s-]\S*)/.exec(command)?.[1] ?? null
}

/**
 * Every `guard-id: <id>` marker comment in a tracked file, read with `git grep`.
 *
 * @param {string} repoRoot
 * @returns {{file: string, line: number, id: string}[]}
 */
export function guardMarkers(repoRoot) {
  let out
  try {
    out = execFileSync('git', ['grep', '-z', '-n', '-I', '-F', '-e', 'guard-id:'], {
      cwd: repoRoot,
      encoding: 'utf8',
      maxBuffer: 64 << 20,
    })
  } catch (error) {
    if (error.status === 1) return []
    throw error
  }
  const markers = []
  for (const entry of out.split('\n')) {
    const [file, line, text] = entry.split('\0')
    const id = text === undefined ? null : MARKER_LINE.exec(text)?.[1]
    if (id) markers.push({ file, line: Number(line), id })
  }
  return markers
}

function makeTargets(repoRoot) {
  const text = readText(repoRoot, 'Makefile') === null ? '' : readRootMakefile(repoRoot)
  return (target) => new RegExp(`^${escapeRegExp(target)}:`, 'm').test(text)
}

function declaresTest(text, file, name) {
  if (file.endsWith('_test.go')) return new RegExp(`func ${escapeRegExp(name)}\\(`).test(text)
  if (file.endsWith('.py'))
    return new RegExp(`^\\s*def ${escapeRegExp(name)}\\s*\\(`, 'm').test(text)
  return ["'", '"', '`'].some((quote) => text.includes(`${quote}${name}${quote}`))
}

function workflowFilter(repoRoot, workflow, cache) {
  if (!cache.has(workflow)) {
    const text = readText(repoRoot, `${WORKFLOW_DIR}/${workflow}`)
    const paths = text === null ? null : changePaths(text)
    cache.set(workflow, {
      paths,
      negations: (paths ?? []).filter((entry) => entry.startsWith('!')),
      patterns: (paths ?? []).map(globToRegExp),
    })
  }
  return cache.get(workflow)
}

/**
 * Run every registry check.
 *
 * @param {{repoRoot?: string}} [options]
 * @returns {Promise<{failures: string[], guards: number, filesScanned: number}>}
 */
export async function checkRegistry({ repoRoot = REPO_ROOT } = {}) {
  const { records, errors } = await loadRegistry(repoRoot)
  const failures = [...errors]
  const tracked = trackedFiles(repoRoot)
  const trackedSet = new Set(tracked)
  const hasTarget = makeTargets(repoRoot)
  const filters = new Map()
  const allScanned = new Set()

  for (const record of records) {
    const fail = (message) => failures.push(`${record.id}: ${message}`)

    // 1. Schema references.
    const script = commandScript(record.command)
    if (script && !fs.existsSync(path.join(repoRoot, script))) {
      fail(`command runs ${script}, which does not exist`)
    }
    if (record.wiring.make !== null && !hasTarget(record.wiring.make)) {
      fail(`wiring.make ${record.wiring.make} is not a target in the root Makefile`)
    }
    const workflowPath = `${WORKFLOW_DIR}/${record.wiring.ci.workflow}`
    const workflowExists = readText(repoRoot, workflowPath) !== null
    if (!workflowExists) fail(`wiring.ci.workflow ${workflowPath} does not exist`)

    // Retired lessons keep their provenance in the record after their tracked deletion.
    for (const solution of record.origin?.solutions ?? []) {
      const parts = solution.split('/')
      if (
        !solution.startsWith('docs/solutions/') ||
        !solution.endsWith('.md') ||
        parts.some((part) => part === '' || part === '.' || part === '..') ||
        solution.includes('\\')
      ) {
        fail(`origin.solutions ${solution} must be a path under docs/solutions/ ending .md`)
      } else if (trackedSet.has(solution)) {
        fail(`origin.solutions ${solution} is still tracked; delete the replaced solution`)
      }
    }

    // 2. Proof.
    const { file: proofFile, test: proofTest } = record.proof
    if (
      !/(?:\.test\.mjs|_test\.go)$/.test(proofFile) &&
      !/(?:^|\/)test_[^/]+\.py$/.test(proofFile)
    ) {
      fail(`proof.file ${proofFile} must be a .test.mjs or _test.go file, or a test_*.py file`)
    } else if (!trackedSet.has(proofFile)) {
      fail(`proof.file ${proofFile} is not a tracked file`)
    } else if (!declaresTest(readText(repoRoot, proofFile) ?? '', proofFile, proofTest)) {
      fail(`proof.file ${proofFile} declares no test named ${JSON.stringify(proofTest)}`)
    }

    // 3. Zero files scanned, glob by glob.
    for (const glob of record.scans) {
      if (scannedFiles(repoRoot, [glob], { tracked }).length === 0) {
        fail(`scans glob ${glob} matches zero tracked files`)
      }
    }
    const scanned = scannedFiles(repoRoot, record.scans, { tracked })
    for (const file of scanned) allScanned.add(file)

    // 4. CI path gap.
    if (workflowExists) {
      const filter = workflowFilter(repoRoot, record.wiring.ci.workflow, filters)
      if (filter.negations.length > 0) {
        fail(
          `${workflowPath} trigger paths use negation (${filter.negations.join(', ')}), which this check cannot evaluate`,
        )
      } else if (filter.paths !== null) {
        for (const file of scanned) {
          if (!filter.patterns.some((pattern) => pattern.test(file))) {
            fail(`scans ${file}, which no push or pull_request path in ${workflowPath} matches`)
          }
        }
      }
    }

    // 5. Baseline format.
    if (record.baseline !== null) {
      if (record.baselineFormat === 'json') {
        try {
          JSON.parse(fs.readFileSync(path.join(repoRoot, record.baseline), 'utf8'))
        } catch (error) {
          fail(`cannot read ${record.baseline}: ${error.message}`)
        }
      } else {
        for (const error of readBaseline(repoRoot, record.baseline).errors) fail(error)
      }
    }
  }

  // 6. Budgets.
  const { budgets, errors: budgetErrors } = readBudgets(repoRoot)
  failures.push(...budgetErrors)
  const ids = new Set(records.map((record) => record.id))
  for (const guardId of Object.keys(budgets)) {
    const suppressionOwner = guardId.endsWith('-suppressions')
      ? guardId.slice(0, -'-suppressions'.length)
      : null
    const ownedSuppressions =
      suppressionOwner !== null &&
      ids.has(suppressionOwner) &&
      budgets[guardId].default === undefined &&
      budgets[guardId].subjects !== undefined
    if (!ids.has(guardId) && !ownedSuppressions)
      failures.push(`${BUDGETS_PATH}: ${guardId} is not a registered guard`)
  }

  // 7. golangci caps, registered as its own rule.
  if (ids.has(GOLANGCI_GUARD)) {
    const text = readText(repoRoot, '.golangci.yml')
    if (text === null) failures.push(`${GOLANGCI_GUARD}: .golangci.yml does not exist`)
    else
      for (const failure of checkGolangciCaps(text).failures) {
        failures.push(`${GOLANGCI_GUARD}: ${failure}`)
      }
  }

  // 8. Unregistered gates, by filename pattern and by marker.
  failures.push(...unregisteredGates({ repoRoot, records, tracked, trackedSet }))

  return { failures, guards: records.length, filesScanned: allScanned.size }
}

function unregisteredGates({ repoRoot, records, tracked, trackedSet }) {
  const failures = []
  const named = new Set(records.flatMap((record) => record.command.split(/\s+/)))
  const proofs = new Set(records.map((record) => record.proof.file))
  for (const file of tracked) {
    if (GATE_TEST.test(file)) {
      const stem = file.replace(/\.test\.mjs$/, '')
      if (trackedSet.has(`${stem}.mjs`) || trackedSet.has(`${stem}.sh`)) continue
      if (!proofs.has(file)) failures.push(`unregistered gate: ${file}`)
    } else if (GATE_SCRIPT.test(file)) {
      const test = file.replace(/\.(?:mjs|sh)$/, '.test.mjs')
      if (!named.has(file) && !proofs.has(test)) failures.push(`unregistered gate: ${file}`)
    }
  }

  const markers = guardMarkers(repoRoot)
  const ids = new Set(records.map((record) => record.id))
  for (const marker of markers) {
    if (!ids.has(marker.id)) {
      failures.push(
        `unregistered gate: ${marker.file}:${marker.line} carries guard-id: ${marker.id}, which no record registers`,
      )
    }
  }

  const markedFiles = new Map()
  for (const marker of markers) {
    if (!markedFiles.has(marker.id)) markedFiles.set(marker.id, new Set())
    markedFiles.get(marker.id).add(marker.file)
  }
  for (const record of records) {
    const files = [
      ...record.command.split(/\s+/).filter((token) => trackedSet.has(token)),
      record.proof.file,
    ]
    if (files.some((file) => GATE_SCRIPT.test(file) || GATE_TEST.test(file))) continue
    const own = new Set([...files, ...scannedFiles(repoRoot, record.scans, { tracked })])
    if (![...(markedFiles.get(record.id) ?? [])].some((file) => own.has(file))) {
      failures.push(
        `${record.id}: no filename pattern finds this gate, and no file it names (command, proof.file, scans) carries a guard-id: ${record.id} comment`,
      )
    }
  }
  return failures
}

if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, allowPositionals: false })
  const { failures, guards, filesScanned } = await checkRegistry()
  if (failures.length > 0) {
    console.error('Guard registry check failed:')
    for (const failure of failures) console.error(`  ${failure}`)
    process.exit(1)
  }
  console.log(`Guard registry OK (${guards} guards, ${filesScanned} files scanned)`)
}
