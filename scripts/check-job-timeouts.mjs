#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import { keyValue, listWorkflowFiles, nestedUnder, structuralLines } from './workflow-yaml-lib.mjs'

// Every workflow job must declare `timeout-minutes`. Without one, a wedged step (a hung test, a
// stalled download, a BES upload tail) runs to GitHub's 360-minute default and bills every one of
// those minutes; the worst commit in the 2026-10 CI review sat in CI for 87 minutes. A job that
// calls a reusable workflow (`uses:` at job level) cannot set it and is exempt.

/**
 * Jobs in one workflow that declare neither `timeout-minutes` nor a job-level `uses:`.
 *
 * @param {string} contents Whole workflow file text.
 * @returns {{job: string, line: number}[]}
 */
export function findJobsWithoutTimeout(contents) {
  const lines = structuralLines(contents)
  const jobsAt = lines.findIndex(
    (entry) => entry.indent === 0 && keyValue(entry.text)?.key === 'jobs',
  )
  if (jobsAt === -1) return []
  const body = nestedUnder(lines, jobsAt, 0)
  if (body.length === 0) return []
  const jobIndent = lines[body[0]].indent
  const missing = []
  for (const index of body) {
    const entry = lines[index]
    if (entry.indent !== jobIndent) continue
    const job = keyValue(entry.text)?.key
    if (!job) continue
    // A job's own keys sit at its shallowest nested indent. `steps:` items may share that
    // indent (`- uses:` directly under `steps:`), so list items are never job keys.
    const children = nestedUnder(lines, index, jobIndent).map((i) => lines[i])
    const keyIndent = Math.min(...children.map((child) => child.indent))
    const keys = children
      .filter((child) => child.indent === keyIndent && !child.text.startsWith('-'))
      .map((child) => keyValue(child.text)?.key)
    if (!keys.includes('timeout-minutes') && !keys.includes('uses')) {
      missing.push({ job, line: entry.line })
    }
  }
  return missing
}

export function findJobsWithoutTimeoutInRepo(repoRoot, { fs: fsImpl = fs } = {}) {
  const files = listWorkflowFiles(repoRoot, { fs: fsImpl })
  const offenders = files.flatMap((relative) =>
    findJobsWithoutTimeout(
      fsImpl.readFileSync(path.join(repoRoot, ...relative.split('/')), 'utf8'),
    ).map((offender) => ({ ...offender, file: relative })),
  )
  return { files, offenders }
}

export function renderVerdict({ files, offenders }) {
  if (files.length === 0) {
    return { ok: false, text: 'No workflow files found: zero files read is not a pass.' }
  }
  if (offenders.length > 0) {
    return {
      ok: false,
      text: [
        `Job(s) without timeout-minutes (${files.length} workflow files read):`,
        ...offenders.map((o) => `  - ${o.file}:${o.line} job \`${o.job}\``),
        '',
        'Add `timeout-minutes:` to each job, about 2-3x its p99 duration, so a wedged run stops',
        'billing at a known ceiling instead of the 360-minute default.',
      ].join('\n'),
    }
  }
  return {
    ok: true,
    text: `Every job declares timeout-minutes (${files.length} workflow files read).`,
  }
}

function main() {
  parseArgs({ options: {}, strict: true, allowPositionals: false })
  const repoRoot = path.join(path.dirname(fileURLToPath(import.meta.url)), '..')
  const verdict = renderVerdict(findJobsWithoutTimeoutInRepo(repoRoot))
  if (!verdict.ok) {
    console.error(verdict.text)
    process.exit(1)
  }
  console.log(verdict.text)
}

if (isMainModule(import.meta.url)) main()
