// Test support: a throwaway git repository with signing disabled, for the diff and describer
// suites. Not vendored; only tests import it.

import { spawnSync } from 'node:child_process'
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { validateResult } from '../skill-extensions.mjs'
import { detectAreas } from './verify-areas.mjs'
import { readChangedFiles } from './verify-diff.mjs'

const IDENT = [
  '-c',
  'user.name=T',
  '-c',
  'user.email=t@example.invalid',
  '-c',
  'commit.gpgsign=false',
]

/**
 * @param {{after: (cleanup: () => void) => void}} t
 * @returns {{dir: string, git: (...args: string[]) => string,
 *   write: (files: Record<string, string | Buffer | null>) => void,
 *   move: (from: string, to: string) => void, commit: (message?: string) => string}}
 */
export function gitFixture(t) {
  const dir = mkdtempSync(path.join(tmpdir(), 'verify-git-'))
  t.after(() => rmSync(dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 }))
  const git = (/** @type {string[]} */ ...args) => {
    const out = spawnSync('git', [...IDENT, ...args], { cwd: dir, encoding: 'utf8' })
    if (out.status !== 0) throw new Error(`git ${args.join(' ')}: ${out.stderr}`)
    return out.stdout.trim()
  }
  git('init', '--quiet', '--initial-branch=main')
  git('config', 'gc.auto', '0')
  return {
    dir,
    git,
    write(files) {
      for (const [file, content] of Object.entries(files)) {
        if (content === null) {
          git('rm', '--quiet', '-r', '--', file)
          continue
        }
        mkdirSync(path.dirname(path.join(dir, file)), { recursive: true })
        writeFileSync(path.join(dir, file), content)
        git('add', '--', file)
      }
    },
    move(from, to) {
      mkdirSync(path.dirname(path.join(dir, to)), { recursive: true })
      git('mv', from, to)
    },
    commit(message = 'change') {
      git('commit', '--quiet', '--allow-empty', '-m', message)
      return git('rev-parse', 'HEAD')
    },
  }
}

/**
 * The changed files git itself reports (`-M -U0`) for `before`, then `moves`, then `after`.
 * @param {{after: (cleanup: () => void) => void}} t @param {Record<string, string | null>} before
 * @param {[string, string][]} moves @param {Record<string, string | null>} after
 */
export function realDiff(t, before, moves, after) {
  const repo = gitFixture(t)
  repo.write(before)
  const baseSha = repo.commit('base')
  for (const [from, to] of moves) repo.move(from, to)
  repo.write(after)
  const headSha = repo.commit('head')
  return readChangedFiles({ baseSha, headSha, cwd: repo.dir })
}

/**
 * The describer envelope for one detected area of `base...head` (an empty path list when the
 * detector put nothing there).
 * @param {string} cwd @param {string} baseSha @param {string} headSha @param {string} area
 */
export function envelopeFor(cwd, baseSha, headSha, area, extension = 'boss-verify-test') {
  const found = detectAreas(readChangedFiles({ baseSha, headSha, cwd })).find(
    (entry) => entry.area === area,
  )
  return {
    role: 'proof-describe',
    extension,
    area: { area, label: found?.label ?? area, paths: found?.paths ?? [] },
    baseSha,
    headSha,
    pr: { number: 1, title: '', body: '' },
  }
}

/**
 * Run a describer module as its own script, the way the core does, and return its validated
 * result. Throws when it exits non-zero or fails proof-describe validation.
 * @param {string} moduleUrl @param {object} envelope @param {string} cwd
 * @param {NodeJS.ProcessEnv} [env]
 */
export function runDescriberScript(moduleUrl, envelope, cwd, env = process.env) {
  const out = spawnSync(process.execPath, [fileURLToPath(moduleUrl)], {
    input: JSON.stringify(envelope),
    cwd,
    env,
    encoding: 'utf8',
  })
  if (out.status !== 0) throw new Error(`describer exited ${out.status}: ${out.stderr}`)
  const result = JSON.parse(out.stdout)
  const checked = validateResult(result, 'proof-describe')
  if (!checked.ok) throw new Error(`invalid describer result: ${checked.errors.join('; ')}`)
  return result
}
