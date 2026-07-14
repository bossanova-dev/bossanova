// scripts/tracker/cli.mjs
// Thin shell entrypoint that routes the boss-build spine's executable claim
// helpers through the resolved tracker adapter, so the skill names a tracker-agnostic
// capability instead of a Linear-specific script. node builtins only (the cron
// worktree is dependency-free).
//
//   node scripts/tracker/cli.mjs claim-token
//     -> prints a fresh run token (stdout), tracker-agnostic 32-char hex
//   node scripts/tracker/cli.mjs claim-verdict --me <token> --comments <json-array>
//     -> exit 0 WON (my token is the first writer), exit 3 LOST
//
// Verdict delegates to the resolved adapter's resolveClaim capability (BOS-190); the
// Linear reference impl computes first-writer-wins over the claim comments, unchanged.

import crypto from 'node:crypto'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { resolveTrackerAdapter } from './adapter.mjs'

// Tracker-agnostic run token: 16 random bytes as lowercase hex (32 chars) — the same
// shape the claim marker captures. Duplicated here rather than imported from the
// Linear claim helper so this dispatcher stays tracker-layer only.
export function generateClaimToken() {
  return crypto.randomBytes(16).toString('hex')
}

function parseFlags(rest) {
  const flags = {}
  for (let i = 0; i < rest.length; i += 2) flags[rest[i]?.replace(/^--/, '')] = rest[i + 1]
  return flags
}

/**
 * Dispatch one tracker capability. Returns the process exit code; never calls
 * process.exit directly so it is unit-testable.
 * @param {string[]} argv
 * @param {{write?: (s: string) => void, errWrite?: (s: string) => void, env?: object}} [io]
 * @returns {number}
 */
export function runCli(
  argv,
  {
    write = (s) => process.stdout.write(s),
    errWrite = (s) => process.stderr.write(s),
    env = process.env,
  } = {},
) {
  const [cmd, ...rest] = argv
  if (cmd === 'claim-token') {
    write(generateClaimToken() + '\n')
    return 0
  }
  if (cmd === 'claim-verdict') {
    const { me, comments } = parseFlags(rest)
    if (!me) {
      errWrite('claim-verdict: --me <token> is required\n')
      return 2
    }
    // --comments is required (parity with the linear-claim.mjs verdict it replaces): an absent
    // list is a caller error, not a silent LOST. Only 0 (WON) / 3 (LOST) route the claim.
    if (comments === undefined) {
      errWrite('claim-verdict: --comments <json-array> is required\n')
      return 2
    }
    const adapter = resolveTrackerAdapter({ env })
    const won = adapter.resolveClaim(JSON.parse(comments), me)
    return won ? 0 : 3
  }
  errWrite(`unknown tracker capability: ${cmd ?? '(none)'}\n`)
  return 2
}

const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)
if (invokedDirectly) {
  process.exit(runCli(process.argv.slice(2)))
}
