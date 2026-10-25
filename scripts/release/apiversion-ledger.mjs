#!/usr/bin/env node

// apiversion-ledger.mjs — reconcile this branch's API-version contracts against the base branch
// and the production branch (BOS-1364).
//
// The API-version contract (lib/bossalib/apiversion) depends on two trees besides the branch:
// origin/production decides which versions are actually served, and origin/main keeps moving
// while the branch is open. This helper reads their non-test Go package files and answers:
//
//   - target: which version should a new behavioral change attach to right now — reuse the open
//     window, or mint the next date (recording the releases the ledger has not caught up with)?
//   - findings: did a rebase or conflict resolution drop a version, a ledger entry or a contract,
//     re-date a shipped contract, attach new behavior to a served version, or fork the open window?
//
// It is read-only apart from the optional `git fetch`, uses Node built-ins only, and fails closed:
// anything it cannot parse makes the verdict `unevaluated` (exit 2), never `clean`.
//
// Usage: node scripts/release/apiversion-ledger.mjs check [--base origin/main]
//          [--production origin/production] [--fetch] [--json]
//
// The verdict table lives in this file and its fixtures (apiversion-ledger.test.mjs); the docs
// point here rather than restating it.

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

export const APIVERSION_DIR = 'lib/bossalib/apiversion'
export const FILES = {
  version: `${APIVERSION_DIR}/version.go`,
  transform: `${APIVERSION_DIR}/transform.go`,
  released: `${APIVERSION_DIR}/released.go`,
}

export const EXIT = { clean: 0, conflict: 1, unevaluated: 2 }

/** Thrown for anything that must make the verdict `unevaluated`. */
export class Unevaluated extends Error {}

const DATE = /^\d{4}-\d{2}-\d{2}$/

function stripLineComments(text) {
  return text.replace(/\/\/[^\n]*/g, '')
}

/** Return the body of `func <name>(` up to the next top-level `\nfunc ` (or EOF). */
function funcBody(text, name) {
  const re = new RegExp(`\\nfunc\\s+${name}\\s*\\(`)
  const m = re.exec(text)
  if (!m) return null
  const rest = text.slice(m.index + 1)
  const next = rest.search(/\nfunc\s/)
  return next === -1 ? rest : rest.slice(0, next)
}

/** Parse `const <Name> Version = "<date>"` declarations into Map<name, date>. */
export function parseConsts(versionGo) {
  const consts = new Map()
  for (const m of versionGo.matchAll(/^\s*(?:const\s+)?(\w+)\s+Version\s*=\s*"([^"]+)"/gm)) {
    if (DATE.test(m[2])) consts.set(m[1], m[2])
  }
  return consts
}

function resolveConst(consts, ident, where) {
  const v = consts.get(ident)
  if (!v) throw new Unevaluated(`${where}: identifier ${ident} is not a known Version const`)
  return v
}

/** Parse DefaultRegistry's version slice and Current argument. */
export function parseRegistry(versionGo, consts) {
  const body = funcBody(versionGo, 'DefaultRegistry')
  if (!body) throw new Unevaluated('version.go: func DefaultRegistry not found')
  const code = stripLineComments(body)
  const m = /\[\]Version\s*\{([^}]*)\}\s*,\s*(\w+)/.exec(code)
  if (!m) throw new Unevaluated('version.go: no []Version{...} slice inside DefaultRegistry')
  const idents = m[1]
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  if (idents.length === 0) throw new Unevaluated('version.go: DefaultRegistry slice is empty')
  for (const id of idents) {
    if (!/^\w+$/.test(id)) {
      throw new Unevaluated(
        `version.go: DefaultRegistry slice member ${JSON.stringify(id)} is not an identifier`,
      )
    }
  }
  const registry = idents.map((id) => resolveConst(consts, id, 'version.go DefaultRegistry'))
  const current = resolveConst(consts, m[2], 'version.go DefaultRegistry Current')
  if (!registry.includes(current)) {
    throw new Unevaluated(
      `version.go: Current ${current} is not a member of the DefaultRegistry slice`,
    )
  }
  return { registry, current }
}

/** Parse the date literals of `ReleasedVersions = []Version{...}`. */
export function parseReleased(releasedGo) {
  const m = /ReleasedVersions\s*=\s*\[\]Version\s*\{([^}]*)\}/.exec(releasedGo)
  if (!m) throw new Unevaluated('released.go: ReleasedVersions = []Version{...} not found')
  const released = [...stripLineComments(m[1]).matchAll(/"([^"]+)"/g)].map((x) => x[1])
  if (released.length === 0) throw new Unevaluated('released.go: ReleasedVersions is empty')
  for (const v of released) {
    if (!DATE.test(v))
      throw new Unevaluated(`released.go: ${JSON.stringify(v)} is not a YYYY-MM-DD literal`)
  }
  return released
}

/**
 * Parse every contract: ProductionChanges transforms (resolved through their Version() methods)
 * plus every handler-level gateAtLeast(ctx, <Const>, "<Name>") in version.go.
 * Returns Map<name, date>.
 */
export function parseContracts(versionGo, transformGo, consts, companions = new Map()) {
  const contracts = new Map()
  const body = funcBody(transformGo, 'ProductionChanges')
  if (!body) throw new Unevaluated('transform.go: func ProductionChanges not found')
  const call = /NewChanges\(\s*DefaultRegistry\(\)\s*(?:,([^)]*))?\)/.exec(stripLineComments(body))
  if (!call) {
    throw new Unevaluated(
      'transform.go: no NewChanges(DefaultRegistry(), ...) call inside ProductionChanges',
    )
  }
  const args = (call[1] ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  if (args.length === 0)
    throw new Unevaluated('transform.go: ProductionChanges registers no transforms')
  const methods = parseMethods(new Map([['transform.go', transformGo], ...companions]))
  for (const arg of args) {
    const lit = /^&?(\w+)\s*\{\s*\}$/.exec(arg)
    if (!lit) {
      throw new Unevaluated(
        `transform.go: ProductionChanges argument ${JSON.stringify(arg)} is not a <Name>{} literal`,
      )
    }
    const name = lit[1]
    const method = methods.get(name)
    if (!method)
      throw new Unevaluated(
        `apiversion package: ${name} has no resolvable Version() method (searched transform.go and ${companions.size} companion files)`,
      )
    contracts.set(name, resolveConst(consts, method.constName, `${method.file} ${name}.Version()`))
  }
  for (const m of versionGo.matchAll(/gateAtLeast\(\s*\w+\s*,\s*(\w+)\s*,\s*"(\w+)"\s*\)/g)) {
    contracts.set(m[2], resolveConst(consts, m[1], `version.go gate ${m[2]}`))
  }
  return contracts
}

function parseMethods(sources) {
  const methods = new Map()
  for (const [file, text] of sources) {
    for (const m of text.matchAll(
      /func\s*\(\s*(?:\w+\s+)?\*?(\w+)\s*\)\s*Version\(\)\s*Version\s*\{\s*return\s+(\w+)\s*\}/g,
    )) {
      const previous = methods.get(m[1])
      if (previous)
        throw new Unevaluated(
          `${m[1]} has duplicate Version() methods in ${previous.file} and ${file}`,
        )
      methods.set(m[1], { constName: m[2], file })
    }
  }
  return methods
}

/**
 * Parse one tree given its anchored files and all companion non-test Go package files.
 *
 * @param {{version?: string, transform?: string, released?: string, companions?: Map<string, string>}} files
 * @param {string} [label]
 */
export function parseTree(
  { version, transform, released, companions = new Map() },
  label = 'tree',
) {
  try {
    for (const [file, text] of [
      ...Object.entries({
        'version.go': version,
        'transform.go': transform,
        'released.go': released,
      }),
      ...companions,
    ]) {
      if (text == null) throw new Unevaluated(`${file} is missing`)
      if (/^(<{7}|>{7}|={7})( |$)/m.test(text))
        throw new Unevaluated(`${file} carries conflict markers`)
    }
    // Gates are discovered only in version.go; one elsewhere would silently read as clean.
    for (const [file, text] of [
      ...Object.entries({ 'transform.go': transform, 'released.go': released }),
      ...companions,
    ]) {
      if (/\bgateAtLeast\s*\(/.test(stripLineComments(text)))
        throw new Unevaluated(`${file}: gateAtLeast call outside version.go is not scanned`)
    }
    const consts = parseConsts(version)
    if (consts.size === 0) throw new Unevaluated('version.go: zero Version consts')
    const { registry, current } = parseRegistry(version, consts)
    return {
      consts,
      registry,
      current,
      released: parseReleased(released),
      contracts: parseContracts(version, transform, consts, companions),
    }
  } catch (err) {
    if (err instanceof Unevaluated) throw new Unevaluated(`${label}: ${err.message}`)
    throw err
  }
}

function addDay(date) {
  const d = new Date(`${date}T00:00:00Z`)
  d.setUTCDate(d.getUTCDate() + 1)
  return d.toISOString().slice(0, 10)
}

const sorted = (xs) => [...xs].sort()

function resolveTarget(base, production, mb, served) {
  if (!served.has(base.current)) {
    return { action: 'reuse', version: base.current }
  } else {
    const used = new Set([
      ...base.consts.values(),
      ...production.consts.values(),
      ...mb.consts.values(),
    ])
    let next = addDay(base.current)
    while (used.has(next)) next = addDay(next)
    const baseReleased = new Set(base.released)
    return {
      action: 'mint',
      version: next,
      recordReleased: sorted([...served].filter((v) => !baseReleased.has(v))),
    }
  }
}

function deriveLedgerContext({ head, base, production, mb, rebased }) {
  const baseReg = new Set(base.registry)
  const prodAhead = production.registry.filter((v) => !baseReg.has(v))
  const served = new Set([...base.released, ...production.registry.filter((v) => baseReg.has(v))])
  const target = resolveTarget(base, production, mb, served)
  // dropped-*: compare head against mb, plus base once the branch has been rebased onto it.
  const refRegistry = new Set([...mb.registry, ...(rebased ? base.registry : [])])
  const refReleased = new Set([...mb.released, ...(rebased ? base.released : [])])
  const refContracts = new Map([...mb.contracts, ...(rebased ? base.contracts : [])])
  const headReg = new Set(head.registry)
  const headReleased = new Set(head.released)

  const newOnHead = [...head.contracts]
    .filter(([name]) => !mb.contracts.has(name) && !base.contracts.has(name))
    .sort()
  const minted = head.registry.filter((v) => !baseReg.has(v))
  const unrecorded = sorted([...served].filter((v) => !headReleased.has(v)))
  return {
    head,
    base,
    production,
    mb,
    rebased,
    baseReg,
    prodAhead,
    served,
    target,
    refRegistry,
    refReleased,
    refContracts,
    headReg,
    headReleased,
    newOnHead,
    minted,
    unrecorded,
  }
}

function finding(level, code, subject, action, version) {
  return { level, code, subject, ...(version ? { version } : {}), action }
}

function productionAhead({ prodAhead }) {
  const findings = []
  for (const v of prodAhead) {
    findings.push(
      finding(
        'info',
        'production-ahead',
        v,
        'production registers a version base does not; it is left out of the served set',
        v,
      ),
    )
  }

  return findings
}

function droppedVersions({ refRegistry, headReg }) {
  const findings = []
  for (const v of sorted(refRegistry)) {
    if (!headReg.has(v)) {
      findings.push(
        finding(
          'error',
          'dropped-version',
          v,
          'restore it; registries are append-only and a conflict resolution must union both sides',
          v,
        ),
      )
    }
  }
  return findings
}

function droppedReleased({ refReleased, headReleased }) {
  const findings = []
  for (const v of sorted(refReleased)) {
    if (!headReleased.has(v))
      findings.push(finding('error', 'dropped-released', v, 'restore the ledger entry', v))
  }
  return findings
}

function droppedContracts({ refContracts, head }) {
  const findings = []
  for (const [name, v] of [...refContracts].sort()) {
    if (!head.contracts.has(name)) {
      findings.push(
        finding(
          'error',
          'dropped-contract',
          name,
          "union both sides' ProductionChanges / gates; never take one side",
          v,
        ),
      )
    }
  }
  return findings
}

function retargetedContracts({ head, base, mb }) {
  const findings = []
  for (const [name, v] of [...head.contracts].sort()) {
    const shipped = base.contracts.get(name) ?? mb.contracts.get(name)
    if (shipped && shipped !== v) {
      findings.push(
        finding(
          'error',
          'retargeted-contract',
          name,
          `restore base's version ${shipped}; a shipped contract is never re-dated`,
          v,
        ),
      )
    }
  }

  return findings
}

function releasedTarget({ newOnHead, served, target }) {
  const findings = []
  for (const [name, v] of newOnHead) {
    if (served.has(v)) {
      const record = target.recordReleased?.length
        ? ` and record ${target.recordReleased.join(', ')} in ReleasedVersions`
        : ''
      findings.push(
        finding(
          'error',
          'released-target',
          name,
          `re-target to ${target.version} (${target.action}${record}); ${v} is already served`,
          v,
        ),
      )
    }
  }

  return findings
}

function unreleasedFork({ served, base, minted }) {
  const findings = []
  if (!served.has(base.current)) {
    for (const v of minted) {
      if (v !== base.current) {
        findings.push(
          finding(
            'error',
            'unreleased-fork',
            v,
            `fold your contracts into ${base.current} (the open window) and delete your const`,
            v,
          ),
        )
      }
    }
  }
  return findings
}

function unrecordedRelease({ unrecorded, minted }) {
  const findings = []
  if (unrecorded.length && minted.length) {
    findings.push(
      finding(
        'error',
        'unrecorded-release',
        unrecorded.join(', '),
        `append ${unrecorded.map((v) => JSON.stringify(v)).join(', ')} to ReleasedVersions`,
      ),
    )
  } else if (unrecorded.length) {
    findings.push(
      finding(
        'info',
        'ledger-lag',
        unrecorded.join(', '),
        'informational: production serves these but ReleasedVersions does not record them yet',
      ),
    )
  }

  return findings
}

function rebaseNeeded({ rebased, base, mb, newOnHead }) {
  const findings = []
  if (!rebased) {
    const baseNew = [...base.contracts].filter(([name]) => !mb.contracts.has(name)).sort()
    if (baseNew.length) {
      const headNewVersions = new Set(newOnHead.map(([, v]) => v))
      const sameWindow = baseNew
        .filter(([, v]) => headNewVersions.has(v))
        .map(([n, v]) => `${n}@${v}`)
      const list = baseNew.map(([n, v]) => `${n}@${v}`).join(', ')
      const sw = sameWindow.length
        ? `; same-window: ${sameWindow.join(', ')} — keep both sides`
        : ''
      findings.push(finding('warning', 'rebase-needed', list, `rebase onto base, then re-run${sw}`))
    }
  }

  return findings
}

const LEDGER_RULES = Object.freeze([
  productionAhead,
  droppedVersions,
  droppedReleased,
  droppedContracts,
  retargetedContracts,
  releasedTarget,
  unreleasedFork,
  unrecordedRelease,
  rebaseNeeded,
])

/**
 * Pure evaluation over parsed trees. `mb` is the merge-base tree; `rebased` is whether base is an
 * ancestor of HEAD. Returns {verdict, target, findings}.
 */
export function evaluate(input) {
  const ctx = deriveLedgerContext(input)
  const findings = LEDGER_RULES.flatMap((rule) => rule(ctx))
  const verdict = findings.some((f) => f.level === 'error') ? 'conflict' : 'clean'
  return { verdict, target: ctx.target, findings }
}

// ---- git-backed tree loading ------------------------------------------------------------------

function git(cwd, args, opts = {}) {
  return execFileSync('git', args, {
    cwd,
    encoding: 'utf8',
    stdio: ['ignore', 'pipe', 'pipe'],
    ...opts,
  })
}

function tryGit(cwd, args) {
  try {
    return { ok: true, out: git(cwd, args) }
  } catch (err) {
    return { ok: false, status: err.status, err: String(err.stderr || err.message).trim() }
  }
}

function isCompanion(rel) {
  return rel.endsWith('.go') && !rel.endsWith('_test.go') && !Object.values(FILES).includes(rel)
}

function readRefCompanions(root, ref, label) {
  const listing = tryGit(root, ['ls-tree', '-z', `${ref}:${APIVERSION_DIR}`])
  if (!listing.ok)
    throw new Unevaluated(`${label} (${ref}): cannot read ${APIVERSION_DIR}: ${listing.err}`)
  const companions = new Map()
  for (const entry of listing.out.split('\0').filter(Boolean).sort()) {
    const [metadata, name] = entry.split('\t')
    const rel = `${APIVERSION_DIR}/${name}`
    if (metadata.split(' ')[1] !== 'blob' || !isCompanion(rel)) continue
    const r = tryGit(root, ['show', `${ref}:${rel}`])
    if (!r.ok) throw new Unevaluated(`${label} (${ref}): cannot read ${rel}: ${r.err}`)
    companions.set(rel, r.out)
  }
  return companions
}

function readWorkCompanions(root) {
  const companions = new Map()
  try {
    const entries = fs.readdirSync(path.join(root, APIVERSION_DIR), { withFileTypes: true })
    for (const entry of entries.sort((a, b) => a.name.localeCompare(b.name))) {
      const rel = `${APIVERSION_DIR}/${entry.name}`
      if (entry.isFile() && isCompanion(rel))
        companions.set(rel, fs.readFileSync(path.join(root, rel), 'utf8'))
    }
  } catch (err) {
    throw new Unevaluated(`head (working tree): cannot read ${APIVERSION_DIR}: ${err.message}`)
  }
  return companions
}

function readRefTree(root, ref, label) {
  const files = {}
  for (const [key, rel] of Object.entries(FILES)) {
    const r = tryGit(root, ['show', `${ref}:${rel}`])
    if (!r.ok) throw new Unevaluated(`${label} (${ref}): cannot read ${rel}: ${r.err}`)
    files[key] = r.out
  }
  return parseTree(
    { ...files, companions: readRefCompanions(root, ref, label) },
    `${label} (${ref})`,
  )
}

function readWorkTree(root) {
  const files = {}
  for (const [key, rel] of Object.entries(FILES)) {
    const p = path.join(root, rel)
    if (!fs.existsSync(p)) throw new Unevaluated(`head (working tree): ${rel} is missing`)
    files[key] = fs.readFileSync(p, 'utf8')
  }
  return parseTree({ ...files, companions: readWorkCompanions(root) }, 'head (working tree)')
}

function fetchRefs(root, refs) {
  const remotes = new Set(
    tryGit(root, ['remote']).ok
      ? git(root, ['remote'])
          .split('\n')
          .map((s) => s.trim())
          .filter(Boolean)
      : [],
  )
  const byRemote = new Map()
  for (const ref of refs) {
    const slash = ref.indexOf('/')
    const remote = slash > 0 ? ref.slice(0, slash) : ''
    if (!remotes.has(remote))
      throw new Unevaluated(`--fetch: ${ref} is not a <remote>/<branch> ref of a configured remote`)
    if (!byRemote.has(remote)) byRemote.set(remote, [])
    byRemote.get(remote).push(ref.slice(slash + 1))
  }
  for (const [remote, branches] of byRemote) {
    const r = tryGit(root, ['fetch', '--quiet', remote, ...branches])
    if (!r.ok)
      throw new Unevaluated(`--fetch: git fetch ${remote} ${branches.join(' ')} failed: ${r.err}`)
  }
}

/** Run the full check from `cwd`. Always resolves to a result object; never throws Unevaluated. */
export function check({
  cwd = process.cwd(),
  base = 'origin/main',
  production = 'origin/production',
  fetch = false,
} = {}) {
  const refs = { base, production, mergeBase: null, rebased: null }
  try {
    const top = tryGit(cwd, ['rev-parse', '--show-toplevel'])
    if (!top.ok) throw new Unevaluated(`not inside a git repository: ${top.err}`)
    const root = top.out.trim()
    if (fetch) fetchRefs(root, [base, production])
    for (const ref of [base, production]) {
      const r = tryGit(root, ['rev-parse', '--verify', '--quiet', `${ref}^{commit}`])
      if (!r.ok) throw new Unevaluated(`ref ${ref} does not resolve to a commit`)
    }
    const mbr = tryGit(root, ['merge-base', 'HEAD', base])
    if (!mbr.ok) throw new Unevaluated(`git merge-base HEAD ${base} failed: ${mbr.err}`)
    refs.mergeBase = mbr.out.trim()
    const anc = tryGit(root, ['merge-base', '--is-ancestor', base, 'HEAD'])
    if (!anc.ok && anc.status !== 1)
      throw new Unevaluated(`git merge-base --is-ancestor failed: ${anc.err}`)
    refs.rebased = anc.ok
    const trees = {
      head: readWorkTree(root),
      base: readRefTree(root, base, 'base'),
      production: readRefTree(root, production, 'production'),
      mb: readRefTree(root, refs.mergeBase, 'merge-base'),
      rebased: refs.rebased,
    }
    return { ...evaluate(trees), refs }
  } catch (err) {
    if (!(err instanceof Unevaluated)) throw err
    return { verdict: 'unevaluated', reason: err.message, target: null, findings: [], refs }
  }
}

export function formatHuman(result) {
  const lines = []
  if (result.verdict === 'unevaluated') {
    lines.push(`apiversion-ledger: UNEVALUATED — ${result.reason}`)
    lines.push('No target was computed; an unevaluated check is unknown, never a pass.')
    return lines.join('\n')
  }
  const t = result.target
  const rec = t.recordReleased?.length
    ? `; record ${t.recordReleased.join(', ')} in ReleasedVersions`
    : ''
  lines.push(`target: ${t.action} ${t.version}${rec}`)
  for (const f of result.findings) {
    lines.push(
      `${f.level} ${f.code}: ${f.subject}${f.version && f.version !== f.subject ? ` @ ${f.version}` : ''} — ${f.action}`,
    )
  }
  const r = result.refs
  lines.push(
    `verdict: ${result.verdict} (base ${r.base}, production ${r.production}, merge-base ${r.mergeBase.slice(0, 12)}, rebased ${r.rebased})`,
  )
  return lines.join('\n')
}

function parseArgs(argv) {
  const [cmd, ...rest] = argv
  if (cmd !== 'check')
    throw new Error(
      `usage: apiversion-ledger.mjs check [--base REF] [--production REF] [--fetch] [--json]`,
    )
  const opts = { base: 'origin/main', production: 'origin/production', fetch: false, json: false }
  for (let i = 0; i < rest.length; i++) {
    const a = rest[i]
    if (a === '--fetch') opts.fetch = true
    else if (a === '--json') opts.json = true
    else if (a === '--base' || a === '--production') {
      const v = rest[++i]
      if (!v) throw new Error(`${a} needs a value`)
      opts[a.slice(2)] = v
    } else throw new Error(`unknown argument ${a}`)
  }
  return opts
}

function main() {
  let opts
  try {
    opts = parseArgs(process.argv.slice(2))
  } catch (err) {
    process.stderr.write(`${err.message}\n`)
    return EXIT.unevaluated
  }
  const result = check(opts)
  process.stdout.write(`${opts.json ? JSON.stringify(result, null, 2) : formatHuman(result)}\n`)
  return EXIT[result.verdict]
}

if (isMainModule(import.meta.url)) {
  process.exitCode = main()
}
