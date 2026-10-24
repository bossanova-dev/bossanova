#!/usr/bin/env node

// run-ledger.mjs — the native ledger step of the BOS-339 make-over-bazel facade.
//
// After `bazel test //...` runs every non-`manual` target, this runs the ledger
// rows that bazel excludes but today's `make test` still had to cover natively
// (the bazel-`manual` targets). Selection is by disposition (and optional module),
// preserving today-parity exactly:
//   default-run   — ran under today's plain `go test ./...`; runs in the hot loop.
//   explicit-only — intentionally excluded from the hot loop (own explicit target).
//
// Flags:
//   --disposition <default-run|explicit-only>  filter (required in practice)
//   --module <path>                            optional module filter (e.g. services/boss);
//                                              repeatable — the union is selected
//   --race                                     inject `-race` into each `go test`
//   --print                                    print commands, do not execute (dry run)
// Env:
//   LEDGER_DRY_RUN=1                           same as --print
//
// Behavior: prints each selected command before running. In print/dry-run mode it
// executes nothing and exits 0. Otherwise it runs each command via `sh -c` with
// inherited stdio; on the first non-zero exit it stops and propagates that code. A row
// listed in scripts/bazel/flaky-tests.json gets up to two retries first, and a retry that
// passes prints a `run-ledger: FLAKY` line.
// An empty selection prints a note and exits 0 (e.g. plugins have no ledger rows).

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { quarantinedLabels } from '../guards/check-flaky-quarantine.mjs'

// fileURLToPath (not `new URL(...).pathname`) so a checkout path containing a
// space or other percent-encoded char resolves to a real filesystem path —
// bossd provisions worktrees under user-controlled paths.
const scriptDir = path.dirname(fileURLToPath(import.meta.url))
// The two env overrides exist so scripts/bazel/bazel-run-ledger.test.mjs can drive real
// retries against fixture rows; CI and make never set them.
const ledgerPath = process.env.RUN_LEDGER_FILE || path.join(scriptDir, 'ledger.json')
const quarantinePath =
  process.env.RUN_LEDGER_QUARANTINE_FILE || path.join(scriptDir, 'flaky-tests.json')

// A row whose label is in scripts/bazel/flaky-tests.json runs up to this many times, matching
// Bazel's default for a `flaky = True` target. scripts/guards/check-flaky-quarantine.mjs keeps every
// listed entry owned, tracked and expiring.
const FLAKY_ATTEMPTS = 3

// The only dispositions the ledger (and the today-parity contract) recognize.
// An unknown value must fail loudly, never silently select zero rows.
const VALID_DISPOSITIONS = new Set(['default-run', 'explicit-only'])

function parseArgs(argv) {
  const opts = { disposition: null, modules: null, race: false, print: false }
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]
    switch (arg) {
      case '--disposition':
        opts.disposition = argv[++i]
        break
      case '--module':
        // Repeatable: `--module a --module b` selects the UNION. A single
        // `--module` keeps its original meaning, so existing callers are unaffected.
        opts.modules ??= new Set()
        opts.modules.add(argv[++i])
        break
      case '--race':
        opts.race = true
        break
      case '--print':
        opts.print = true
        break
      default:
        console.error(`run-ledger: unknown argument: ${arg}`)
        process.exit(2)
    }
  }
  return opts
}

function withRace(command) {
  // Inject `-race` immediately after the first `go test ` so it precedes flags
  // and packages: `go test -race -timeout 300s ./...` (a valid `go test` form).
  return command.replace('go test ', 'go test -race ')
}

function main() {
  const opts = parseArgs(process.argv.slice(2))

  // Reject an unknown --disposition (e.g. a typo) instead of matching zero rows
  // and exiting 0 — that would silently skip every default-run manual target
  // while `make test` still reported success (a today-parity hole).
  if (opts.disposition !== null && !VALID_DISPOSITIONS.has(opts.disposition)) {
    console.error(
      `run-ledger: unknown --disposition "${opts.disposition}" (expected ${[...VALID_DISPOSITIONS].join(' | ')})`,
    )
    process.exit(2)
  }

  const dryRun = opts.print || process.env.LEDGER_DRY_RUN === '1'

  const ledger = JSON.parse(fs.readFileSync(ledgerPath, 'utf8'))

  const selected = ledger.filter((row) => {
    if (opts.disposition && row.disposition !== opts.disposition) return false
    if (opts.modules && !opts.modules.has(row.module)) return false
    return true
  })

  if (selected.length === 0) {
    const scope = [
      opts.disposition ? `disposition=${opts.disposition}` : null,
      opts.modules ? `module=${[...opts.modules].join(',')}` : null,
    ]
      .filter(Boolean)
      .join(' ')
    console.log(`run-ledger: no ledger rows match (${scope || 'no filter'}) — nothing to run`)
    process.exit(0)
  }

  const quarantined = quarantinedLabels(fs.readFileSync(quarantinePath, 'utf8'))

  for (const row of selected) {
    const command = opts.race ? withRace(row.command) : row.command
    console.log(command)
    if (dryRun) continue
    const attempts = quarantined.has(row.label) ? FLAKY_ATTEMPTS : 1
    let code = 0
    for (let attempt = 1; attempt <= attempts; attempt++) {
      const result = spawnSync('sh', ['-c', command], { stdio: 'inherit' })
      code = result.status === null ? 1 : result.status
      if (code === 0) {
        if (attempt > 1) {
          console.log(
            `run-ledger: FLAKY ${row.label} passed on attempt ${attempt}/${attempts} (quarantined in scripts/bazel/flaky-tests.json)`,
          )
        }
        break
      }
      if (attempt < attempts) {
        console.error(
          `run-ledger: ${row.label} failed (exit ${code}) on attempt ${attempt}/${attempts}; retrying (quarantined)`,
        )
      }
    }
    if (code !== 0) {
      console.error(`run-ledger: command failed (exit ${code}): ${command}`)
      process.exit(code)
    }
  }
}

main()
