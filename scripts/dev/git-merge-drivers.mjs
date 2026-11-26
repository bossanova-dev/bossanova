#!/usr/bin/env node
// Define the git merge drivers .gitattributes names, in the repository's shared config.
//
//   node scripts/dev/git-merge-drivers.mjs install [--repo <dir>]
//
// `generated` keeps the current side of a file both sides changed and reports no conflict. It is
// for generated output only: the merge result is stale until the generators run again, and CI's
// drift check fails any head that skipped that step. Linked worktrees share the common config, so
// one install covers every worktree. Setting a value is idempotent and touches nothing else.

import { spawnSync } from 'node:child_process'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

export const DRIVERS = Object.freeze({
  generated: Object.freeze({
    name: 'keep the current side of a generated file; regenerate after the merge',
    driver: 'true',
  }),
})

/** @param {string} repo */
export function install(repo) {
  for (const [id, fields] of Object.entries(DRIVERS))
    for (const [key, value] of Object.entries(fields)) {
      const res = spawnSync('git', ['-C', repo, 'config', `merge.${id}.${key}`, value], {
        encoding: 'utf8',
      })
      if (res.status !== 0)
        throw new Error(`git config merge.${id}.${key} failed: ${res.stderr || res.error}`)
    }
}

/** @param {string[]} argv */
export function main(argv) {
  const { values, positionals } = parseArgs({
    args: argv,
    options: { repo: { type: 'string', default: process.cwd() } },
    allowPositionals: true,
    strict: true,
  })
  if (positionals.length !== 1 || positionals[0] !== 'install')
    throw new Error('usage: git-merge-drivers.mjs install [--repo <dir>]')
  install(values.repo)
  process.stdout.write(`merge drivers installed: ${Object.keys(DRIVERS).join(', ')}\n`)
}

if (isMainModule(import.meta.url)) {
  try {
    main(process.argv.slice(2))
  } catch (error) {
    process.stderr.write(`${error instanceof Error ? error.message : error}\n`)
    process.exitCode = 1
  }
}
