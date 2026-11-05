import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { resolveBossBinary } from './boss-binary.mjs'
import { loadSkillConfig } from './skill-config.mjs'
import { isMainModule } from './main-module.mjs'

/**
 * Run the portable issue lease CLI, returning its exit status and JSON payload.
 * @param {string[]} argv
 * @param {{loadConfig?: () => {adapters: {tracker: string}}, resolveBinary?: typeof resolveBossBinary, run?: typeof spawnSync}} [deps]
 */
export function main(argv, deps = {}) {
  let values, command
  try {
    const parsed = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        issue: { type: 'string' },
        scope: { type: 'string' },
        token: { type: 'string' },
        'ttl-seconds': { type: 'string' },
      },
    })
    values = parsed.values
    command = parsed.positionals[0]
    const ttl = Number(values['ttl-seconds'] ?? 7200)
    if (
      parsed.positionals.length !== 1 ||
      !['acquire', 'release'].includes(command) ||
      !values.issue?.trim() ||
      !['plan', 'build'].includes(values.scope) ||
      !/^[a-fA-F0-9]{32}$/.test(values.token ?? '') ||
      !Number.isInteger(ttl) ||
      ttl <= 0 ||
      ttl > 2147483647 ||
      (command === 'release' && values['ttl-seconds'] !== undefined)
    ) {
      throw new Error(
        'expected acquire|release --issue ID --scope plan|build --token 32-hex [--ttl-seconds N]',
      )
    }
  } catch (error) {
    return { status: 2, error: error.message }
  }
  const unavailable = (reason) => ({
    status: command === 'release' ? 0 : 4,
    result: { verdict: 'unavailable', reason },
  })
  try {
    const config = (deps.loadConfig ?? loadSkillConfig)()
    const binary = (deps.resolveBinary ?? resolveBossBinary)()
    if (!binary.ok) return unavailable(binary.reason)
    const args = [
      'lease',
      command,
      '--issue',
      `${config.adapters.tracker}:${values.issue}`,
      '--purpose',
      values.scope,
      '--holder',
      values.token,
    ]
    if (command === 'acquire') args.push('--ttl', `${values['ttl-seconds'] ?? 7200}s`)
    args.push('--json')
    const response = (deps.run ?? spawnSync)(binary.path, args, {
      encoding: 'utf8',
      timeout: 15000,
      shell: false,
    })
    if (response.error) return unavailable(response.error.message)
    if (response.status !== 0 && response.status !== 3) {
      return unavailable(String(response.stderr || `boss exited ${response.status}`).trim())
    }
    if (command === 'acquire' && response.status === 3) {
      let expiresAt = ''
      try {
        expiresAt = JSON.parse(String(response.stdout)).expires_at ?? ''
      } catch {
        // Exit 3 is contention even when an older CLI emits no JSON.
      }
      return { status: 3, result: { verdict: 'held', expiresAt } }
    }
    const payload = JSON.parse(String(response.stdout))
    if (command === 'release') {
      if (response.status !== 0 || typeof payload.is_released !== 'boolean')
        return unavailable('invalid boss release response')
      return { status: 0, result: { verdict: payload.is_released ? 'released' : 'not-held' } }
    }
    if (
      payload.is_acquired !== true ||
      !['organization', 'daemon'].includes(payload.scope) ||
      typeof payload.expires_at !== 'string'
    ) {
      return unavailable('invalid boss acquire response')
    }
    return {
      status: 0,
      result: { verdict: 'acquired', scope: payload.scope, expiresAt: payload.expires_at },
    }
  } catch (error) {
    return unavailable(error.message)
  }
}

if (isMainModule(import.meta.url)) {
  const outcome = main(process.argv.slice(2))
  if (outcome.result) process.stdout.write(`${JSON.stringify(outcome.result)}\n`)
  if (outcome.error) process.stderr.write(`${outcome.error}\n`)
  process.exitCode = outcome.status
}
