import { spawnSync } from 'node:child_process'

import { classifyChecks, isGreen } from '../pr-check-state.mjs'

// A lookup failure is a fact the clearance predicate can fail closed on, never a rejection.
export function createPrLookup({ run = spawnSync, timeoutMs = 10_000 } = {}) {
  const cache = new Map()
  const timeout = Number.isSafeInteger(timeoutMs) && timeoutMs > 0 ? timeoutMs : 10_000

  async function read(url) {
    let timer
    try {
      const result = await Promise.race([
        Promise.resolve().then(() =>
          run(
            'gh',
            ['pr', 'view', url, '--json', 'state,isDraft,mergedAt,files,statusCheckRollup'],
            {
              encoding: 'utf8',
              timeout,
              killSignal: 'SIGKILL',
              stdio: ['ignore', 'pipe', 'pipe'],
            },
          ),
        ),
        new Promise((resolve) => {
          timer = setTimeout(() => resolve({ error: 'timeout' }), timeout)
        }),
      ])
      if (result?.error) return { error: String(result.error.code ?? result.error) }
      if (result?.status !== 0) return { error: 'command-failed' }
      const facts = JSON.parse(String(result.stdout))
      if (
        !facts ||
        !['OPEN', 'CLOSED', 'MERGED'].includes(facts.state) ||
        typeof facts.isDraft !== 'boolean' ||
        !(facts.mergedAt === null || typeof facts.mergedAt === 'string')
      )
        return { error: 'invalid-pr-facts' }
      // At the page cap, gh may have omitted paths. An incomplete set cannot prove disjointness.
      const files =
        Array.isArray(facts.files) &&
        facts.files.length < 100 &&
        facts.files.every((file) => typeof file?.path === 'string' && file.path.trim() !== '')
          ? facts.files.map((file) => file.path)
          : null
      const rollup = facts.statusCheckRollup
      // The shared classifier ignores unnamed entries; reject them so a malformed sibling cannot
      // disappear beside a passing check and turn an unreadable rollup green.
      const validRollup =
        Array.isArray(rollup) &&
        rollup.every((entry) =>
          ['name', 'context'].some(
            (key) => typeof entry?.[key] === 'string' && entry[key].trim() !== '',
          ),
        )
      const checks = classifyChecks({
        rollup: validRollup ? rollup : null,
        ...(validRollup ? {} : { readError: 'invalid-rollup' }),
      })
      return {
        state: facts.state,
        isDraft: facts.isDraft,
        mergedAt: facts.mergedAt,
        files,
        checks,
        green: isGreen(checks),
      }
    } catch {
      return { error: 'lookup-failed' }
    } finally {
      clearTimeout(timer)
    }
  }

  return async (url) => {
    if (!cache.has(url)) cache.set(url, read(url))
    return cache.get(url)
  }
}
