#!/usr/bin/env node

// Prepare and audit a filtered full-history copy of this repository for publication.
//
//   prepare --source <path-or-url> --scratch <dir> [--spec <dir>] [--keep-ref <pattern> ...]
//           [--filter-repo <bin>]
//   audit   --repo <scratch> [--spec <dir>] --report <file>
//
// prepare mirror-clones the source into an empty scratch directory, drops every ref that is not
// kept, removes every remote, and runs git-filter-repo there with the spec in
// .github/scripts/public-history/. It never pushes: publication is a human step (see that
// directory's README). audit reads a prepared (or any) repository and writes a Markdown report;
// it exits 1 when a hard check fails, 0 when none does, and 2 on a usage or runtime error.

import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { UsageError } from './public-history-spec.mjs'
import { prepare } from './public-history-prepare.mjs'
import { audit } from './public-history-audit.mjs'

export { DEFAULT_SPEC_DIR } from './public-history-spec.mjs'
export { DEFAULT_REPLACEMENT } from './public-history-spec.mjs'
export { UsageError } from './public-history-spec.mjs'
export { globToRegex } from './public-history-spec.mjs'
export { parsePathsFile } from './public-history-spec.mjs'
export { pathRuleMatches } from './public-history-spec.mjs'
export { matchingPathRule } from './public-history-spec.mjs'
export { parseReplacementsFile } from './public-history-spec.mjs'
export { replacementRuleMatches } from './public-history-spec.mjs'
export { replacementMarker } from './public-history-spec.mjs'
export { parseAllowlistFile } from './public-history-spec.mjs'
export { loadSpec } from './public-history-spec.mjs'
export { parsePrivatePaths } from './public-history-spec.mjs'
export { uncoveredPrivatePaths } from './public-history-spec.mjs'
export { scratchRefusal } from './public-history-prepare.mjs'
export { selectKeptRefs } from './public-history-prepare.mjs'
export { filterRepoArgs } from './public-history-prepare.mjs'
export { prepare } from './public-history-prepare.mjs'
export { SECRET_FAMILIES } from './public-history-scan.mjs'
export { redact } from './public-history-scan.mjs'
export { audit } from './public-history-audit.mjs'
export { renderReport } from './public-history-report.mjs'

// ---------------------------------------------------------------------------------------------
// CLI

const USAGE = `usage:
  public-history.mjs prepare --source <path-or-url> --scratch <dir> [--spec <dir>]
                             [--keep-ref <pattern> ...] [--filter-repo <bin>]
  public-history.mjs audit --repo <scratch> [--spec <dir>] --report <file>`

const COMMAND_OPTIONS = {
  prepare: {
    source: { type: 'string' },
    scratch: { type: 'string' },
    spec: { type: 'string' },
    'keep-ref': { type: 'string', multiple: true },
    'filter-repo': { type: 'string' },
  },
  audit: {
    repo: { type: 'string' },
    spec: { type: 'string' },
    report: { type: 'string' },
  },
}

export async function main(argv, { stdout = process.stdout, stderr = process.stderr } = {}) {
  try {
    const [command, ...rest] = argv
    const options = Object.hasOwn(COMMAND_OPTIONS, command ?? '') ? COMMAND_OPTIONS[command] : null
    if (!options) throw new UsageError(`unknown or missing subcommand: ${command ?? '(none)'}`)
    const { values } = parseArgs({ args: rest, options, strict: true, allowPositionals: false })
    if (command === 'prepare') {
      const result = prepare({
        source: values.source,
        scratch: values.scratch,
        spec: values.spec,
        keepRefs: values['keep-ref'] ?? [],
        filterRepo: values['filter-repo'],
        log: (line) => stderr.write(`${line}\n`),
      })
      stdout.write(`${JSON.stringify({ scratch: result.scratch, keptRefs: result.keptRefs })}\n`)
      return 0
    }
    const result = await audit({ repo: values.repo, spec: values.spec, report: values.report })
    stdout.write(
      `${JSON.stringify({
        report: path.resolve(values.report),
        hardFindings: result.hardFindings.length,
        secretHits: result.secretHits.length,
      })}\n`,
    )
    return result.hardFindings.length === 0 ? 0 : 1
  } catch (error) {
    const usage = error instanceof UsageError || error?.code?.startsWith?.('ERR_PARSE_ARGS')
    stderr.write(`public-history: ${error.message}\n${usage ? `${USAGE}\n` : ''}`)
    return 2
  }
}

if (isMainModule(import.meta.url)) {
  process.exitCode = await main(process.argv.slice(2))
}
