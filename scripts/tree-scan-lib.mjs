import { execFileSync } from 'node:child_process'
import { lstatSync, readFileSync, readlinkSync } from 'node:fs'
import { join } from 'node:path'

import { assertArtifactSet } from './size-ratchet-lib.mjs'

/** Extensions whose bytes are not prose and would be noise to scan. */
export const BINARY_EXT =
  /\.(png|jpe?g|gif|webp|ico|icns|pdf|zip|gz|tgz|bz2|xz|woff2?|ttf|otf|eot|mp4|mov|webm|wasm|bin|so|dylib|dll|exe)$/i

export const RESIDUAL =
  'trackedFiles depends on git ls-files output; this helper proves a non-empty enumeration but does not prove every caller chose the right pathspec.'

export function trackedFiles(repoRoot, { execFile = execFileSync } = {}) {
  const out = execFile('git', ['ls-files', '-z'], { cwd: repoRoot, maxBuffer: 64 << 20 })
  return out.toString('utf8').split('\0').filter(Boolean)
}

/**
 * Read the text Git stores for a tracked path.
 *
 * Git stores a symlink as a blob whose contents are the TARGET PATH, so that path is what a
 * tree-wide scan must read. `readFileSync` follows the link and reads the target file, which fails
 * in both directions: a link into a forbidden tree passes when the target file is clean, and the
 * target itself gets scanned twice. `lstat` + `readlink` reads what Git stores without spawning
 * `git cat-file` per file, and works on dangling links too.
 */
export function readTrackedText(filePath, root, { fsImpl = null } = {}) {
  const impl = fsImpl ?? { lstatSync, readFileSync, readlinkSync }
  const abs = join(root, filePath)
  const body = impl.lstatSync(abs).isSymbolicLink()
    ? impl.readlinkSync(abs)
    : impl.readFileSync(abs, 'utf8')
  return body
}

/**
 * Lines for a tracked text file, or null for binary/unreadable bytes.
 */
export function textLines(filePath, root) {
  if (BINARY_EXT.test(filePath)) return null
  let body
  try {
    body = readTrackedText(filePath, root)
  } catch {
    return null
  }
  if (body.includes('\0')) return null
  return body.split('\n')
}

export function assertNonVacuousScan(files, expectedLength, label) {
  assertArtifactSet(files, expectedLength, label)
}
