#!/usr/bin/env node
// Supply-chain gate for GitHub Actions configuration.
//
// Fails when, under .github/:
//   - a non-local `uses:` (workflow step, reusable-workflow job, or composite-action step) is not
//     pinned to a 40-hex commit SHA (`docker://` images need an `@sha256:` digest). A tag can be
//     moved; a commit cannot. Local `./...` references are exempt.
//   - a workflow has no top-level `permissions:` block, or that block grants any `write` scope.
//     Write grants belong on the job that needs them (TOP_LEVEL_WRITE_ALLOWLIST holds the
//     exceptions, each with its reason).
//   - a job that holds a write grant runs a package manager (pnpm/npm/npx/yarn) after an
//     `actions/checkout` that persists its credentials: install scripts and any other
//     node_modules code could read the token from .git/config. Such checkouts need
//     `persist-credentials: false`.
//   - .github/dependabot.yml does not track the `github-actions` ecosystem, so pinned SHAs
//     would never receive bump PRs.
//
// The YAML here is read line by line rather than parsed (scripts/workflow-yaml-lib.mjs): the repo
// carries no YAML dependency for scripts, and the shapes checked are keys at known positions.
// Block scalars (`run: |`) and comments are skipped, so shell text that mentions `uses:` is never
// an action reference.

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import {
  classifyLines,
  decodeEscapes,
  flowPairs,
  keyColumn,
  keyValue,
  listWorkflowFiles,
  nestedUnder,
  structuralLines,
} from './workflow-yaml-lib.mjs'

const REPO_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

export const ACTIONS_DIR = '.github/actions'
export const DEPENDABOT_FILE = '.github/dependabot.yml'

// Repo-relative workflow path -> reason it cannot move its write grant down to a job.
export const TOP_LEVEL_WRITE_ALLOWLIST = new Map()
const TOP_LEVEL_WRITE = 'top-level-write'

const COMMIT_SHA = /^[0-9a-f]{40}$/
const DOCKER_DIGEST = /@sha256:[0-9a-f]{64}$/
// A JS package manager runs third-party code from node_modules: install scripts, `pnpm exec`,
// `run` scripts, `npx`/`dlx` downloads. So a write-granted job that invokes one at all must not
// persist its checkout credentials. Reading shell well enough to tell an install from any other
// invocation failed open every time it was tried (substitutions, redirections, quoting, folded
// scalars), so the rule is a word match: any manager name the job's text carries outside
// comments counts, and a stray hit costs only a `persist-credentials: false`.
const MANAGER_NAMES = ['pnpm', 'npm', 'npx', 'pnpx', 'yarn', 'yarnpkg', 'corepack'].join('|')
const PACKAGE_MANAGER = new RegExp(`(?<![\\w-])(?:${MANAGER_NAMES})(?![\\w-])`)
// An action named for a manager can run it (`pnpm/action-setup` with `run_install`,
// `bahmutov/npm-install`, `borales/actions-yarn`), so a name component of the ref counts too.
const PACKAGE_MANAGER_ACTION = new RegExp(`(?:^|[/._-])(?:${MANAGER_NAMES})(?:$|[/@._-])`)

// Readings of a line that may run a manager. YAML decodes a double-quoted scalar's escapes
// first and the shell then joins quoted fragments and drops backslashes, so each of the line as
// written and the line with its escapes decoded is read both as is (`exec"npm", "ci"` in an
// inline script) and with quotes, backslashes and NUL bytes removed (`np\m`, `np''m`,
// `"n\x27\x27pm"`, `"cd web\nnpm ci"` all run npm, and bash drops a NUL inside a script).
function shellReadings(text) {
  const decoded = decodeEscapes(text)
  const shell = (reading) => reading.replace(/['"\\\0]/g, '')
  return [text, shell(text), decoded, shell(decoded)]
}

function namesPackageManager(text) {
  return shellReadings(text).some((reading) => PACKAGE_MANAGER.test(reading))
}

// Whether a job's raw YAML lines name a package manager, or use an action named for one,
// outside YAML comments and whole-line shell comments.
export function runsPackageManager(rawLines) {
  return classifyLines(rawLines.join('\n')).some((entry) => {
    if (entry.kind === 'scalar') {
      return !entry.raw.trimStart().startsWith('#') && namesPackageManager(entry.raw)
    }
    if (entry.kind !== 'structural') return false
    const ref = usesRef(entry.text)
    return namesPackageManager(entry.text) || (ref !== null && PACKAGE_MANAGER_ACTION.test(ref))
  })
}

const CHECKOUT = /^actions\/checkout@/

// The `uses:` value of a step line, in block (`- uses: x`) or flow (`- { uses: x }`) form.
function usesRef(text) {
  const kv = keyValue(text)
  if (kv && kv.key === 'uses') return kv.value
  const pair = flowPairs(text)?.find((entry) => entry.key === 'uses')
  return pair ? pair.value : null
}

export function usesProblem(ref) {
  if (ref.startsWith('./')) return null
  if (ref.startsWith('docker://')) {
    return DOCKER_DIGEST.test(ref) ? null : `\`${ref}\` is not pinned to an @sha256: digest`
  }
  const at = ref.lastIndexOf('@')
  if (at < 0) return `\`${ref}\` has no @ref; pin it to a 40-hex commit SHA`
  return COMMIT_SHA.test(ref.slice(at + 1))
    ? null
    : `\`${ref}\` is not pinned to a 40-hex commit SHA (use owner/repo@<sha> # <tag>)`
}

function usesFindings(file, lines) {
  const findings = []
  for (const entry of lines) {
    const ref = usesRef(entry.text)
    if (ref === null) continue
    const problem = usesProblem(ref)
    if (problem) findings.push({ file, line: entry.line, message: problem })
  }
  return findings
}

// Write scopes granted by a `permissions:` key at index `start`.
function writeScopes(lines, start) {
  const inline = keyValue(lines[start].text).value
  if (inline !== '') {
    if (inline === 'write-all') return ['write-all']
    return [...inline.matchAll(/['"]?([A-Za-z-]+)['"]?\s*:\s*['"]?write\b/g)].map(
      (m) => `${m[1]}: write`,
    )
  }
  return nestedUnder(lines, start)
    .map((i) => keyValue(lines[i].text))
    .filter((kv) => kv && kv.value === 'write')
    .map((kv) => `${kv.key}: write`)
}

// Whether flow or block `with:` pairs set `persist-credentials: false`.
function dropsCredentials(pairs) {
  return pairs.some((pair) => pair.key === 'persist-credentials' && pair.value === 'false')
}

// Whether the checkout step whose `uses:` sits at `jobLines[at]` sets
// `with: persist-credentials: false`. Only the step's own `with:` mapping counts: the same key
// under `env:`, deeper in another mapping, or inside a step name sets nothing.
function checkoutDropsCredentials(jobLines, at) {
  const flowStep = flowPairs(jobLines[at].text)
  if (flowStep) {
    const withValue = flowStep.find((pair) => pair.key === 'with')?.value ?? ''
    return dropsCredentials(flowPairs(withValue) ?? [])
  }
  // The step's fields share one key column: the dash line's key (none for a bare `-` line),
  // then its siblings below.
  const column = keyColumn(jobLines[at])
  const opensStep = (entry) =>
    /^-\s/.test(entry.text)
      ? keyColumn(entry) === column
      : /^(?:-\s+)*-$/.test(entry.text) && entry.indent < column
  let itemStart = at
  while (itemStart >= 0) {
    const entry = jobLines[itemStart]
    if (opensStep(entry)) break
    if (entry.indent < column) return false
    itemStart--
  }
  if (itemStart < 0) return false
  const fields = [itemStart].concat(
    nestedUnder(jobLines, itemStart, jobLines[itemStart].indent).filter(
      (i) => jobLines[i].indent === column,
    ),
  )
  return fields.some((i) => {
    const field = keyValue(jobLines[i].text)
    if (!field || field.key !== 'with') return false
    if (field.value !== '') return dropsCredentials(flowPairs(field.value) ?? [])
    const nested = nestedUnder(jobLines, i)
    const childIndent = nested.length > 0 ? jobLines[nested[0]].indent : -1
    const children = nested
      .filter((j) => jobLines[j].indent === childIndent)
      .map((j) => keyValue(jobLines[j].text))
      .filter(Boolean)
    return dropsCredentials(children)
  })
}

function persistCredentialFindings(file, jobLines, jobRaw, grants) {
  if (grants.length === 0) return []
  if (!runsPackageManager(jobRaw)) return []
  const findings = []
  for (let i = 0; i < jobLines.length; i++) {
    const ref = usesRef(jobLines[i].text)
    if (ref === null || !CHECKOUT.test(ref) || checkoutDropsCredentials(jobLines, i)) continue
    findings.push({
      file,
      line: jobLines[i].line,
      message: `job holds ${grants.join(', ')} and runs a package manager, but this checkout persists its credentials; set \`persist-credentials: false\``,
    })
  }
  return findings
}

export function checkWorkflow(file, text, { allowlist = TOP_LEVEL_WRITE_ALLOWLIST } = {}) {
  const lines = structuralLines(text)
  const raws = text.split('\n')
  const findings = usesFindings(file, lines)

  const topPermissions = lines.findIndex(
    (l) => l.indent === 0 && keyValue(l.text)?.key === 'permissions',
  )
  let topGrants = []
  if (topPermissions < 0) {
    findings.push({
      file,
      line: 1,
      message: 'no top-level `permissions:` block; add `permissions: contents: read` (or `{}`)',
    })
  } else {
    topGrants = writeScopes(lines, topPermissions)
    if (topGrants.length > 0 && !allowlist.has(file)) {
      findings.push({
        file,
        line: lines[topPermissions].line,
        kind: TOP_LEVEL_WRITE,
        message: `top-level \`permissions:\` grants ${topGrants.join(', ')}; move write grants to the jobs that need them`,
      })
    }
  }

  const jobsAt = lines.findIndex((l) => {
    const kv = l.indent === 0 && keyValue(l.text)
    return kv && kv.key === 'jobs' && kv.value === ''
  })
  if (jobsAt < 0) return findings
  const jobBlock = nestedUnder(lines, jobsAt).map((i) => lines[i])
  if (jobBlock.length === 0) return findings
  const jobIndent = jobBlock[0].indent
  const jobStarts = jobBlock.flatMap((l, i) => (l.indent === jobIndent ? [i] : []))
  for (const [n, start] of jobStarts.entries()) {
    const end = n + 1 < jobStarts.length ? jobStarts[n + 1] : jobBlock.length
    const jobLines = jobBlock.slice(start, end)
    const lastLine = end < jobBlock.length ? jobBlock[end].line - 1 : raws.length
    const jobRaw = raws.slice(jobLines[0].line - 1, lastLine)
    const body = jobLines.slice(1)
    const fieldIndent = body.length > 0 ? body[0].indent : -1
    const permissionsAt = body.findIndex(
      (l) => l.indent === fieldIndent && keyValue(l.text)?.key === 'permissions',
    )
    const grants = permissionsAt >= 0 ? writeScopes(body, permissionsAt) : topGrants
    findings.push(...persistCredentialFindings(file, body, jobRaw, grants))
  }
  return findings
}

export function checkActionFile(file, text) {
  return usesFindings(file, structuralLines(text))
}

export function checkDependabot(file, text) {
  const tracked =
    text !== null &&
    structuralLines(text).some((l) => {
      const kv = keyValue(l.text)
      return kv && kv.key === 'package-ecosystem' && kv.value === 'github-actions'
    })
  return tracked
    ? []
    : [
        {
          file,
          line: 1,
          message:
            'dependabot does not track `package-ecosystem: github-actions`; pinned action SHAs would never be bumped',
        },
      ]
}

function listFiles(dir, match) {
  if (!fs.existsSync(dir)) return []
  const out = []
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name)
    if (entry.isDirectory()) out.push(...listFiles(full, match))
    else if (match(entry.name)) out.push(full)
  }
  return out.sort()
}

export function checkTree(root = REPO_ROOT, { allowlist = TOP_LEVEL_WRITE_ALLOWLIST } = {}) {
  const rel = (full) => path.relative(root, full).split(path.sep).join('/')
  const workflowFiles = listWorkflowFiles(root).map((file) => path.join(root, ...file.split('/')))
  const actionFiles = listFiles(path.join(root, ACTIONS_DIR), (name) =>
    /^action\.ya?ml$/.test(name),
  )

  const findings = []
  for (const full of workflowFiles) {
    findings.push(...checkWorkflow(rel(full), fs.readFileSync(full, 'utf8'), { allowlist }))
  }
  for (const full of actionFiles) {
    findings.push(...checkActionFile(rel(full), fs.readFileSync(full, 'utf8')))
  }
  for (const [file, reason] of allowlist) {
    const full = path.join(root, file)
    const stale =
      !fs.existsSync(full) ||
      !checkWorkflow(file, fs.readFileSync(full, 'utf8'), { allowlist: new Map() }).some(
        (f) => f.kind === TOP_LEVEL_WRITE,
      )
    if (stale) {
      findings.push({
        file,
        line: 1,
        message: `allowlisted (${reason}) but no longer has a top-level write grant; remove it from TOP_LEVEL_WRITE_ALLOWLIST`,
      })
    }
  }
  const dependabot = path.join(root, DEPENDABOT_FILE)
  findings.push(
    ...checkDependabot(
      DEPENDABOT_FILE,
      fs.existsSync(dependabot) ? fs.readFileSync(dependabot, 'utf8') : null,
    ),
  )
  return { findings, workflows: workflowFiles.length, actions: actionFiles.length }
}

const USAGE = 'usage: check-action-pins.mjs [--root <repo-root>]'

function main() {
  let values
  try {
    ;({ values } = parseArgs({
      options: { root: { type: 'string' }, help: { type: 'boolean', short: 'h' } },
    }))
  } catch (error) {
    console.error(`check-action-pins: ${error.message}`)
    console.error(USAGE)
    process.exit(2)
  }
  if (values.help) {
    console.log(USAGE)
    return
  }
  const root = path.resolve(values.root ?? REPO_ROOT)
  const { findings, workflows, actions } = checkTree(root)
  if (findings.length > 0) {
    console.error('GitHub Actions supply-chain check failed:')
    for (const f of findings) console.error(`  ${f.file}:${f.line}: ${f.message}`)
    process.exit(1)
  }
  console.log(
    `Action pins OK (${workflows} workflows, ${actions} composite actions: every action pinned by commit SHA, no top-level write grant, dependabot tracks github-actions)`,
  )
}

if (isMainModule(import.meta.url)) main()
