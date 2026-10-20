#!/usr/bin/env node
// The boss-verify cron gate with `--local-sessions-only` (verify-route.mjs): it acts only on PRs
// with a live session on this daemon, for a daemon that shares its repo with another machine. It
// is a file of its own because a project-config cron's gateCommand is `node <repo-relative path>`
// with no arguments. Extra arguments are passed through to the gate.

import { isMainModule } from '../main-module.mjs'
import { runBossVerifyGate } from './boss-verify.mjs'

if (isMainModule(import.meta.url))
  runBossVerifyGate(['--local-sessions-only', ...process.argv.slice(2)])
