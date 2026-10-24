import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

const pending = new Set()
let exitHookInstalled = false

/**
 * @param {string} prefix
 * @param {{after: (cleanup: () => void) => void}} [t]
 * @returns {string}
 */
export function tempDir(prefix, t) {
  const dir = mkdtempSync(join(tmpdir(), prefix))
  const remove = () => {
    rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
    pending.delete(dir)
  }
  if (t) t.after(remove)
  else {
    pending.add(dir)
    if (!exitHookInstalled) {
      exitHookInstalled = true
      process.once('exit', () => {
        for (const path of pending) {
          rmSync(path, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
        }
      })
    }
  }
  return dir
}
