#!/usr/bin/env node

import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseArgs } from 'node:util'

import { isMainModule } from '../skills-toolbox/main-module.mjs'
import {
  keyColumn,
  keyValue,
  listWorkflowFiles,
  nestedUnder,
  structuralLines,
} from './workflow-yaml-lib.mjs'

// Every workflow job must declare `timeout-minutes`. Without one, a wedged step (a hung test, a
// stalled download, a BES upload tail) runs to GitHub's 360-minute default and bills every one of
// those minutes; the worst commit in the 2026-10 CI review sat in CI for 87 minutes. A job that
// calls a reusable workflow (`uses:` at job level) cannot set it and is exempt.
//
// A job whose step caps add up to its own cap or more is reported too. When one capped step runs
// to its cap, the job hits its cap first and ends as cancelled, so later `!cancelled()` steps
// never run and nothing names the step that ran long.

/**
 * Each job of one workflow with its line, its own keys, and its steps' keys.
 *
 * @param {string} contents Whole workflow file text.
 * @returns {{job: string, line: number, keys: Map<string, string>, steps: Map<string, string>[]}[]}
 */
function readJobs(contents) {
  const lines = structuralLines(contents)
  const jobsAt = lines.findIndex(
    (entry) => entry.indent === 0 && keyValue(entry.text)?.key === 'jobs',
  )
  if (jobsAt === -1) return []
  const body = nestedUnder(lines, jobsAt, 0)
  if (body.length === 0) return []
  const jobIndent = lines[body[0]].indent
  const jobs = []
  for (const index of body) {
    const entry = lines[index]
    if (entry.indent !== jobIndent) continue
    const job = keyValue(entry.text)?.key
    if (!job) continue
    // A job's own keys sit at its shallowest nested indent. `steps:` items may share that
    // indent (`- uses:` directly under `steps:`), so list items are never job keys.
    const nested = nestedUnder(lines, index, jobIndent)
    const keyIndent = Math.min(...nested.map((i) => lines[i].indent))
    const keys = new Map()
    let stepsAt = -1
    for (const i of nested) {
      if (lines[i].indent !== keyIndent || lines[i].text.startsWith('-')) continue
      const kv = keyValue(lines[i].text)
      if (!kv) continue
      keys.set(kv.key, kv.value)
      if (kv.key === 'steps') stepsAt = i
    }
    jobs.push({
      job,
      line: entry.line,
      keys,
      steps: stepsAt === -1 ? [] : readSteps(lines, stepsAt),
    })
  }
  return jobs
}

// The keys of each item under `steps:`. Items may sit at the `steps:` indent itself, so the list
// ends at the first shallower line or the first same-indent line that is not an item.
function readSteps(lines, stepsAt) {
  const stepsIndent = lines[stepsAt].indent
  const body = []
  for (let i = stepsAt + 1; i < lines.length; i++) {
    const { indent, text } = lines[i]
    if (indent < stepsIndent || (indent === stepsIndent && !text.startsWith('-'))) break
    body.push(i)
  }
  if (body.length === 0) return []
  const itemIndent = lines[body[0]].indent
  return body
    .filter((i) => lines[i].indent === itemIndent && lines[i].text.startsWith('-'))
    .map((i) => {
      const column = keyColumn(lines[i])
      const keys = new Map()
      const first = keyValue(lines[i].text)
      if (first) keys.set(first.key, first.value)
      for (const j of nestedUnder(lines, i, lines[i].indent)) {
        if (lines[j].indent !== column) continue
        const kv = keyValue(lines[j].text)
        if (kv) keys.set(kv.key, kv.value)
      }
      return keys
    })
}

const minutes = (value) => (/^\d+$/.test(value ?? '') ? Number(value) : null)

/**
 * Jobs in one workflow that declare neither `timeout-minutes` nor a job-level `uses:`.
 *
 * @param {string} contents Whole workflow file text.
 * @returns {{job: string, line: number}[]}
 */
export function findJobsWithoutTimeout(contents) {
  return readJobs(contents)
    .filter(({ keys }) => !keys.has('timeout-minutes') && !keys.has('uses'))
    .map(({ job, line }) => ({ job, line }))
}

/**
 * Jobs whose literal step `timeout-minutes` add up to their literal job `timeout-minutes` or more.
 * An expression on either side is not a number this check can add, so it is skipped.
 *
 * @param {string} contents Whole workflow file text.
 * @returns {{job: string, line: number, jobMinutes: number, stepMinutes: number}[]}
 */
export function findJobsWithOverrunStepCaps(contents) {
  return readJobs(contents).flatMap(({ job, line, keys, steps }) => {
    const jobMinutes = minutes(keys.get('timeout-minutes'))
    if (jobMinutes === null) return []
    const stepMinutes = steps.reduce(
      (sum, step) => sum + (minutes(step.get('timeout-minutes')) ?? 0),
      0,
    )
    return stepMinutes >= jobMinutes ? [{ job, line, jobMinutes, stepMinutes }] : []
  })
}

export function findJobsWithoutTimeoutInRepo(repoRoot, { fs: fsImpl = fs } = {}) {
  const files = listWorkflowFiles(repoRoot, { fs: fsImpl })
  const offenders = []
  const overruns = []
  for (const relative of files) {
    const text = fsImpl.readFileSync(path.join(repoRoot, ...relative.split('/')), 'utf8')
    offenders.push(...findJobsWithoutTimeout(text).map((o) => ({ ...o, file: relative })))
    overruns.push(...findJobsWithOverrunStepCaps(text).map((o) => ({ ...o, file: relative })))
  }
  return { files, offenders, overruns }
}

export function renderVerdict({ files, offenders, overruns = [] }) {
  if (files.length === 0) {
    return { ok: false, text: 'No workflow files found: zero files read is not a pass.' }
  }
  const text = []
  if (offenders.length > 0) {
    text.push(
      `Job(s) without timeout-minutes (${files.length} workflow files read):`,
      ...offenders.map((o) => `  - ${o.file}:${o.line} job \`${o.job}\``),
      '',
      'Add `timeout-minutes:` to each job, about 2-3x its p99 duration, so a wedged run stops',
      'billing at a known ceiling instead of the 360-minute default.',
    )
  }
  if (overruns.length > 0) {
    if (text.length > 0) text.push('')
    text.push(
      `Job(s) whose step caps reach the job cap (${files.length} workflow files read):`,
      ...overruns.map(
        (o) =>
          `  - ${o.file}:${o.line} job \`${o.job}\`: steps ${o.stepMinutes} min >= job ${o.jobMinutes} min`,
      ),
      '',
      'Raise the job `timeout-minutes` above the sum of its step caps plus setup, or lower a',
      'step cap, so a step that runs long fails by name instead of the whole job being cancelled.',
    )
  }
  if (text.length > 0) return { ok: false, text: text.join('\n') }
  return {
    ok: true,
    text: `Every job declares timeout-minutes and fits its step caps (${files.length} workflow files read).`,
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
