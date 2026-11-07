import { readFileSync } from 'node:fs'

/**
 * A missing or malformed receipt is a refusal, never the legacy no-receipt path.
 * @param {string} path
 * @returns {{settled: boolean, attempts?: number, elapsedMs?: number}}
 */
export function loadReadReceipt(path) {
  const receipt = JSON.parse(readFileSync(path, 'utf8'))
  if (!receipt || typeof receipt.settled !== 'boolean') {
    throw new Error('read receipt must contain a boolean settled')
  }
  return receipt
}

/**
 * Preserve positive content evidence even when the read did not settle.
 * @template {{verdict: string | null, exitCode: number, reason: string}} T
 * @param {T} result
 * @param {{settled: boolean, attempts?: number, elapsedMs?: number}} receipt
 */
export function applyReadReceipt(result, receipt) {
  if (receipt.settled || !['unattributed', 'drift'].includes(result.verdict)) return result
  return {
    ...result,
    verdict: 'unsettled',
    cause: 'unsettled',
    exitCode: 1,
    reason: `read did not settle after ${receipt.attempts ?? 'unknown'} attempts and ${receipt.elapsedMs ?? 'unknown'} ms; the write is unconfirmed, not drifted`,
  }
}
