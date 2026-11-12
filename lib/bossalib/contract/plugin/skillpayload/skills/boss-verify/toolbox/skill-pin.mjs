import fs from 'node:fs'
import path from 'node:path'
import os from 'node:os'
import { createHash } from 'node:crypto'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { setTimeout } from 'node:timers/promises'
import { hashFile } from './toolbox-drift.mjs'
import { isMainModule } from './main-module.mjs'

const RECORD = '.boss-skill-pin.json'
// Long enough for a multi-day epic run's callback wakes; it only bounds what crashed runs leak.
export const PIN_TTL_MS = 14 * 24 * 60 * 60 * 1000

/** @param {string} skill */
function requireSkillName(skill) {
  if (!/^[a-z0-9][a-z0-9-]*$/.test(skill)) throw new Error('BLOCKED: invalid skill name')
}

// Every block of one run, and every callback wake of it, shares bossd's chat id; a concurrent
// run in the same worktree has another. Shell blocks spell this `${BOSS_AGENT_SESSION_ID:-local}`.
/** @param {NodeJS.ProcessEnv} env */
export function pinRun(env) {
  const run = env.BOSS_AGENT_SESSION_ID || 'local'
  if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(run)) throw new Error('BLOCKED: invalid pin run id')
  return run
}

/** @param {string} gitDir @param {string} skill @param {string} run */
export function pinDir(gitDir, skill, run) {
  requireSkillName(skill)
  return path.join(path.resolve(gitDir), 'boss-skill-pins', skill, run)
}

/** @param {{skill: string, skillsHome?: string, env?: NodeJS.ProcessEnv, home?: string}} options */
export function resolveInstalledSkill({
  skill,
  skillsHome,
  env = process.env,
  home = os.homedir(),
}) {
  requireSkillName(skill)
  const candidates = [
    skillsHome || env.BOSS_SKILLS_HOME,
    path.join(home, '.claude/skills'),
    path.join(home, '.codex/skills'),
  ].filter(Boolean)
  for (const candidate of candidates) {
    const source = path.join(candidate, skill)
    if (fs.existsSync(path.join(source, 'SKILL.md'))) {
      return { source: fs.realpathSync(source), skillsHome: path.resolve(candidate) }
    }
  }
  throw new Error(`BLOCKED: installed ${skill} has no SKILL.md`)
}

/** @typedef {{relpath: string, mode: number, sha256: string}} ManifestEntry */

// Compare executable bits rather than writable bits: the published pin is read-only.
/** @param {string} root @returns {ManifestEntry[]} */
export function captureManifest(root) {
  /** @type {ManifestEntry[]} */
  const entries = []
  /** @param {string} dir @param {string} prefix @param {Set<string>} ancestors */
  function walk(dir, prefix, ancestors) {
    const real = fs.realpathSync(dir)
    if (ancestors.has(real)) throw new Error(`cyclic symlink: ${prefix}`)
    const parents = new Set([...ancestors, real])
    for (const name of fs.readdirSync(dir).sort()) {
      if (!prefix && name === RECORD) continue
      const relpath = prefix ? `${prefix}/${name}` : name
      const file = path.join(dir, name)
      const stat = fs.statSync(file)
      if (stat.isDirectory()) walk(file, relpath, parents)
      else if (stat.isFile())
        entries.push({ relpath, mode: stat.mode & 0o111 ? 0o555 : 0o444, sha256: hashFile(file) })
      else throw new Error(`unsupported file: ${relpath}`)
    }
  }
  walk(root, '', new Set())
  return entries.sort((a, b) => a.relpath.localeCompare(b.relpath))
}

/** @param {ManifestEntry[]} manifest */
function digest(manifest) {
  return createHash('sha256').update(JSON.stringify(manifest)).digest('hex')
}

/** @param {string} dir */
function removeTree(dir) {
  fs.rmSync(dir, { recursive: true, force: true, maxRetries: 3 })
}

/** @param {string} parent @param {string} keep */
function sweepStalePins(parent, keep) {
  const cutoff = Date.now() - PIN_TTL_MS
  for (const name of fs.readdirSync(parent)) {
    const entry = path.join(parent, name)
    try {
      if (entry !== keep && fs.lstatSync(entry).mtimeMs < cutoff) removeTree(entry)
    } catch {
      // A peer run removed it first, or it is not ours to remove; the sweep never fails a pin.
    }
  }
}

/** @param {string} source */
function requireCapabilities(source) {
  if (!fs.statSync(path.join(source, 'SKILL.md')).isFile())
    throw new Error('SKILL.md is not a file')
  if (!fs.statSync(path.join(source, 'toolbox')).isDirectory())
    throw new Error('toolbox is not a directory')
}

/** @param {string} source @param {string} target */
function copyTree(source, target) {
  fs.mkdirSync(target, { recursive: true, mode: 0o755 })
  fs.chmodSync(target, 0o755)
  for (const name of fs.readdirSync(source)) {
    const from = path.join(source, name)
    const to = path.join(target, name)
    const stat = fs.statSync(from)
    if (stat.isDirectory()) copyTree(from, to)
    else if (stat.isFile()) {
      fs.copyFileSync(fs.realpathSync(from), to)
      fs.chmodSync(to, stat.mode & 0o777)
    } else throw new Error(`unsupported file: ${from}`)
  }
}

/**
 * @param {{skill: string, gitDir: string, skillsHome?: string, env?: NodeJS.ProcessEnv,
 * home?: string, onCaptured?: (attempt: number) => void}} options
 */
export async function createPin(options) {
  const { skill, gitDir, onCaptured, env = process.env } = options
  const run = pinRun(env)
  const destination = pinDir(gitDir, skill, run)
  const installed = resolveInstalledSkill(options)
  const parent = path.dirname(destination)
  fs.mkdirSync(parent, { recursive: true })
  let lastError = ''
  for (let attempt = 1; attempt <= 3; attempt++) {
    const stage = fs.mkdtempSync(path.join(parent, `.${run}-`))
    try {
      requireCapabilities(installed.source)
      if (fs.existsSync(path.join(installed.source, RECORD)))
        throw new Error('installed source contains a pin record')
      const before = captureManifest(installed.source)
      onCaptured?.(attempt)
      copyTree(installed.source, stage)
      const after = captureManifest(installed.source)
      const copied = captureManifest(stage)
      const sourceDigest = digest(before)
      const manifestSha256 = digest(copied)
      if (sourceDigest !== digest(after) || sourceDigest !== manifestSha256) {
        throw new Error('capture mismatch')
      }
      let payloadRecord = null
      const records = fs
        .readdirSync(installed.skillsHome)
        .filter((name) => name.startsWith('.') && name.endsWith('-skills.payload.json'))
      if (records.length === 1)
        payloadRecord = JSON.parse(
          fs.readFileSync(path.join(installed.skillsHome, records[0]), 'utf8'),
        )
      const record = {
        skill,
        source: installed.source,
        manifestSha256,
        fileCount: copied.length,
        pinnedAt: new Date().toISOString(),
        payloadRecord,
        manifest: copied,
      }
      for (const entry of copied) fs.chmodSync(path.join(stage, entry.relpath), entry.mode)
      fs.writeFileSync(path.join(stage, RECORD), `${JSON.stringify(record, null, 2)}\n`, {
        mode: 0o444,
      })
      fs.chmodSync(path.join(stage, RECORD), 0o444)
      const backup = `${stage}-previous`
      const replacing = fs.existsSync(destination)
      if (replacing) fs.renameSync(destination, backup)
      try {
        fs.renameSync(stage, destination)
      } catch (error) {
        if (replacing) fs.renameSync(backup, destination)
        throw error
      }
      removeTree(backup)
      sweepStalePins(parent, destination)
      return {
        skillDir: destination,
        toolbox: path.join(destination, 'toolbox'),
        manifestSha256,
        payloadRecord,
      }
    } catch (error) {
      lastError = error instanceof Error ? error.message : String(error)
    } finally {
      removeTree(stage)
    }
    if (attempt < 3) await setTimeout(25)
  }
  throw new Error(`BLOCKED: installed ${skill} kept changing while it was pinned (${lastError})`)
}

/** @param {{gitDir: string, skill: string, env?: NodeJS.ProcessEnv}} options */
export function verifyPin({ gitDir, skill, env = process.env }) {
  const dir = pinDir(gitDir, skill, pinRun(env))
  if (!fs.existsSync(dir)) return { status: 2, reason: 'skill pin missing' }
  try {
    const record = JSON.parse(fs.readFileSync(path.join(dir, RECORD), 'utf8'))
    requireCapabilities(dir)
    const actual = captureManifest(dir)
    /** @type {ManifestEntry[]} */
    const expected = record.manifest
    const first = actual.find(
      (entry, index) =>
        JSON.stringify(entry) !== JSON.stringify(expected[index]) ||
        (fs.statSync(path.join(dir, entry.relpath)).mode & 0o222) !== 0,
    )
    if (first || actual.length !== expected.length || digest(actual) !== record.manifestSha256) {
      return {
        status: 1,
        reason: `skill pin differs: ${first?.relpath || expected[actual.length]?.relpath || RECORD}`,
      }
    }
    return { status: 0, reason: 'skill pin verified' }
  } catch (error) {
    return { status: 1, reason: error instanceof Error ? error.message : String(error) }
  }
}

async function main() {
  const { values, positionals } = parseArgs({
    strict: true,
    allowPositionals: true,
    options: {
      skill: { type: 'string' },
      'skills-home': { type: 'string' },
      'git-dir': { type: 'string' },
    },
  })
  const [cmd] = positionals
  if (positionals.length !== 1 || !values.skill || !['create', 'verify', 'remove'].includes(cmd)) {
    throw new Error(
      'usage: skill-pin.mjs create|verify|remove --skill <name> [--skills-home <dir>] [--git-dir <dir>]',
    )
  }
  const gitDir =
    values['git-dir'] ||
    execFileSync('git', ['rev-parse', '--absolute-git-dir'], { encoding: 'utf8' }).trim()
  const options = { skill: values.skill, gitDir, skillsHome: values['skills-home'] }
  if (cmd === 'create') console.log(JSON.stringify(await createPin(options)))
  else if (cmd === 'remove') removeTree(pinDir(gitDir, values.skill, pinRun(process.env)))
  else if (cmd === 'verify') {
    const result = verifyPin(options)
    console.log(JSON.stringify(result))
    process.exitCode = result.status
  }
}

if (isMainModule(import.meta.url)) {
  main().catch((error) => {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  })
}
