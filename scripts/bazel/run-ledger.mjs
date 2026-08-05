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
// inherited stdio; on the first non-zero exit it stops and propagates that code.
// An empty selection prints a note and exits 0 (e.g. plugins have no ledger rows).

import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

// fileURLToPath (not `new URL(...).pathname`) so a checkout path containing a
// space or other percent-encoded char resolves to a real filesystem path —
// bossd provisions worktrees under user-controlled paths.
const scriptDir = path.dirname(fileURLToPath(import.meta.url))
const ledgerPath = path.join(scriptDir, 'ledger.json')

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

  for (const row of selected) {
    const command = opts.race ? withRace(row.command) : row.command
    console.log(command)
    if (dryRun) continue
    const result = spawnSync('sh', ['-c', command], { stdio: 'inherit' })
    if (result.status !== 0) {
      const code = result.status === null ? 1 : result.status
      console.error(`run-ledger: command failed (exit ${code}): ${command}`)
      process.exit(code)
    }
  }
}

main()
