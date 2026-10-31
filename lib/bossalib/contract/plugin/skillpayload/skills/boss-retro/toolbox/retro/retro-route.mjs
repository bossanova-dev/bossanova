// Route remedies using checkout and installed-core path evidence, never theme wording.
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { spawnSync } from 'node:child_process'
import { resolveCheckout } from '../git-checkout.mjs'
import { resolveBossBinary } from '../boss-binary.mjs'
import { idempotencyKey } from '../bs-record-notes.mjs'

export function upstreamConfig(config = {}, env = {}) {
  const value = config.retro?.upstream
  if (value !== undefined && value !== null) {
    const keys = Object.keys(value)
    if (
      typeof value !== 'object' ||
      Array.isArray(value) ||
      keys.length !== 1 ||
      !(
        (value.self === true && keys[0] === 'self') ||
        (typeof value.notesRepo === 'string' && value.notesRepo.trim() && keys[0] === 'notesRepo')
      )
    )
      throw new Error('retro.upstream must be either {notesRepo: nonblank string} or {self: true}')
  }
  if (env.BOSS_RETRO_UPSTREAM_REPO !== undefined) {
    if (!env.BOSS_RETRO_UPSTREAM_REPO.trim())
      throw new Error('BOSS_RETRO_UPSTREAM_REPO must be nonblank')
    return { notesRepo: env.BOSS_RETRO_UPSTREAM_REPO.trim() }
  }
  return value ? { ...value } : null
}

export function defaultProbes({ cwd = process.cwd(), env = process.env } = {}) {
  const { root } = resolveCheckout({ cwd })
  const listing = spawnSync('git', ['ls-files', '-z'], {
    cwd: root,
    encoding: 'utf8',
    timeout: 30000,
  })
  if (listing.status !== 0) throw new Error('tracked file inventory unreadable')
  const home =
    env.BOSS_SKILLS_HOME || path.resolve(path.dirname(fileURLToPath(import.meta.url)), '../../..')
  const installedCores = fs
    .readdirSync(home, { withFileTypes: true })
    .filter(
      (entry) =>
        /^boss(?:-.+)?$/.test(entry.name) && fs.existsSync(path.join(home, entry.name, 'SKILL.md')),
    )
    .map((entry) => path.join(home, entry.name))
  return {
    trackedFiles: listing.stdout.split('\0').filter(Boolean),
    installedCores,
    pathExists: fs.existsSync,
  }
}

export function classifyRemedyOwner(cluster, probes) {
  const tracked = new Set(probes.trackedFiles),
    cores = probes.installedCores ?? []
  const names = new Set(cores.map((core) => path.basename(core)))
  const vendored = (token) => {
    const match = token.match(/^\.(?:claude|codex|agents)\/skills\/([^/]+)\//)
    return match && names.has(match[1])
  }
  const tokens = new Set()
  for (const text of [
    cluster.ladder?.target,
    cluster.ladder?.rungEvidence,
    ...(cluster.wheres ?? [cluster.where]),
  ]) {
    // Exclude full URLs before extraction so their interior cannot become relative evidence.
    const relative = String(text ?? '').replace(/[a-z]+:\/\/[^\s`<>]+/gi, ' ')
    for (const raw of relative.match(
      /(?<![A-Za-z0-9_.@\-/~])[A-Za-z0-9_.@-]+(?:\/[A-Za-z0-9_.@-]+)+/g,
    ) ?? []) {
      const token = raw.replace(/[.,;:)\]]+$/, '')
      if (
        /\.[A-Za-z0-9]+$/.test(token) &&
        !token.split('/').some((part) => part === '.' || part === '..')
      )
        tokens.add(token)
    }
  }
  const evidence = []
  for (const token of tokens) {
    const aliases = Object.entries(probes.pathAliases ?? {}).filter(([from]) =>
      token.startsWith(from),
    )
    const candidates = [token, ...aliases.map(([from, to]) => to + token.slice(from.length))]
    const consumer = candidates.find((candidate) => tracked.has(candidate) && !vendored(candidate))
    if (consumer) {
      evidence.push({ path: token, owner: 'consumer', resolved: consumer })
      continue
    }
    const localCore = candidates.find((candidate) => tracked.has(candidate) && vendored(candidate))
    let installed
    for (const core of cores) {
      const parts = token.split('/')
      for (let i = 0; i < parts.length - 1 && !installed; i++) {
        const suffix = parts.slice(i).join('/')
        installed = [
          path.join(core, suffix),
          path.join(core, 'toolbox', suffix),
          ...(suffix.startsWith(path.basename(core) + '/')
            ? [path.join(path.dirname(core), suffix)]
            : []),
        ].find((candidate) => probes.pathExists(candidate))
      }
      if (installed) break
    }
    if (localCore || installed)
      evidence.push({ path: token, owner: 'core', resolved: localCore || installed })
  }
  const owners = new Set(evidence.map((item) => item.owner))
  return { owner: owners.size > 1 ? 'mixed' : owners.has('core') ? 'core' : 'consumer', evidence }
}

export function route(selection, { upstream = null, probes = undefined, pathAliases = {} } = {}) {
  const out = structuredClone(selection),
    forward = [],
    flagged = []
  if (!upstream?.self) {
    const inventory = { ...(probes ?? defaultProbes()), pathAliases }
    out.selected = out.selected.filter(({ cluster }) => {
      const ownership = classifyRemedyOwner(cluster, inventory)
      if (ownership.owner === 'consumer') return true
      const record = { key: cluster.key, ...ownership }
      if (ownership.owner === 'core' && upstream?.notesRepo) {
        forward.push({ ...record, notes: cluster.notes })
        return false
      }
      cluster.remedyOwner = ownership
      flagged.push(record)
      return true
    })
  }
  return {
    selection: out,
    forward,
    flagged,
    counts: { forwarded: forward.length, flagged: flagged.length },
  }
}

export function renderRemedyOwner(cluster) {
  const flag = cluster.remedyOwner
  if (!flag) return []
  return [
    '## Remedy owner',
    '',
    `Owner: ${flag.owner}. Configure retro.upstream.notesRepo to route installed boss-* core remedies upstream.`,
    '',
    ...flag.evidence.map((item) => `- ${item.owner}: \`${item.path.replace(/[`\r\n]/g, '')}\``),
    '',
  ]
}

function defaultBoss(args) {
  const binary = resolveBossBinary()
  if (!binary.ok) throw new Error(binary.reason)
  return spawnSync(binary.path, args, { encoding: 'utf8', timeout: 30000 })
}
export function forward(forwardPlan, { repo = undefined, boss = defaultBoss } = {}) {
  if (typeof repo !== 'string' || !repo.trim()) throw new Error('forward requires an upstream repo')
  const proven = [],
    failed = [],
    provenKeys = []
  const invoke = (args) => {
    const result = boss(args)
    if (result.error || result.status !== 0) throw new Error('notes command failed')
    return JSON.parse(result.stdout)
  }
  for (const theme of forwardPlan) {
    let complete = true
    for (const note of theme.notes) {
      try {
        const added = invoke([
          'notes',
          'add',
          '--repo',
          repo,
          '--tag',
          'improvement',
          '--json',
          '--idempotency-key',
          idempotencyKey(note.body),
          '--',
          note.body,
        ])
        if (typeof added.id !== 'string' || !added.id) throw new Error('note id missing')
        const read = invoke(['notes', 'show', added.id, '--json'])
        if (
          read.repo_id !== repo ||
          read.body !== note.body ||
          !Array.isArray(read.tags) ||
          !read.tags.includes('improvement')
        )
          throw new Error('read-back proof mismatch')
        proven.push(note.id)
      } catch (error) {
        complete = false
        failed.push({
          noteId: note.id,
          reason: error instanceof SyntaxError ? 'invalid notes JSON' : error.message,
        })
      }
    }
    if (complete) provenKeys.push(theme.key)
  }
  return { proven, failed, provenKeys }
}
export function forwardDeleteIds(plan, result) {
  const keys = new Set(result.provenKeys),
    proven = new Set(result.proven)
  return [
    ...new Set(
      plan.forward
        .filter((theme) => keys.has(theme.key) && theme.notes.every((note) => proven.has(note.id)))
        .flatMap((theme) => theme.notes.map((note) => note.id))
        .filter((id) => !id.startsWith('synthetic:')),
    ),
  ]
}
