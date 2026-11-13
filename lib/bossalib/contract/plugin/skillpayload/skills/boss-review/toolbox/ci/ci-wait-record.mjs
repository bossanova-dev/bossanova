// ci-wait-record.mjs — where ../ci-wait.mjs keeps its per-PR, per-head wait records, and the one
// read other helpers make of them: did a wait on this exact head conclude green?
//
// The record is a latch of the wait's last reading, written by `ci-wait.mjs run`. Its `settled`
// verdict means the head's checks were green on two consecutive reads with every workflow run
// complete, which is the build's own recorded evidence that CI passed on that head. Reading it
// makes no network call. Node builtins only.

import { createHash } from 'node:crypto'
import { readFileSync, readdirSync, statSync } from 'node:fs'
import os from 'node:os'
import path from 'node:path'

// gitCommonDir finds the git directory every worktree of the checkout containing `cwd` shares — the
// repository `gh pr` resolves the PR number against — or null outside any checkout.
function gitCommonDir(cwd) {
  for (let dir = path.resolve(cwd); ; dir = path.dirname(dir)) {
    const dotGit = path.join(dir, '.git')
    try {
      if (statSync(dotGit).isDirectory()) return dotGit
      const gitdir = /^gitdir:\s*(.+)$/m.exec(readFileSync(dotGit, 'utf8'))?.[1]?.trim()
      if (gitdir) {
        const own = path.resolve(dir, gitdir)
        try {
          return path.resolve(own, readFileSync(path.join(own, 'commondir'), 'utf8').trim())
        } catch {
          return own
        }
      }
    } catch {
      // No .git here; keep climbing.
    }
    if (path.dirname(dir) === dir) return null
  }
}

// defaultStateDir namespaces the state files by repository, so a same-numbered PR of another
// checkout on this host is never adopted as this wait.
export function defaultStateDir(cwd = process.cwd()) {
  const identity = gitCommonDir(cwd) ?? path.resolve(cwd)
  const key = createHash('sha256').update(identity).digest('hex').slice(0, 16)
  return path.join(os.tmpdir(), 'boss-ci-wait', key)
}

/**
 * The recorded wait that concluded `settled` (green) on exactly `headSha`, or null.
 *
 * Any PR's record counts: green CI on a commit is a fact about that commit. A missing directory, a
 * torn file or a record for another head is simply not evidence.
 *
 * @param {{ stateDir: string, headSha: string }} options
 * @returns {{ file: string, pr: number, headSha: string, atMs: number | null } | null}
 */
export function settledWaitOn({ stateDir, headSha }) {
  let names
  try {
    names = readdirSync(stateDir)
  } catch {
    return null
  }
  for (const name of names.filter((n) => /^pr\d+-[0-9a-f]+\.json$/.test(n)).sort()) {
    if (!name.endsWith(`-${headSha}.json`)) continue
    const file = path.join(stateDir, name)
    let record
    try {
      record = JSON.parse(readFileSync(file, 'utf8'))
    } catch {
      continue
    }
    if (record?.headSha === headSha && record.lastVerdict?.state === 'settled')
      return { file, pr: record.pr, headSha, atMs: record.lastVerdict.atMs ?? null }
  }
  return null
}
