#!/usr/bin/env node

import { execFileSync, spawnSync } from 'node:child_process'
import { rmSync } from 'node:fs'
import { join } from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** Restore a capture's out-of-bound changes. The caller starts capture on a clean committed tip.
 * @param {string} cwd
 */
export function discardCaptureOutsideBoundary(cwd) {
  /** @param {string[]} args */
  const git = (args) => execFileSync('git', args, { cwd, encoding: 'utf8' })
  const root = git(['rev-parse', '--show-toplevel']).trim()
  const changed = new Set([
    ...git(['diff', 'HEAD', '--no-renames', '--name-only', '-z']).split('\0'),
    ...git(['diff', '--cached', 'HEAD', '--no-renames', '--name-only', '-z']).split('\0'),
    ...git(['ls-files', '--others', '--exclude-standard', '-z']).split('\0'),
  ])
  const discarded = [...changed]
    .filter((file) => file && !file.startsWith('docs/solutions/'))
    .sort()
  for (const file of discarded) {
    const tracked = spawnSync('git', ['cat-file', '-e', `HEAD:${file}`], { cwd })
    if (tracked.status === 0) {
      git(['restore', '--source=HEAD', '--staged', '--worktree', '--', file])
    } else {
      if (tracked.status !== 128 && tracked.status !== 1)
        throw new Error(`Cannot inspect HEAD:${file}`)
      git(['rm', '--cached', '--force', '--ignore-unmatch', '--', file])
      rmSync(join(root, file), { recursive: true, force: true, maxRetries: 3 })
    }
  }
  return { discarded }
}

if (isMainModule(import.meta.url)) {
  parseArgs({ options: {}, strict: true })
  console.log(JSON.stringify(discardCaptureOutsideBoundary(process.cwd())))
}
