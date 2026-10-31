#!/usr/bin/env node

import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import {
  blockerVerdict,
  classifyBlocker,
  prUrlsOf,
  dependentPathsFrom,
  softClearedBlockers,
} from '../tracker/blocker-clearance.mjs'
import { loadSkillConfig, trackerConfigFor, keyChangesSection } from '../skill-config.mjs'
import { createPrLookup } from './blocker-pr-facts.mjs'

/** @typedef {{ id: string, state?: { name?: string, type?: string }, attachments?: { nodes?: { url?: string }[] } }} Blocker */
/** @typedef {{ state?: string, isDraft?: boolean, mergedAt?: string|null, error?: string, green?: boolean, files?: string[]|null }} PrFacts */
/** @typedef {{ write: (text: string) => unknown }} Output */
/**
 * Evaluate every requested candidate before writing output, so a failed read cannot
 * leave a partial report that looks like a successful queue assessment.
 * @param {string[]} argv
 * @param {{tracker?: {readBlockers?: (ids: string[], options?: {mode: string}) => Promise<{id: string, description?: string, blockers: Blocker[]}[]>}, prLookup?: (url: string) => Promise<PrFacts>, stdout?: Output, stderr?: Output}} [dependencies]
 * @returns {Promise<number>}
 */
export async function main(argv, dependencies = {}) {
  const stdout = dependencies.stdout ?? process.stdout
  const stderr = dependencies.stderr ?? process.stderr
  try {
    const { values } = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: false,
      options: {
        id: { type: 'string', multiple: true },
        summary: { type: 'boolean' },
        mode: { type: 'string', default: 'strict' },
      },
    })
    if (!['strict', 'selection'].includes(values.mode))
      throw new Error('--mode must be strict or selection')
    const config = loadSkillConfig()
    const inReviewState = trackerConfigFor(config).states.inReview
    const ids = [...new Set(values.id?.map((id) => id.trim()) ?? [])]
    if (ids.length === 0 || ids.some((id) => !id))
      throw new Error('at least one non-empty --id is required')
    const tracker = dependencies.tracker ?? resolveTrackerAdapter()
    if (typeof tracker.readBlockers !== 'function')
      throw new Error('tracker does not support readBlockers')
    const candidates = await tracker.readBlockers(ids, { mode: values.mode })
    if (!Array.isArray(candidates)) throw new Error('readBlockers did not return an array')
    const byId = new Map(candidates.map((candidate) => [candidate.id, candidate]))
    for (const id of ids) {
      if (!Array.isArray(byId.get(id)?.blockers))
        throw new Error(`readBlockers did not return blockers for ${id}`)
    }
    const lookup = dependencies.prLookup ?? createPrLookup()
    /** @type {Map<string, PrFacts>} */
    const prFacts = new Map()
    for (const id of ids) {
      for (const blocker of byId.get(id).blockers) {
        if (classifyBlocker(blocker).clearance !== 'needs-pr-facts') continue
        for (const url of prUrlsOf(blocker)) {
          if (prFacts.has(url)) continue
          try {
            prFacts.set(url, await lookup(url))
          } catch (error) {
            prFacts.set(url, { error: error instanceof Error ? error.message : String(error) })
          }
        }
      }
    }
    const rows = ids.map((id) => {
      const blockers = byId.get(id).blockers
      const options = {
        prFacts,
        mode: values.mode,
        inReviewState,
        dependentPaths: dependentPathsFrom(
          keyChangesSection(config, byId.get(id).description ?? ''),
        ),
      }
      return {
        id,
        verdict: blockerVerdict(blockers, options),
        softCleared: softClearedBlockers(blockers, options),
        blockers: blockers.map((blocker) => ({
          id: blocker.id,
          state: blocker.state ?? null,
          prs: prUrlsOf(blocker),
          ...classifyBlocker(blocker, options),
        })),
      }
    })
    for (const row of rows) stdout.write(`${JSON.stringify(row)}\n`)
    if (values.summary) {
      const gating = new Map()
      for (const row of rows) {
        for (const blocker of row.blockers) {
          if (['cleared', 'review-clearable'].includes(blocker.clearance)) continue
          const entry = gating.get(blocker.id) ?? {
            id: blocker.id,
            state: blocker.state,
            prs: blocker.prs,
            dependents: new Set(),
          }
          entry.dependents.add(row.id)
          gating.set(blocker.id, entry)
        }
      }
      const ranked = [...gating.values()]
        .map(({ dependents, ...blocker }) => ({
          ...blocker,
          dependentsHeld: dependents.size,
        }))
        .sort((a, b) => b.dependentsHeld - a.dependentsHeld || a.id.localeCompare(b.id))
      stdout.write(`${JSON.stringify({ gating: ranked })}\n`)
    }
    return 0
  } catch (error) {
    stderr.write(`blocker-verdict: ${error instanceof Error ? error.message : String(error)}\n`)
    return 2
  }
}

if (isMainModule(import.meta.url)) process.exitCode = await main(process.argv.slice(2))
