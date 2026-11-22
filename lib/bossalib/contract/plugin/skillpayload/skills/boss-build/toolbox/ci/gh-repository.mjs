import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { FALLBACK_LABELS, ghSpawn } from './gh-quota-read.mjs'
import hostsParser from './gh-hosts.generated.cjs'

const { hostKeys } = hostsParser

/** @param {string} repo @param {object} deps @param {number} timeoutMs @param {string} [label] */
export function repository(repo, deps, timeoutMs, label = FALLBACK_LABELS.PR_STATE) {
  const env = deps?.env ?? process.env
  if (!repo) repo = env.GH_REPO ?? ''
  if (!repo) {
    // This reads gh's local default remote configuration without spending API quota.
    const selected = ghSpawn(['repo', 'set-default', '--view'], {
      label,
      timeoutMs,
      deps,
    })
    if (selected.error || selected.status !== 0)
      throw new Error('could not read gh default repository')
    repo = String(selected.stdout ?? '').trim()
  }
  if (repo) {
    if (!/^(?:[\w.-]+(?::\d+)?\/)?[\w.-]+\/[\w.-]+$/.test(repo))
      throw new Error('invalid repository')
    return repo
  }
  return implicitRepository(deps, timeoutMs)
}

function remoteRepository(remote, translate) {
  let host, path
  let ssh = false
  const scp = /^(?:[^@/]+@)?([\w.-]+):([^/].*)$/.exec(remote)
  if (scp && !remote.includes('://')) {
    ;[, host, path] = scp
    ssh = true
  } else {
    try {
      const url = new URL(remote)
      if (!['https:', 'http:', 'ssh:', 'git:'].includes(url.protocol)) return null
      ssh = url.protocol === 'ssh:'
      host = url.hostname
      path = url.pathname.slice(1)
    } catch {
      return null
    }
  }
  const match = /^([\w.-]+\/[\w.-]+?)(?:\.git)?\/?$/.exec(path ?? '')
  if (!match) return null
  if (ssh) host = translate(host.toLowerCase())
  return { host: host.toLowerCase().replace(/^www\./, ''), path: match[1] }
}

function implicitRepository(deps, timeoutMs) {
  const env = deps?.env ?? process.env
  const res = (deps?.spawn ?? spawnSync)('git', ['remote', '-v'], {
    encoding: 'utf8',
    timeout: timeoutMs,
    env,
    ...(deps?.cwd ? { cwd: deps.cwd } : {}),
  })
  if (res.error || res.status !== 0) throw new Error('could not read git remotes')
  const remotes = new Map()
  const translate = sshTranslator(deps, timeoutMs)
  for (const line of String(res.stdout ?? '').split('\n')) {
    const match = /^(\S+)\s+(\S+)\s+\((fetch|push)\)$/.exec(line)
    if (!match) continue
    const [, name, url, direction] = match
    const parsed = remoteRepository(url, translate)
    if (parsed && (direction === 'fetch' || !remotes.has(name))) remotes.set(name, parsed)
  }
  const hosts = configuredHosts(env, deps?.cwd)
  const defaultHost = env.GH_HOST?.toLowerCase() || (hosts.length === 1 ? hosts[0] : null)
  let candidates = [...remotes].filter(
    ([, { host }]) => host === 'github.com' || host === defaultHost || hosts.includes(host),
  )
  if (defaultHost) {
    const matching = candidates.filter(([, { host }]) => host === defaultHost)
    if (env.GH_HOST || matching.length) candidates = matching
  }
  const priority = (name) => ({ upstream: 3, github: 2, origin: 1 })[name.toLowerCase()] ?? 0
  candidates.sort(([a], [b]) => priority(b) - priority(a))
  const selected = candidates[0]?.[1]
  if (!selected) throw new Error('no remote matches a configured GitHub host')
  return `${selected.host}/${selected.path}`
}

function configuredHosts(env, cwd) {
  // Match gh's ConfigDir precedence; only host keys are retained from its host map.
  // https://github.com/cli/go-gh/blob/trunk/pkg/config/config.go
  const directory =
    env.GH_CONFIG_DIR ||
    (env.XDG_CONFIG_HOME
      ? join(env.XDG_CONFIG_HOME, 'gh')
      : process.platform === 'win32' && env.AppData
        ? join(env.AppData, 'GitHub CLI')
        : join((process.platform === 'win32' ? env.USERPROFILE : env.HOME) || '', '.config', 'gh'))
  let text
  try {
    text = readFileSync(resolve(cwd ?? process.cwd(), directory, 'hosts.yml'), 'utf8')
  } catch (error) {
    if (error.code === 'ENOENT') return []
    throw new Error('could not read gh host configuration')
  }
  return hostKeys(text).map((host) => host.toLowerCase())
}

function sshTranslator(deps, timeoutMs) {
  const hosts = new Map()
  return (host) => {
    if (!hosts.has(host)) {
      // gh resolves SSH aliases locally before applying its known-host filter.
      // https://github.com/cli/go-gh/blob/trunk/pkg/ssh/ssh.go
      const result = (deps?.spawn ?? spawnSync)('ssh', ['-G', '--', host], {
        encoding: 'utf8',
        timeout: timeoutMs,
        killSignal: 'SIGKILL',
        maxBuffer: 1024 * 1024,
        env: deps?.env ?? process.env,
        ...(deps?.cwd ? { cwd: deps.cwd } : {}),
      })
      const resolved =
        !result.error && result.status === 0
          ? /^hostname\s+([\w.-]+)\s*$/im.exec(String(result.stdout ?? ''))?.[1]?.toLowerCase()
          : null
      const hostname = resolved || host
      hosts.set(host, hostname === 'ssh.github.com' ? 'github.com' : hostname)
    }
    return hosts.get(host)
  }
}
