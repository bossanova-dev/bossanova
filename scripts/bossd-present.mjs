#!/usr/bin/env node

import fs from 'node:fs'
import { fileURLToPath } from 'node:url'

// Single source of truth for "did a bossd daemon provision this session?".
// bossd injects BOSS_SESSION_ID into every managed session's env and forbids
// .env from shadowing it (services/bossd/internal/session/tmux_chat_test.go,
// e2e_session_env_test.go). Its presence is the reliable managed-session signal;
// absence => standalone (a plain scheduled runner). If BOS-192 lands a canonical
// config/env detector, delegate to it here instead of reading the var directly.
export function isBossdManaged(env = process.env) {
  return Boolean(env.BOSS_SESSION_ID)
}

const invokedDirectly =
  process.argv[1] &&
  fs.realpathSync(process.argv[1]) === fs.realpathSync(fileURLToPath(import.meta.url))

if (invokedDirectly) {
  // exit 0 = managed, exit 3 = standalone (matches worktree-lock / pr-ownership).
  process.exit(isBossdManaged() ? 0 : 3)
}
