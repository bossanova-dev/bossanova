/** @typedef {[string, string[], object?]} Call
 * @typedef {{ok: boolean, [key: string]: any}} Result
 * @typedef {((command: string, args: string[], opts?: object) => Result) & {uncached?: Reader}} Reader
 * @typedef {(command: string, args: string[], opts?: object) => Promise<Result>} AsyncReader
 */

// How many reads `warm` runs at once: enough to hide per-call latency, few enough to stay well
// inside GitHub's concurrent-request limits.
export const WARM_CONCURRENCY = 8

/**
 * A reader that runs each distinct read once, for a command that never writes, so a scan that asks
 * for the same statuses, permission or PR twice pays once. `warm(calls)` runs a batch of
 * `[command, args, opts]` reads concurrently through `readAsync` and caches the results, which the
 * synchronous reads then return; without `readAsync` it does nothing. A failed read is cached
 * too, so one scan sees one answer for each read.
 * @param {Reader} read
 * @param {AsyncReader | null} readAsync
 */
export function cachedReader(read, readAsync = null, { concurrency = WARM_CONCURRENCY } = {}) {
  const cache = new Map()
  const keyOf = (command, args, opts) => JSON.stringify([command, args, opts ?? {}])
  const cached = (command, args, opts) => {
    const key = keyOf(command, args, opts)
    if (!cache.has(key)) cache.set(key, read(command, args, opts))
    return cache.get(key)
  }
  cached.warms = readAsync !== null
  cached.uncached = read.uncached ?? read
  cached.warm = async (calls) => {
    if (!readAsync) return
    /** @type {Map<string, Call>} */
    const pending = new Map()
    for (const [command, args, opts] of calls) {
      const key = keyOf(command, args, opts)
      if (!cache.has(key)) pending.set(key, [command, args, opts])
    }
    const queue = [...pending]
    const worker = async () => {
      for (let next = queue.shift(); next; next = queue.shift()) {
        const [key, [command, args, opts]] = next
        let result
        try {
          result = await readAsync(command, args, opts)
        } catch {
          continue // left to the synchronous read
        }
        if (!cache.has(key)) cache.set(key, result)
      }
    }
    await Promise.all(Array.from({ length: Math.min(concurrency, queue.length) }, worker))
  }
  return cached
}

/**
 * Fresh caches for each observation boundary, sharing only successful reads the caller marks
 * stable for the operation. Synchronous and warmed reads populate the same stable memo.
 * @param {Reader} read
 * @param {AsyncReader | null} readAsync
 * @param {{sticky: (command: string, args: string[]) => boolean}} options
 */
export function operatorReads(read, readAsync, { sticky }) {
  const memo = new Map()
  const keyOf = (command, args, opts) => JSON.stringify([command, args, opts ?? {}])
  const remember = (command, args, opts, result) => {
    if (sticky(command, args) && result.ok) memo.set(keyOf(command, args, opts), result)
    return result
  }
  const stable = (command, args, opts) =>
    memo.get(keyOf(command, args, opts)) ?? remember(command, args, opts, read(command, args, opts))
  stable.uncached = read.uncached ?? read
  const stableAsync =
    readAsync &&
    (async (command, args, opts) =>
      memo.get(keyOf(command, args, opts)) ??
      remember(command, args, opts, await readAsync(command, args, opts)))
  return {
    read: stable,
    async round(calls) {
      const reader = cachedReader(stable, stableAsync)
      await reader.warm(calls)
      return reader
    },
  }
}
