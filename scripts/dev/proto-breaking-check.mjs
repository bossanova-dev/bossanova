#!/usr/bin/env node
// Runs `buf breaking` for `make lint-proto-breaking`. By default it compares against the merge
// base of HEAD and origin/main, so a branch is judged only on its own proto changes: proto that
// main added after the branch point is not reported as deleted on a branch that lacks it.
import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/**
 * @param {string} cwd
 * @param {string[]} args
 */
function git(cwd, args) {
  const result = spawnSync('git', args, { cwd, encoding: 'utf8' })
  return { ok: result.status === 0, out: (result.stdout || '').trim() }
}

/**
 * Resolve the buf input for the commit where HEAD left baseRef.
 * @param {{cwd: string, baseRef?: string}} options
 * @returns {{against: string} | {error: string}}
 */
export function resolveBreakingBase({ cwd, baseRef = 'origin/main' }) {
  const fix = `fetch full history (git fetch --unshallow, or fetch-depth: 0 in actions/checkout), or set BUF_BREAKING_BASE to an explicit buf input`
  if (!git(cwd, ['rev-parse', '--verify', '--quiet', `${baseRef}^{commit}`]).ok) {
    return {
      error: `proto breaking check: ${baseRef} does not resolve to a commit; fetch it (git fetch origin main), or set BUF_BREAKING_BASE to an explicit buf input`,
    }
  }
  const base = git(cwd, ['merge-base', 'HEAD', baseRef])
  if (!base.ok || !/^[0-9a-f]{40,64}$/.test(base.out)) {
    const shallow = git(cwd, ['rev-parse', '--is-shallow-repository']).out === 'true'
    return {
      error: `proto breaking check: no merge base of HEAD and ${baseRef}${shallow ? ' (this clone is shallow)' : ''}; ${fix}`,
    }
  }
  return { against: `.git#ref=${base.out}` }
}

/** @param {string[]} argv */
export function main(argv, cwd = process.cwd()) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    options: {
      buf: { type: 'string' },
      against: { type: 'string', default: '' },
      'base-ref': { type: 'string', default: 'origin/main' },
    },
  })
  if (!values.buf) {
    process.stderr.write('proto breaking check: --buf is required\n')
    return 2
  }
  let against = values.against
  if (!against) {
    const resolved = resolveBreakingBase({ cwd, baseRef: values['base-ref'] })
    if ('error' in resolved) {
      process.stderr.write(`${resolved.error}\n`)
      return 2
    }
    against = resolved.against
  }
  process.stderr.write(`buf breaking --against ${against}\n`)
  const result = spawnSync(
    values.buf,
    ['breaking', '--against', against, '--against-config', 'buf.yaml'],
    { cwd, stdio: 'inherit' },
  )
  if (result.error) {
    process.stderr.write(`proto breaking check: ${result.error.message}\n`)
    return 2
  }
  return result.status ?? 1
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
