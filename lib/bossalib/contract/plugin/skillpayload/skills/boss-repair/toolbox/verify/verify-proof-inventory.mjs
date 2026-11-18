// What a proof page carries, as a list two renders can be compared by: the area items, the
// typed blocks, the banner signals, the sections and the media. `render-pr --inventory` writes it
// from the model the page's HTML is rendered from; `verify-proof-page.mjs inventory-diff` compares
// two of them and fails when the second lost anything the first had.
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { mediaName } from './verify-proof-media.mjs'

export const INVENTORY_SCHEMA = 'proof-inventory/1'
export const INVENTORY_DIFF_USAGE =
  'Usage: verify-proof-page.mjs inventory-diff --before <inventory> --after <inventory>'

/**
 * @typedef {import('./verify-proof-page-model.mjs').PageModel} PageModel
 * @typedef {import('./verify-describe-blocks.mjs').Block} Block
 * @typedef {{kind: string, title: string}} InventoryBlock
 * @typedef {{schema: string, items: string[], blocks: InventoryBlock[], signals: string[],
 *   sections: string[], clips: string[]}} ProofInventory
 * @typedef {{lost: string[], reworded: string[]}} InventoryDiff
 */

/** The name a block goes by on the page. @param {Block} block */
function blockTitle(block) {
  switch (block.kind) {
    case 'code':
      return block.title ?? `${block.path}:${block.start}`
    case 'route':
      return `${block.method} ${block.path}`
    case 'table':
    case 'client-call':
      return block.name
    case 'schema':
      return block.types.map((type) => type.name).join(', ')
    default:
      return block.kind
  }
}

/**
 * What the page built from `model` carries. Area items (the facts each area section lists, which
 * are its describer items unless a spec wrote its own) and blocks come in page order. A describer's
 * `steps` blocks render as the After merge list, so each After merge entry counts as one `steps`
 * block. Sections are the page's anchor ids: `a-<area>` and `sec-<declared section>`.
 * @param {PageModel} model @returns {ProofInventory}
 */
export function proofInventory(model) {
  const ev = model.evidence
  const files = [
    ...ev.heads.flatMap(({ clip }) => [clip.file, ...clip.stills.map((still) => still.file)]),
    ...ev.pairs.flatMap((pair) => [pair.base.file, pair.head.file]),
    ...ev.documents.flatMap((pair) =>
      [pair.head, ...(pair.base ? [pair.base] : [])].flatMap((doc) => [
        doc.file,
        ...doc.pages.map((page) => page.file),
      ]),
    ),
  ]
  return {
    schema: INVENTORY_SCHEMA,
    items: model.areas.flatMap((area) => area.facts.map((fact) => fact.text)),
    blocks: [
      ...model.areas.flatMap((area) =>
        area.blocks.map((block) => ({ kind: block.kind, title: blockTitle(block) })),
      ),
      ...model.afterMerge.map((tick) => ({ kind: 'steps', title: tick.text })),
    ],
    signals: model.banner.signals.map((signal) => signal.label),
    sections: [
      ...model.areas.map((area) => `a-${area.id}`),
      ...model.repoSections.map((section) => `sec-${section.id}`),
    ],
    clips: [...new Set(files.map(mediaName))],
  }
}

/** @param {string[]} from @param {string[]} other */
const onlyIn = (from, other) => {
  const present = new Set(other)
  return [...new Set(from)].filter((value) => !present.has(value))
}

/** @param {InventoryBlock[]} blocks */
const kindCounts = (blocks) => {
  /** @type {Map<string, number>} */
  const counts = new Map()
  for (const block of blocks) counts.set(block.kind, (counts.get(block.kind) ?? 0) + 1)
  return counts
}

/**
 * What `after` lost against `before`, and what it only words differently. Counts decide loss for
 * items and blocks, since a swapped describer may word the same fact another way; a signal, section
 * or clip is lost when its name is gone.
 * @param {ProofInventory} before @param {ProofInventory} after @returns {InventoryDiff}
 */
export function compareInventories(before, after) {
  /** @type {string[]} */
  const lost = []
  if (after.items.length < before.items.length)
    lost.push(`items: ${before.items.length} before, ${after.items.length} after`)
  const afterKinds = kindCounts(after.blocks)
  for (const [kind, count] of kindCounts(before.blocks)) {
    const now = afterKinds.get(kind) ?? 0
    if (now < count) lost.push(`${kind} blocks: ${count} before, ${now} after`)
  }
  for (const key of /** @type {const} */ (['signals', 'sections', 'clips']))
    for (const name of onlyIn(before[key], after[key])) lost.push(`${key.slice(0, -1)}: ${name}`)
  const titles = (/** @type {ProofInventory} */ inv) =>
    inv.blocks.map((block) => `${block.kind} ${block.title}`)
  const reworded = [
    ...onlyIn(before.items, after.items).map((text) => `item before only: ${text}`),
    ...onlyIn(after.items, before.items).map((text) => `item after only: ${text}`),
    ...onlyIn(titles(before), titles(after)).map((title) => `block before only: ${title}`),
    ...onlyIn(titles(after), titles(before)).map((title) => `block after only: ${title}`),
  ]
  return { lost, reworded }
}

/** A `proof-inventory/1` file, or an error naming it. @param {string} file @returns {ProofInventory} */
export function readInventory(file) {
  let value
  try {
    value = JSON.parse(readFileSync(file, 'utf8'))
  } catch (error) {
    throw new Error(`${file}: ${error instanceof Error ? error.message : String(error)}`)
  }
  const strings = (/** @type {unknown} */ list) =>
    Array.isArray(list) && list.every((entry) => typeof entry === 'string')
  const ok =
    value?.schema === INVENTORY_SCHEMA &&
    ['items', 'signals', 'sections', 'clips'].every((key) => strings(value[key])) &&
    Array.isArray(value.blocks) &&
    value.blocks.every(
      (/** @type {any} */ block) =>
        typeof block?.kind === 'string' && typeof block?.title === 'string',
    )
  if (!ok) throw new Error(`${file}: not a ${INVENTORY_SCHEMA} file`)
  return value
}

/**
 * The `inventory-diff` verb: prints each loss, then each wording difference, one per line, and
 * returns 1 when anything was lost, else 0. Unreadable input throws.
 * @param {string[]} args the arguments after the verb
 * @param {{write?: (text: string) => void}} [io]
 */
export function inventoryDiffCli(args, { write = (text) => process.stdout.write(text) } = {}) {
  const { values } = parseArgs({
    args,
    strict: true,
    options: { before: { type: 'string' }, after: { type: 'string' } },
  })
  if (!values.before || !values.after) throw new Error(INVENTORY_DIFF_USAGE)
  const { lost, reworded } = compareInventories(
    readInventory(values.before),
    readInventory(values.after),
  )
  const lines = [
    ...(lost.length ? lost.map((line) => `lost ${line}`) : ['nothing lost']),
    ...reworded.map((line) => `reworded ${line}`),
  ]
  write(`${lines.join('\n')}\n`)
  return lost.length ? 1 : 0
}
