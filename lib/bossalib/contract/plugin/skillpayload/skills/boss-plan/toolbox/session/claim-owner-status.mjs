import { readClaimReceipt } from '../plan/plan-claim.mjs'
import { spawnSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { resolveBossBinary } from '../boss-binary.mjs'
import { loadClaimComments, parseClaimComments } from '../linear-claim.mjs'
import { runIssueLease as releaseIssueLease } from '../issue-lease.mjs'
import { isMainModule } from '../main-module.mjs'

/**
 * Gather only proven-terminal owner evidence; a failed local lookup is unknown.
 * @param {string[]} argv
 * @param {{resolveBinary?: typeof resolveBossBinary, run?: typeof spawnSync,
 * now?: () => Date, releaseLease?: typeof releaseIssueLease}} [deps]
 */
export function main(argv, deps = {}) {
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        comments: { type: 'string' },
        'claim-receipt': { type: 'string' },
        out: { type: 'string' },
        issue: { type: 'string' },
        'release-forfeited-leases': { type: 'boolean' },
      },
    })
    if (
      positionals.length ||
      !values.comments ||
      !values['claim-receipt'] ||
      !values.out ||
      (values['release-forfeited-leases'] && !values.issue?.trim())
    )
      throw new Error(
        'expected --comments <file|json> --claim-receipt <path> --out <file> [--issue <ID> --release-forfeited-leases]',
      )
    const receipt = readClaimReceipt(values['claim-receipt'])
    const claims = parseClaimComments(loadClaimComments(values.comments), { scope: 'plan' }).filter(
      (claim) => claim.token !== receipt.token,
    )
    const now = (deps.now ?? (() => new Date()))().getTime()
    const owners = new Map()
    let binary
    try {
      binary = (deps.resolveBinary ?? resolveBossBinary)()
    } catch {
      binary = null
    }
    for (const { sessionId } of claims) {
      if (!sessionId || owners.has(sessionId)) continue
      let reason = 'owner status unavailable'
      let terminal = false
      if (binary?.ok && !sessionId.startsWith('-')) {
        try {
          const response = (deps.run ?? spawnSync)(binary.path, ['show', sessionId, '--json'], {
            encoding: 'utf8',
            timeout: 15000,
            shell: false,
          })
          if (!response.error && response.status === 0) {
            const session = JSON.parse(String(response.stdout))
            if (typeof session?.archived_at === 'string' && session.archived_at.trim()) {
              terminal = true
              reason = 'archived'
            } else if (session?.state === 'CLOSED') {
              terminal = true
              reason = 'closed'
            }
          }
        } catch {
          /* Unreadable local state is not proof about a peer on another host. */
        }
      }
      owners.set(sessionId, { terminal, reason })
    }
    const sessions = Object.fromEntries([...owners].filter(([, owner]) => owner.terminal))
    const peers = claims.map(({ token, sessionId, createdAt }) => {
      const owner = owners.get(sessionId)
      const peer = {
        token,
        sessionId,
        createdAt,
        ageMinutes: Math.max(0, (now - Date.parse(createdAt)) / 60000),
        owner: !sessionId ? 'none' : owner?.terminal ? 'terminal' : 'unknown',
        reason: owner?.reason ?? 'claim has no owner',
      }
      if (!owner?.terminal || !values['release-forfeited-leases']) return peer
      let lease
      try {
        const released = (deps.releaseLease ?? releaseIssueLease)({
          command: 'release',
          scope: 'plan',
          issue: values.issue,
          token,
        })
        lease = released.result ?? { verdict: 'unavailable', reason: released.error }
      } catch (error) {
        lease = { verdict: 'unavailable', reason: error.message }
      }
      return { ...peer, lease }
    })
    writeFileSync(values.out, `${JSON.stringify({ sessions, peers })}\n`)
    return {
      status: 0,
      summary: `peers=${peers.length} terminal=${peers.filter((peer) => peer.owner === 'terminal').length} unknown=${peers.filter((peer) => peer.owner === 'unknown').length}`,
    }
  } catch (error) {
    return { status: 2, error: error.message }
  }
}

if (isMainModule(import.meta.url)) {
  const outcome = main(process.argv.slice(2))
  if (outcome.summary) process.stdout.write(`${outcome.summary}\n`)
  if (outcome.error) process.stderr.write(`${outcome.error}\n`)
  process.exitCode = outcome.status
}
