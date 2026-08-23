// skills-toolbox/finalize/cli.mjs
// Thin shell entrypoint that routes the boss-build spine's executable finalize
// step (PR-number tag injection) through the resolved finalize adapter, so the skill
// names a finalize capability instead of hard-wiring the tag-injection script. node
// builtins only (the cron worktree is dependency-free).
//
//   BASE_BRANCH=<base> node toolbox/finalize/cli.mjs inject-pr-tag <pr-number>
//     -> rebases since the PR base and injects [#<PR>] into any commit missing it,
//        delegating to the boss-finalize adapter's injectPrTag capability (which
//        shells the add-pr-numbers.sh helper). Zero behaviour change vs. the prior
//        direct shell-out.

import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { resolveFinalizeAdapter } from './adapter.mjs'

/**
 * Dispatch one finalize capability. Returns the process exit code; never calls
 * process.exit directly so it is unit-testable.
 * @param {string[]} argv
 * @param {{env?: object, errWrite?: (s: string) => void, resolve?: Function, runImpl?: Function}} [io]
 * @returns {number}
 */
export function runCli(
  argv,
  {
    env = process.env,
    errWrite = (s) => process.stderr.write(s),
    resolve = (e) => resolveFinalizeAdapter(e, { runImpl }),
    runImpl,
  } = {},
) {
  const [cmd, ...rest] = argv
  if (cmd === 'inject-pr-tag') {
    const pr = rest[0]
    if (!pr) {
      errWrite('inject-pr-tag: <pr-number> is required\n')
      return 2
    }
    const adapter = resolve(env)
    try {
      adapter.injectPrTag(pr, { baseBranch: env.BASE_BRANCH })
    } catch (err) {
      errWrite(`inject-pr-tag: ${err?.message ?? String(err)}\n`)
      return Number.isInteger(err?.status) && err.status > 0 ? err.status : 1
    }
    return 0
  }
  errWrite(`unknown finalize capability: ${cmd ?? '(none)'}\n`)
  return 2
}

import { isMainModule } from '../main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  process.exit(runCli(process.argv.slice(2)))
}
