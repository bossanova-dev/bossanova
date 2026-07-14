// scripts/finalize/boss-finalize.mjs
// boss-finalize reference implementation of the finalize adapter (see adapter.mjs).
// Captures the tag-injection -> draft->ready -> repair finalize policy the
// boss-build skill owns today, with ZERO behaviour change: injectPrTag delegates
// to the existing add-pr-numbers.sh helper (the dependency-free finalize helper the
// cron siblings shell), and the operation map + policy constants are the exact values
// the SKILL body's Steps 8-10 use. node builtins only (the cron worktree is
// dependency-free — mirrors scripts/tracker/linear.mjs).
//
// A BossFinalizeAdapter has:
//   - injectPrTag(prNumber, {baseBranch}) — the one executable capability (delegates
//     to add-pr-numbers.sh, exactly as the current Step 8/9 shell block does); and
//   - operationMap, a declarative description of the agent-driven finalize
//     capabilities (ready the PR, run repair) plus the tag-injection command — the
//     single source of truth the spine names instead of hard-wiring the commands.

import { execFileSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

// The dependency-free finalize helper (NOT a boss CLI) is installed under BOTH skill
// trees: services/boss/internal/skillinstall ships it to ~/.claude and the Codex mirror
// ships it to ~/.codex. boss-build can run from either host, so resolve to whichever
// tree actually contains the helper instead of hard-wiring the Claude path — a
// Codex-only install has no .claude tree, so shelling the Claude path would ENOENT
// before any commit tags are injected. Reachable in a cron worktree; the same helper the
// cron siblings use.
const TAG_INJECT_REL = 'skills/bossanova/boss-finalize/add-pr-numbers.sh'
const TAG_INJECT_CANDIDATES = Object.freeze([
  path.join(os.homedir(), '.claude', TAG_INJECT_REL),
  path.join(os.homedir(), '.codex', TAG_INJECT_REL),
])

/**
 * Resolve the tag-injection helper to whichever skill tree ships it. Prefers the Claude
 * install (unchanged for the common case); falls back to the Codex install so a
 * Codex-only host still reaches the finalize path. Returns the Claude candidate when
 * neither exists so any ENOENT surfaces against the expected primary location.
 * @param {(p: string) => boolean} [exists]
 * @returns {string}
 */
export function resolveTagInject(exists = fs.existsSync) {
  return TAG_INJECT_CANDIDATES.find((p) => exists(p)) ?? TAG_INJECT_CANDIDATES[0]
}

const TAG_INJECT = resolveTagInject()

// Declarative map of agent-driven finalize capability -> the concrete command/skill
// the spine invokes for it. Frozen so a consumer can read but not mutate it.
export const finalizeOperationMap = Object.freeze({
  injectPrTag: Object.freeze({
    command: TAG_INJECT,
    arg: 'prNumber',
    env: 'BASE_BRANCH',
    summary: 'rebase since PR base + inject [#<PR>] into any commit missing it',
  }),
  readyPr: Object.freeze({
    command: 'gh pr ready',
    arg: 'prNumber',
    guard: 'isDraft==true',
    summary: 'flip a draft PR to ready-for-review once green',
  }),
  repair: Object.freeze({
    skill: 'boss-repair',
    cap: 5,
    summary: 'fix failing checks / rebase conflicts / review comments, capped',
  }),
})

const defaultRun = (cmd, args, opts) => execFileSync(cmd, args, { stdio: 'inherit', ...opts })

/**
 * Build the boss-finalize adapter.
 * @param {{runImpl?: (cmd: string, args: string[], opts: object) => any}} [config]
 * @returns {import('./adapter.mjs').FinalizeAdapter}
 */
export function createBossFinalizeAdapter({ runImpl = defaultRun } = {}) {
  return {
    finalize: 'boss-finalize',
    operationMap: finalizeOperationMap,
    // The exact constants the SKILL body's Steps 8-10 use today: tag format, the
    // boss-repair cap (Step 8), and the settle-loop cap (Step 10).
    policy: Object.freeze({ tagFormat: '[#<PR>]', repairCap: 5, settleCap: 3 }),
    injectPrTag(prNumber, { baseBranch } = {}) {
      return runImpl(TAG_INJECT, [String(prNumber)], {
        env: { ...process.env, BASE_BRANCH: baseBranch },
      })
    },
  }
}
