import { createHash } from 'node:crypto'
import { spawnSync, execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'
import {
  computeTreeHash,
  expansionEnv,
  forcesUncached,
  resolveBaseCommit,
} from './gate-stamp-lib.mjs'

export function writeFileAtomic(file, content) {
  const tmp = `${file}.${process.pid}.tmp`
  fs.writeFileSync(tmp, content)
  fs.renameSync(tmp, file)
}

/** Parse the Files section emitted by GNU Make 3.81 and 4.x. */
export function parseMakeDatabase(text) {
  const rules = new Map()
  let rule = null
  let notTarget = false
  let inFiles = false
  for (const line of text.split('\n')) {
    if (line === '# Files') inFiles = true
    if (!inFiles) continue
    if (line === '# Not a target:') {
      notTarget = true
      rule = null
      continue
    }
    if (line.startsWith('\t')) {
      if (rule && line.trim()) rule.recipe.push(line.trim())
      continue
    }
    if (!line || line.startsWith('#')) continue
    const match = line.match(/^([^\s:#=]+):([^=].*|)$/)
    if (!match) {
      rule = null
      continue
    }
    rule = notTarget
      ? null
      : { prerequisites: match[2].trim().split(/\s+/).filter(Boolean), recipe: [] }
    notTarget = false
    if (rule) rules.set(match[1], rule)
  }
  return rules
}

function makeInvocation(argv) {
  let at = 0
  if (argv[0] === 'env') {
    at = 1
    while (/^[A-Za-z_][A-Za-z_0-9]*=/.test(argv[at] ?? '')) at++
  }
  const make = argv[at]
  if (!make || !['make', 'gmake'].includes(path.basename(make))) return null
  const args = argv.slice(at + 1)
  // These change the rule source/directory, or inject rules. Exact-match identity remains safe.
  if (
    args.some((arg) =>
      /^-[Bdeiknpqrstvw]*[CfE]|^--(?:directory|file|makefile|eval)(?:=|$)/.test(arg),
    )
  )
    return null
  const prefix =
    argv[0] === 'env'
      ? ['env', ...argv.slice(1, at).sort((a, b) => a.split('=')[0].localeCompare(b.split('=')[0]))]
      : []
  return { make, args, prefix, goals: makeGoalIndices(args) }
}

function makeGoalIndices(args) {
  const goals = new Set()
  let value = false
  for (const [index, arg] of args.entries()) {
    if (value) {
      value = false
      continue
    }
    if (
      /^-[^-]*[IWo]$|^--(?:include-dir|what-if|new-file|assume-new|old-file|assume-old)$/.test(arg)
    ) {
      value = true
    } else if (/^-[^-]*[jl]$|^--(?:jobs|load-average|max-load)$/.test(arg)) {
      value = /^\d+(?:\.\d+)?$/.test(args[index + 1] ?? '')
    } else if (!arg.startsWith('-') && !arg.includes('=')) goals.add(index)
  }
  return goals
}

export function readMakeDatabase(argv, { cwd = process.cwd(), env = process.env } = {}) {
  const invocation = makeInvocation(argv)
  if (!invocation) return null
  const probeArgs = invocation.args.filter((_, index) => !invocation.goals.has(index))
  const makeEnv = { ...env }
  for (const assignment of invocation.prefix.slice(1)) {
    const equal = assignment.indexOf('=')
    makeEnv[assignment.slice(0, equal)] = assignment.slice(equal + 1)
  }
  // Never probe the requested goal: recursive recipes can execute even with -q.
  const result = spawnSync(invocation.make, ['-pRrq', ...probeArgs, '__gate_run_make_db_probe__'], {
    cwd,
    env: makeEnv,
    encoding: 'utf8',
    timeout: 2000,
    maxBuffer: 16 * 1024 * 1024,
  })
  if (result.error || ![0, 1, 2].includes(result.status) || !result.stdout?.includes('# Files'))
    return null
  return parseMakeDatabase(result.stdout)
}

function aliasRoot(target, rules) {
  const seen = new Set()
  let current = target
  while (!seen.has(current)) {
    seen.add(current)
    const rule = rules?.get(current)
    if (!rule) return current
    if (rule.recipe.length === 0 && rule.prerequisites.length === 1) current = rule.prerequisites[0]
    else if (rule.prerequisites.length === 0 && rule.recipe.length === 1) {
      const match = rule.recipe[0].match(/^\$\(MAKE\)\s+(?:-C\s+\.\s+)?([A-Za-z0-9_.\/-]+)$/)
      if (!match) return current
      current = match[1]
    } else return current
  }
  return target
}

export function canonicalCommand(argv, { makeDb = null, env = process.env } = {}) {
  const invocation = makeInvocation(argv)
  const canonical = invocation
    ? [
        ...invocation.prefix,
        invocation.make,
        ...invocation.args.map((arg, index) =>
          invocation.goals.has(index) ? aliasRoot(arg, makeDb) : arg,
        ),
      ]
    : [...argv]
  return {
    argv: canonical,
    forced: env.BOSS_GATE_FORCE_UNCACHED === '1' || forcesUncached(argv.join(' ')),
  }
}

export function reuseKey({ cwd, canonical, treeHash, baseCommit, forced, env = process.env }) {
  if (!treeHash || !baseCommit) return null
  return createHash('sha256')
    .update(
      JSON.stringify({
        cwd,
        canonical,
        treeHash,
        baseCommit,
        forced,
        env: {
          ...expansionEnv(env),
          GO_TEST_RUN: env.GO_TEST_RUN || '',
          GO_TEST_COUNT: env.GO_TEST_COUNT || '',
          SHORT: env.SHORT || '',
          CI: env.CI || '',
          node: process.version,
        },
      }),
    )
    .digest('hex')
}

export function reuseContext(argv, { cwd = process.cwd(), env = process.env } = {}) {
  const canonical = canonicalCommand(argv, { makeDb: readMakeDatabase(argv, { cwd, env }), env })
  try {
    const realCwd = fs.realpathSync(cwd)
    const git = (args) =>
      execFileSync('git', args, {
        cwd,
        encoding: 'utf8',
        stdio: ['ignore', 'pipe', 'ignore'],
      }).trim()
    const root = git(['rev-parse', '--show-toplevel'])
    let base = env.BASE_REF
    if (!base) {
      try {
        base = git(['symbolic-ref', '-q', 'refs/remotes/origin/HEAD'])
      } catch {
        base = 'origin/main'
      }
    }
    const treeHash = computeTreeHash(root)
    const baseCommit = resolveBaseCommit(root, base)
    return {
      key: reuseKey({
        cwd: realCwd,
        canonical: canonical.argv,
        treeHash,
        baseCommit,
        forced: canonical.forced,
        env,
      }),
      canonical: canonical.argv,
      forced: canonical.forced,
      treeHash,
    }
  } catch {
    return { key: null, canonical: canonical.argv, forced: canonical.forced, treeHash: null }
  }
}

function reuseWindow(env) {
  const value = Number(env.BOSS_GATE_REUSE_WINDOW_MS)
  return Number.isFinite(value) && value > 0 ? value : 3_600_000
}

export function decideReuse({
  entry,
  source,
  now = Date.now(),
  env = process.env,
  forced = false,
}) {
  if (env.BOSS_GATE_REUSE === '0' || !entry?.treeHash || !source) return 'fresh'
  if (source.status === null && source.alive && !source.stopped) return 'join'
  if (source.status !== 0 || source.stopped || forced || entry.canonical.includes('test-race'))
    return 'fresh'
  if (
    !source.tree?.start ||
    source.tree.start !== source.tree.end ||
    source.tree.end !== entry.treeHash
  )
    return 'fresh'
  const age = now - source.finishedAt
  return age >= 0 && age <= reuseWindow(env) ? 'reuse-passed' : 'fresh'
}

function readJson(file) {
  try {
    return JSON.parse(fs.readFileSync(file, 'utf8'))
  } catch {
    return null
  }
}

function sourceState(runDir) {
  let status = null
  let finishedAt = null
  let alive = false
  try {
    const file = path.join(runDir, 'status')
    status = Number(fs.readFileSync(file, 'utf8').trim())
    finishedAt = fs.statSync(file).mtimeMs
  } catch {}
  try {
    const pid = Number(fs.readFileSync(path.join(runDir, 'pid'), 'utf8').trim())
    if (Number.isInteger(pid) && pid > 0) {
      process.kill(pid, 0)
      alive = true
    }
  } catch (error) {
    alive = error.code === 'EPERM'
  }
  return {
    status,
    finishedAt,
    alive,
    stopped: fs.existsSync(path.join(runDir, 'stopped')),
    tree: readJson(path.join(runDir, 'tree.json')),
  }
}

/** Claim before spawning; a racing starter sees the launcher's pid until the supervisor replaces it. */
export function claimReuse(context, runDir, { env = process.env, now = Date.now() } = {}) {
  if (!context.key || env.BOSS_GATE_REUSE === '0') return { kind: 'fresh' }
  const dir = env.BOSS_GATE_RUN_INDEX_DIR || path.join(os.homedir(), '.cache', 'boss', 'gate-runs')
  try {
    fs.mkdirSync(dir, { recursive: true })
    for (const name of fs.readdirSync(dir)) {
      if (!name.endsWith('.json')) continue
      const file = path.join(dir, name)
      const entry = readJson(file)
      if (entry && (!fs.existsSync(entry.runDir) || now - entry.startedAt > 2 * reuseWindow(env)))
        fs.rmSync(file, { force: true })
    }
    const file = path.join(dir, `${context.key}.json`)
    const entry = {
      runDir,
      startedAt: now,
      treeHash: context.treeHash,
      canonical: context.canonical,
    }
    try {
      fs.writeFileSync(file, JSON.stringify(entry), { flag: 'wx' })
      return { kind: 'fresh' }
    } catch (error) {
      if (error.code !== 'EEXIST') throw error
    }
    const previous = readJson(file)
    const kind = decideReuse({
      entry: previous,
      source: previous?.runDir ? sourceState(previous.runDir) : null,
      now,
      env,
      forced: context.forced,
    })
    if (kind !== 'fresh')
      return { kind, from: previous.runDir, key: context.key, canonical: context.canonical }
    writeFileAtomic(file, JSON.stringify(entry))
  } catch {
    /* An unwritable or corrupt index never prevents a fresh gate. */
  }
  return { kind: 'fresh' }
}

export function recordRunTree(runDir, phase) {
  if (!fs.existsSync(path.join(runDir, 'reuse-key'))) return
  try {
    const tree = readJson(path.join(runDir, 'tree.json')) || {}
    const root = execFileSync('git', ['rev-parse', '--show-toplevel'], {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'ignore'],
    }).trim()
    tree[phase] = computeTreeHash(root)
    const file = path.join(runDir, 'tree.json')
    writeFileAtomic(file, JSON.stringify(tree))
  } catch {
    /* Missing tree evidence disables terminal reuse. */
  }
}
