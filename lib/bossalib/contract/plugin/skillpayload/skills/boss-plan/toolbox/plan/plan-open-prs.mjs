#!/usr/bin/env node

import { readFileSync, writeFileSync } from 'node:fs'
import { execFileSync } from 'node:child_process'
import { parseArgs } from 'node:util'
import { areasOverlap, extractKeyChangeAreas, withScanDefaults } from './plan-deps-lib.mjs'
import { runGh } from '../tracker/github-gh.mjs'
import { FALLBACK_LABEL_PREFIX, GH_USAGE_LABEL_ENV } from '../pr-state.mjs'
import { loadSkillConfig } from '../skill-config.mjs'
import { isMainModule } from '../main-module.mjs'

/** @typedef {{number: number, url: string, title: string, headRefName: string, isDraft: boolean,
 * files: string[], filesTruncated: boolean, ticketIds: string[]}} OpenPullRequest
 * @typedef {{listTruncated: boolean, pullRequests: OpenPullRequest[]}} OpenPrsSnapshot
 * @typedef {{severity: string, destination: string, candidate: string, reason: string,
 * text: string, pr?: number, level?: string, shared?: string[]}} OverlapNote
 */

export const OPEN_PR_REASONS = Object.freeze([
  'open-pr-overlap',
  'open-pr-ticket-unfetched',
  'open-prs-truncated',
])

/** @param {{title: string, headRefName: string}} input */
export function parseTicketIds({ title, headRefName }) {
  return [
    ...new Set(
      `${title} ${headRefName}`
        .match(/\b[A-Za-z][A-Za-z0-9]*-\d+\b/g)
        ?.map((id) => id.toUpperCase()) ?? [],
    ),
  ]
}

/** @param {{limit?: number, gh?: typeof runGh, env?: object}} [options]
 * @returns {OpenPrsSnapshot} */
export function fetchOpenPullRequests({ limit = 100, gh = runGh, env = process.env } = {}) {
  const rows = gh(
    [
      'pr',
      'list',
      '--state',
      'open',
      '--limit',
      String(limit),
      '--json',
      'number,title,url,headRefName,isDraft,files',
    ],
    {
      env: { ...env, [GH_USAGE_LABEL_ENV]: `${FALLBACK_LABEL_PREFIX}plan-open-prs` },
    },
  )
  return {
    listTruncated: rows.length >= limit,
    pullRequests: rows.map((row) => ({
      number: row.number,
      url: row.url,
      title: row.title,
      headRefName: row.headRefName,
      isDraft: row.isDraft,
      files: row.files.map((file) => file.path),
      filesTruncated: row.files.length >= 100,
      ticketIds: parseTicketIds(row),
    })),
  }
}

/** @param {unknown} value @returns {string|null} */
export function validateOpenPrsSnapshot(value) {
  if (
    !value ||
    typeof value !== 'object' ||
    !('listTruncated' in value) ||
    typeof value.listTruncated !== 'boolean' ||
    !('pullRequests' in value) ||
    !Array.isArray(value.pullRequests)
  )
    return 'expected listTruncated and pullRequests'
  for (const row of value.pullRequests) {
    if (
      !row ||
      typeof row !== 'object' ||
      !Number.isInteger(row.number) ||
      row.number < 1 ||
      ['url', 'title', 'headRefName'].some((key) => typeof row[key] !== 'string') ||
      typeof row.isDraft !== 'boolean' ||
      typeof row.filesTruncated !== 'boolean' ||
      !Array.isArray(row.files) ||
      row.files.some((file) => typeof file !== 'string' || !file.trim()) ||
      !Array.isArray(row.ticketIds) ||
      row.ticketIds.some((id) => typeof id !== 'string' || !/^[A-Z][A-Z0-9]*-\d+$/.test(id))
    ) {
      return 'a pullRequests row is not plan-open-prs output'
    }
  }
  return null
}

/** @param {OpenPullRequest} pr @param {string[]} subjectAreas @param {object} options */
function prOverlap(pr, subjectAreas, options) {
  return areasOverlap(subjectAreas, pr.files, options)
}

/** @param {OpenPullRequest} pr @param {{shared: string[], fileShared: string[]}} overlap
 * @param {string} candidate @returns {OverlapNote} */
function overlapNote(pr, overlap, candidate) {
  const level = overlap.fileShared.length ? 'file' : 'directory'
  return {
    severity: candidate ? 'info' : 'warning',
    destination: candidate ? 'planning' : 'risks',
    candidate,
    reason: 'open-pr-overlap',
    pr: pr.number,
    level,
    shared: overlap.shared,
    text: `Open PR #${pr.number} (${pr.url})${candidate ? ` for ${candidate}` : ''} overlaps at ${level} level (shared: ${overlap.shared.join(', ')}).`,
  }
}

/** @param {string[]} ids @param {string} subjectIdentifier */
function ticketIdsForSubject(ids, subjectIdentifier) {
  const prefix = subjectIdentifier.toUpperCase().split('-')[0]
  return ids.filter((id) => id.split('-')[0] === prefix)
}

/** Pure classification: mapped PRs augment existing ticket comparisons; unmapped PRs only warn.
 * @param {{subjectIdentifier: string, subjectAreas: string[], snapshot: OpenPrsSnapshot,
 * candidateIdentifiers: string[], overlapOptions?: object}} input */
export function openPullRequestOverlaps({
  subjectIdentifier,
  subjectAreas,
  snapshot,
  candidateIdentifiers,
  overlapOptions = {},
}) {
  const subject = subjectIdentifier.toUpperCase()
  const candidates = new Map(candidateIdentifiers.map((id) => [id.toUpperCase(), id]))
  /** @type {Record<string, string[]>} */
  const foldIn = {}
  /** @type {OverlapNote[]} */
  const notes = []
  /** @type {{id: string, reason: string, pr: number}[]} */
  const skipped = []
  let found = false
  for (const pr of snapshot.pullRequests) {
    const ids = ticketIdsForSubject(pr.ticketIds, subjectIdentifier)
    if (ids.includes(subject)) continue
    const overlap = prOverlap(pr, subjectAreas, overlapOptions)
    if (!overlap.overlap) continue
    found = true
    const candidate = ids.map((id) => candidates.get(id)).find(Boolean)
    if (candidate) {
      foldIn[candidate] = [...new Set([...(foldIn[candidate] ?? []), ...pr.files])]
      notes.push(overlapNote(pr, overlap, candidate))
    } else if (ids.length) {
      skipped.push({ id: ids[0], reason: 'open-pr-ticket-unfetched', pr: pr.number })
    } else {
      notes.push(overlapNote(pr, overlap, ''))
    }
  }
  if (!found && (snapshot.listTruncated || snapshot.pullRequests.some((pr) => pr.filesTruncated))) {
    notes.push({
      severity: 'warning',
      destination: 'risks',
      candidate: subjectIdentifier,
      reason: 'open-prs-truncated',
      text: 'Open-PR overlap: no overlap found in a truncated snapshot; unseen PRs or files could overlap.',
    })
  }
  return { foldIn, notes, skipped }
}

/** A bad snapshot is a named refusal even when reading or parsing fails.
 * @param {string} file @returns {OpenPrsSnapshot} */
export function readOpenPrsSnapshot(file) {
  try {
    const value = JSON.parse(readFileSync(file, 'utf8'))
    const defect = validateOpenPrsSnapshot(value)
    if (defect) throw new Error(defect)
    return value
  } catch (error) {
    throw new Error(
      `open-prs-not-plan-open-prs: ${file}: ${error.message}; write it with plan-open-prs.mjs fetch --out-file ${file}`,
    )
  }
}

/** @param {string[]} argv */
export function openPrsFromArgs(argv) {
  const { values } = parseArgs({
    args: argv,
    strict: true,
    options: { subject: { type: 'string' }, 'open-prs': { type: 'string' } },
  })
  return values['open-prs'] ? readOpenPrsSnapshot(values['open-prs']) : undefined
}

/** Recon before a subject has a description, using the scan's own path extraction rules.
 * @param {{snapshot: OpenPrsSnapshot, subject: string, paths: string[], candidates?: object[], config: object}} input */
export function reconOverlaps({ snapshot, subject, paths, candidates = [], config }) {
  const defaults = withScanDefaults(config, {})
  const options = {
    ...defaults,
    moduleRoots: [
      ...new Set([
        ...defaults.moduleRoots,
        ...paths.filter((p) => p.includes('/')).map((p) => p.split('/')[0]),
      ]),
    ],
    rootFiles: paths.filter((p) => !p.includes('/')),
  }
  const areas = extractKeyChangeAreas(
    config,
    `## Key changes\n\n${paths.map((p) => `- \`${p}\``).join('\n')}`,
    options,
  ).areas
  const classification = openPullRequestOverlaps({
    subjectIdentifier: subject,
    subjectAreas: areas,
    snapshot,
    candidateIdentifiers: candidates.map((row) => row.identifier ?? row.id),
    overlapOptions: options,
  })
  return {
    pullRequests: overlappingPullRequests({
      snapshot,
      subjectIdentifier: subject,
      subjectAreas: areas,
      overlapOptions: options,
    }),
    tickets: candidates.flatMap((row) => {
      if ([row.id, row.identifier].some((id) => id?.toUpperCase() === subject.toUpperCase()))
        return []
      const overlap = areasOverlap(
        areas,
        extractKeyChangeAreas(config, row.description, options).areas,
        options,
      )
      return overlap.overlap
        ? [
            {
              identifier: row.identifier ?? row.id,
              title: row.title,
              stateName: row.stateName,
              level: overlap.fileShared.length ? 'file' : 'directory',
              shared: overlap.shared,
            },
          ]
        : []
    }),
    notes: classification.notes,
  }
}

/** @param {string[]} argv */
export function runCli(argv) {
  let args
  try {
    args = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        'out-file': { type: 'string' },
        limit: { type: 'string' },
        'open-prs': { type: 'string' },
        subject: { type: 'string' },
        paths: { type: 'string' },
        candidates: { type: 'string' },
      },
    })
    const verb = args.positionals[0],
      values = args.values
    if (
      args.positionals.length !== 1 ||
      !['fetch', 'overlap'].includes(verb) ||
      (verb === 'fetch' &&
        (!values['out-file'] ||
          (values.limit !== undefined &&
            !/^(?:[1-9]\d?|1\d\d|2[0-4]\d|250)$/.test(values.limit)))) ||
      (verb === 'overlap' && (!values['open-prs'] || !values.subject || !values.paths?.trim()))
    )
      throw new Error('invalid arguments')
  } catch (error) {
    process.stderr.write(
      `usage: plan-open-prs.mjs fetch --out-file <file> [--limit <1-250>] | overlap --open-prs <file> --subject <ISSUE-ID> --paths <a,b> [--candidates <file>]: ${error.message}\n`,
    )
    return 2
  }
  try {
    const values = args.values
    const command = args.positionals[0]
    if (command === 'fetch') {
      const snapshot = fetchOpenPullRequests({ limit: values.limit ? Number(values.limit) : 100 })
      writeFileSync(values['out-file'], `${JSON.stringify(snapshot)}\n`)
      process.stdout.write(
        `${JSON.stringify({
          count: snapshot.pullRequests.length,
          listTruncated: snapshot.listTruncated,
          filesTruncatedCount: snapshot.pullRequests.filter((pr) => pr.filesTruncated).length,
        })}\n`,
      )
    } else if (command === 'overlap') {
      const result = reconOverlaps({
        snapshot: readOpenPrsSnapshot(values['open-prs']),
        subject: values.subject,
        paths: values.paths
          .split(',')
          .map((p) => p.trim())
          .filter(Boolean),
        candidates: values.candidates ? JSON.parse(readFileSync(values.candidates, 'utf8')) : [],
        config: loadSkillConfig(),
      })
      process.stdout.write(`${JSON.stringify(result)}\n`)
    }
    return 0
  } catch (error) {
    process.stderr.write(`${error.code ? `${error.code}: ` : ''}${error.message}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url)) process.exitCode = runCli(process.argv.slice(2))

/**
 * The repo's committed entries: `dirs` (root trees and submodules) are module
 * roots, `files` (blobs) are root files. Split because a root FILE needs no extension to be an area
 * and an unmarked mention of one is reported rather than dropped (`extractKeyChangeAreas`
 * `rootFiles`), while a directory keeps the module-root rules. `knownFiles` includes nested blobs
 * so shared extensionless files can establish an advisory relation.
 */
export function listRepoRootEntries(cwd = process.cwd()) {
  const out = execFileSync('git', ['ls-tree', '-r', '-t', '-z', 'HEAD'], {
    cwd,
    encoding: 'utf8',
    maxBuffer: 16 * 1024 * 1024,
  })
  const dirs = []
  const files = []
  const knownFiles = []
  for (const record of out.split('\0')) {
    const tab = record.indexOf('\t')
    if (tab === -1) continue
    const type = record.slice(0, tab).split(' ')[1]
    const name = record.slice(tab + 1)
    if (name === '') continue
    if (type === 'blob') knownFiles.push(name)
    if (name.includes('/')) continue
    if (type === 'blob') files.push(name)
    else dirs.push(name)
  }
  return { dirs, files, knownFiles }
}

/** Per-PR evidence shared by recon and build.
 * @param {{snapshot: OpenPrsSnapshot, subjectIdentifier: string, subjectAreas: string[], overlapOptions?: object}} input */
export function overlappingPullRequests({
  snapshot,
  subjectIdentifier,
  subjectAreas,
  overlapOptions = {},
}) {
  return snapshot.pullRequests.flatMap((pr) => {
    if (pr.ticketIds.includes(subjectIdentifier.toUpperCase())) return []
    const overlap = prOverlap(pr, subjectAreas, overlapOptions)
    return overlap.overlap
      ? [
          {
            number: pr.number,
            url: pr.url,
            title: pr.title,
            ticketIds: ticketIdsForSubject(pr.ticketIds, subjectIdentifier),
            level: overlap.fileShared.length ? 'file' : 'directory',
            shared: overlap.shared,
          },
        ]
      : []
  })
}
