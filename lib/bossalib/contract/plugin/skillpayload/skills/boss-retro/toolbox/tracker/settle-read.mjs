/**
 * Read until a predicate holds or another attempt would exceed the budget.
 * Transport errors propagate; the caller owns their classification.
 * @template T
 * @param {{read: () => Promise<T>, isSettled: (value: T) => boolean,
 * budgetMs: number, intervalMs: number, sleep: (ms: number) => Promise<void>,
 * now: () => number}} options
 * @returns {Promise<{value: T, settled: boolean, attempts: number, elapsedMs: number}>}
 */
export async function settleRead({ read, isSettled, budgetMs, intervalMs, sleep, now }) {
  const started = now()
  let attempts = 0
  for (;;) {
    const value = await read()
    attempts += 1
    const settled = isSettled(value)
    const elapsedMs = now() - started
    if (settled || elapsedMs + intervalMs > budgetMs) {
      return { value, settled, attempts, elapsedMs }
    }
    await sleep(intervalMs)
    // A delayed timer must not start an attempt after the deadline either.
    if (now() - started > budgetMs) {
      return { value, settled: false, attempts, elapsedMs: now() - started }
    }
  }
}
