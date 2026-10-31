// Built-in proof-describe script: reports an area's renames and import-only edits. It never marks
// an area breaking; the area detector, not this describer, drops a pure move's ask.

import { isMainModule } from '../main-module.mjs'
import { readChangedFiles } from './verify-diff.mjs'
import { describerMain } from './verify-describe.mjs'
import { pureMoveSummary, renamePairs } from './verify-moves.mjs'

/** @typedef {import('./verify-diff.mjs').Run} Run */

const plural = (/** @type {number} */ n, /** @type {string} */ one) =>
  `${n} ${one}${n === 1 ? '' : 's'}`

/**
 * @param {{area: {paths: string[]}, baseSha: string, headSha: string}} envelope
 * @param {{cwd?: string, run?: Run}} [options]
 */
export function describeMoves(envelope, { cwd, run } = {}) {
  const files = readChangedFiles({ baseSha: envelope.baseSha, headSha: envelope.headSha, cwd, run })
  const paths = new Set(envelope.area.paths)
  const own = files.filter(
    (file) => paths.has(file.path) || (file.previousPath && paths.has(file.previousPath)),
  )
  const summary = pureMoveSummary(own, renamePairs(files))
  /** @type {string[]} */
  const items = []
  if (summary.moved === 0 && summary.importOnly === 0) return { items, breaking: false }
  if (summary.moved > 0) items.push(`${plural(summary.moved, 'file')} moved`)
  if (summary.importOnly > 0) items.push(`${plural(summary.importOnly, 'file')}: import paths only`)
  if (summary.other > 0)
    items.push(`Not a pure move: ${plural(summary.other, 'file')} change more than imports`)
  return { items, breaking: false }
}

export const main = () => describerMain(describeMoves)

if (isMainModule(import.meta.url)) await main()
