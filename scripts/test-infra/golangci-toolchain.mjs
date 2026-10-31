import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** @param {string} version */
function minor(version) {
  const match = /^go1\.(\d+)(?:\.\d+|rc\d+)?$/.exec(version)
  return match ? Number(match[1]) : null
}

/** Resolve the Go environment used by lint without overriding an operator's choice.
 * @param {{cwd?: string | URL, env?: NodeJS.ProcessEnv, exec?: typeof execFileSync}} [options]
 */
export function golangciGoEnv({
  cwd = process.cwd(),
  env = process.env,
  exec = execFileSync,
} = {}) {
  if (env.GOTOOLCHAIN) return { env, goVersion: null, pinnedFrom: null }
  /** @param {string[]} args @param {NodeJS.ProcessEnv} childEnv */
  const go = (args, childEnv) =>
    String(
      exec('go', args, { cwd, env: childEnv, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] }),
    ).trim()
  let toolchain
  try {
    if (go(['env', 'GOTOOLCHAIN'], env) !== 'auto')
      return { env, goVersion: null, pinnedFrom: null }
    toolchain = JSON.parse(go(['work', 'edit', '-json'], env)).Toolchain
  } catch {
    return { env, goVersion: null, pinnedFrom: null }
  }
  if (typeof toolchain !== 'string' || minor(toolchain) === null)
    return { env, goVersion: null, pinnedFrom: null }
  const localGo = go(['env', 'GOVERSION'], env)
  const localMinor = minor(localGo)
  if (localMinor === null || localMinor <= minor(toolchain))
    return { env, goVersion: localGo, pinnedFrom: null }
  /** @type {NodeJS.ProcessEnv} */
  const pinnedEnv = { ...env, GOTOOLCHAIN: toolchain }
  try {
    return { env: pinnedEnv, goVersion: go(['env', 'GOVERSION'], pinnedEnv), pinnedFrom: localGo }
  } catch (error) {
    throw new Error(
      `Cannot run lint with workspace toolchain ${toolchain} (local Go ${localGo}): ${error instanceof Error ? error.message : String(error)}. Let the go command download the toolchain once (check network/GOPROXY), or set GOTOOLCHAIN=local to lint with the local Go; findings may differ from CI.`,
    )
  }
}

if (isMainModule(import.meta.url)) {
  try {
    const { positionals } = parseArgs({ strict: true, allowPositionals: true })
    if (positionals.length !== 1 || positionals[0] !== 'goversion')
      throw new Error('usage: golangci-toolchain.mjs goversion')
    const resolved = golangciGoEnv()
    const version =
      resolved.goVersion ??
      execFileSync('go', ['env', 'GOVERSION'], { env: resolved.env, encoding: 'utf8' }).trim()
    console.log(version)
  } catch (error) {
    console.error(error instanceof Error ? error.message : String(error))
    process.exitCode = 1
  }
}
