import { realpathSync as defaultRealpathSync } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// Node resolves the main module's URL to its real path, while process.argv[1] remains the path
// the caller typed. Resolve both sides so direct CLI entry points also work through symlinks.
export function isMainModule(moduleUrl, opts = {}) {
  const argvPath = Object.hasOwn(opts, 'argvPath') ? opts.argvPath : process.argv[1]
  if (typeof argvPath !== 'string' || argvPath.length === 0) return false

  const realpathSync = opts.realpathSync ?? defaultRealpathSync
  const warn = opts.warn ?? ((message) => process.stderr.write(`${message}\n`))
  const modulePath = fileURLToPath(moduleUrl)
  let matches
  try {
    matches = realpathSync(argvPath) === realpathSync(modulePath)
  } catch {
    matches = path.resolve(argvPath) === path.resolve(modulePath)
  }
  if (!matches && path.basename(argvPath) === path.basename(modulePath)) {
    warn(`main module guard did not match ${path.basename(modulePath)}`)
  }
  return matches
}
