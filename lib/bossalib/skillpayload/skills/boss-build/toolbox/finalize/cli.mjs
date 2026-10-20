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
//   node toolbox/finalize/cli.mjs --help
//     -> prints the capability list (derived from the resolved adapter's own
//        operationMap) and exits 0.

import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { resolveFinalizeAdapter } from './adapter.mjs'

// The invocation spelling for each operationMap key. The map is the single source of
// BOTH dispatch and documentation, so help enumerates the map rather than a literal
// list — a capability added to the map without a line here still appears in help,
// under its own key, instead of being silently undiscoverable.
const CAPABILITY_INVOCATION = Object.freeze({
  injectPrTag: 'inject-pr-tag <pr-number>   (env: BASE_BRANCH)',
})

/**
 * Render the help text. Derived from the resolved adapter's operationMap so the
 * capability names in help can never drift from the ones that dispatch.
 * @param {Record<string, {summary?: string}>|undefined} operationMap
 * @returns {string}
 */
export function finalizeUsage(operationMap) {
  const lines = [
    'usage: node finalize/cli.mjs <capability> [args]',
    '',
    'Executable capability (runs here):',
    '  inject-pr-tag <pr-number>',
    '      Rebase since BASE_BRANCH and inject the PR tag into any commit missing it.',
    '      BASE_BRANCH selects the base; the PR number is required.',
    '',
    '  --help, -h, help',
    '      Print this message and exit 0.',
    '',
    'Finalize capabilities declared by the resolved adapter (operationMap keys):',
  ]
  const entries = Object.entries(operationMap ?? {})
  if (entries.length === 0) {
    lines.push('  (the finalize adapter could not be resolved — see stderr)')
  }
  for (const [key, op] of entries) {
    const summary = typeof op?.summary === 'string' ? op.summary : '(no summary)'
    lines.push(`  ${key} — ${summary}`)
    const invocation = CAPABILITY_INVOCATION[key]
    if (invocation) lines.push(`      run as: ${invocation}`)
  }
  return lines.join('\n') + '\n'
}

/**
 * Dispatch one finalize capability. Returns the process exit code; never calls
 * process.exit directly so it is unit-testable.
 * @param {string[]} argv
 * @param {{env?: object, write?: (s: string) => void, errWrite?: (s: string) => void,
 *   resolve?: Function, runImpl?: Function}} [io]
 * @returns {number}
 */
export function runCli(
  argv,
  {
    env = process.env,
    write = (s) => process.stdout.write(s),
    errWrite = (s) => process.stderr.write(s),
    resolve = (e) => resolveFinalizeAdapter(e, { runImpl }),
    runImpl,
  } = {},
) {
  const [cmd, ...rest] = argv
  // Resolved BEFORE the dispatch chain, so `--help` can never fall through to the
  // unknown-capability path it used to answer with (`unknown finalize capability:
  // --help`, exit 2). An adapter that cannot resolve still gets a usage block and
  // exit 0 — help is a documentation surface, not a health check.
  if (cmd === '--help' || cmd === '-h' || cmd === 'help') {
    let operationMap
    try {
      operationMap = resolve(env)?.operationMap
    } catch (err) {
      errWrite(`help: could not resolve the finalize adapter: ${err?.message ?? String(err)}\n`)
    }
    write(finalizeUsage(operationMap))
    return 0
  }
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
  // The capability list goes out on the rejection too: a caller who guessed wrong
  // learns the real set here rather than having to read this file.
  let operationMap
  try {
    operationMap = resolve(env)?.operationMap
  } catch {
    operationMap = undefined
  }
  errWrite(finalizeUsage(operationMap))
  return 2
}

import { isMainModule } from '../main-module.mjs'

const invokedDirectly = isMainModule(import.meta.url)
if (invokedDirectly) {
  process.exit(runCli(process.argv.slice(2)))
}
