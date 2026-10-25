#!/usr/bin/env node
// Stable mistake-class identities and machine-readable retrospective outcomes.
import fs from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { clusterNotes } from './retro-notes.mjs'
import { DEFAULT_MIN_RUNS, distinctRunCount } from './retro-ladder.mjs'

/** @typedef {{key: string, title?: string, statement?: string, sourceKeys?: string[], notes: Array<{id?: string, body?: string, created_at?: string, run_id?: string}>, ladder?: {rung?: string, target?: string, existingCheck?: string, instances?: number}}} Theme */
/** @typedef {{cluster: Theme, reason?: string}} Entry */
/** @typedef {{classKey: string, aliases: string[], title: string, runs: number, notes: Theme['notes'], rung: string|null, target: string|null, existingCheck: string|null, instances: number, eligible: boolean, covered: boolean}} RetroClass */
/** @typedef {{classKey: string, pr: string, remedy: string}} Promotion */

/**
 * Keep the oldest mechanical member's identity even when the theme's lexical key changes.
 * Currency includes retired themes; selection supplies the final live notes and tracking status.
 * @param {Record<string, Entry[]>} selection
 * @param {Record<string, Entry[]>} currency
 * @param {{minRuns?: number}} options
 * @returns {RetroClass[]}
 */
export function classify(selection, currency, { minRuns = DEFAULT_MIN_RUNS } = {}) {
  if (!Number.isSafeInteger(minRuns) || minRuns < 1)
    throw new Error('min-runs requires a positive integer')
  const selected = new Map(
    Object.values(selection)
      .flat()
      .map((entry) => [entry.cluster.key, entry]),
  )
  return Object.entries(currency).flatMap(([verdict, entries]) =>
    entries.map((entry) => {
      const final = selected.get(entry.cluster.key)
      const cluster = final?.cluster ?? entry.cluster
      const members = clusterNotes(cluster.notes).sort((a, b) => {
        const first = Date.parse(a.notes[0]?.created_at)
        const second = Date.parse(b.notes[0]?.created_at)
        return (
          (Number.isFinite(first) ? first : Infinity) -
            (Number.isFinite(second) ? second : Infinity) || a.key.localeCompare(b.key)
        )
      })
      const aliases = [
        ...new Set([
          cluster.key,
          ...(cluster.sourceKeys ?? []),
          ...members.map((member) => member.key),
        ]),
      ].sort()
      const runs = distinctRunCount(cluster.notes)
      return {
        classKey: members[0]?.key ?? cluster.key,
        aliases,
        title: cluster.title ?? cluster.statement ?? cluster.key,
        runs,
        notes: cluster.notes,
        rung: cluster.ladder?.rung ?? null,
        target: cluster.ladder?.target ?? null,
        existingCheck: cluster.ladder?.existingCheck ?? null,
        instances: cluster.ladder?.instances ?? 0,
        covered: final?.reason === 'already-tracked',
        eligible: verdict === 'live' && runs >= minRuns,
      }
    }),
  )
}

/**
 * @param {Array<Pick<RetroClass, 'classKey'|'runs'|'eligible'> & Partial<RetroClass>>} classes
 * @param {Array<Promotion | {promotions?: Promotion[], tracked?: Array<{classKey: string}>}>} results
 */
export function report(classes, results = []) {
  const coveredKeys = new Set(
    results
      .flatMap((result) => ('tracked' in result ? (result.tracked ?? []) : []))
      .map((claim) => claim.classKey),
  )
  const counts = {
    classes: classes.length,
    byRuns: {
      1: classes.filter((entry) => entry.runs < 2).length,
      '2+': classes.filter((entry) => entry.runs >= 2).length,
    },
    covered: classes.filter((entry) => entry.covered || coveredKeys.has(entry.classKey)).length,
    eligible: classes.filter((entry) => entry.eligible).length,
  }
  const promotions = results
    .flatMap((result) => ('classKey' in result ? [result] : (result.promotions ?? [])))
    .map((promotion) => {
      const entry = classes.find(
        (entry) =>
          entry.classKey === promotion.classKey || entry.aliases?.includes(promotion.classKey),
      )
      if (!entry) throw new Error(`unknown promoted class: ${promotion.classKey}`)
      return {
        classKey: entry.classKey,
        title: entry.title,
        pr: promotion.pr,
        remedy: promotion.remedy,
      }
    })
  return promotions.length
    ? { outcome: 'promoted', promotions, counts }
    : { outcome: 'nothing-seen-twice', counts }
}

/** @param {string[]} argv */
export function runCli(argv) {
  const [command, ...args] = argv
  const { values, positionals } = parseArgs({
    args,
    strict: true,
    allowPositionals: true,
    options:
      command === 'classify'
        ? { 'min-runs': { type: 'string' } }
        : { promotions: { type: 'string' } },
  })
  const read = (file) => JSON.parse(fs.readFileSync(file, 'utf8'))
  if (command === 'classify' && positionals.length === 2) {
    const minRuns = values['min-runs'] === undefined ? DEFAULT_MIN_RUNS : Number(values['min-runs'])
    if (!Number.isSafeInteger(minRuns) || minRuns < 1)
      throw new Error('min-runs requires a positive integer')
    return classify(read(positionals[0]), read(positionals[1]), { minRuns })
  }
  if (command === 'report' && positionals.length === 1)
    return report(read(positionals[0]), values.promotions ? read(values.promotions) : [])
  throw new Error(
    'usage: classify <selection.json> <currency.json> [--min-runs N] | report <classes.json> [--promotions <results.json>]',
  )
}
if (isMainModule(import.meta.url)) {
  try {
    process.stdout.write(JSON.stringify(runCli(process.argv.slice(2))) + '\n')
  } catch (error) {
    process.stderr.write(`retro-classes: ${error.message}\n`)
    process.exitCode = 1
  }
}
