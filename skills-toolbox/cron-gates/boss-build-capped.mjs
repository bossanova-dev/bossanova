#!/usr/bin/env node
// The boss-build cron gate with `--max-open-prs 8` (open-pr-limit.mjs): no build starts while the
// repository has 8 or more open PRs. It is a file of its own because a project-config cron's
// gateCommand is `node <repo-relative path>` with no arguments. Extra arguments are passed through
// to the gate.

import { isMainModule } from '../main-module.mjs'
import { runBossBuildGate } from './boss-build.mjs'

export const MAX_OPEN_PRS = 8

/**
 * @param {string[]} argv
 * @param {Parameters<typeof runBossBuildGate>[1]} [deps]
 */
export function runCappedBossBuildGate(argv, deps) {
  return runBossBuildGate(['--max-open-prs', String(MAX_OPEN_PRS), ...argv], deps)
}

if (isMainModule(import.meta.url)) await runCappedBossBuildGate(process.argv.slice(2))
