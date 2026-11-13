// The page side of repository sections: each section the base tree declares, filled from the
// proof author's spec, as the page model carries it. `verify-proof-sections.mjs` is the discovery
// side, which the extension reader imports, so this half stays out of the toolboxes that only
// discover extensions.

/**
 * @typedef {import('./verify-proof-sections.mjs').SectionDeclaration} SectionDeclaration
 * @typedef {import('./verify-proof-page-model.mjs').Pill} Pill
 * @typedef {import('./verify-proof-page-model.mjs').Link} Link
 * @typedef {{id: string, ref: string, text: string, context?: string, expect?: string, group?: string, tag?: Pill, links: Link[]}} SectionTick
 *   `group` is set on the first item of each run of one group, which the label column shows.
 * @typedef {{kind: 'checklist', numbered: boolean, grouped: boolean, items: SectionTick[]}
 *   | {kind: 'table', title?: string, note?: string, columns: string[], pills: boolean[], rows: {text: string, note?: string, pill?: string, code?: boolean}[][]}
 *   | {kind: 'facts', title?: string, rows: {label: string, text: string, tone?: string}[]}} SectionPart
 *   A table's `pills` says which columns hold only pills, which the page aligns right.
 * @typedef {{id: string, title: string, after: string | null, sub?: string, count: number, open: number, parts: SectionPart[]}} RepoSection
 * @typedef {Omit<SectionTick, 'links'> & {specLinks: string[]}} DraftTick
 * @typedef {Exclude<SectionPart, {kind: 'checklist'}> | {kind: 'checklist', numbered: boolean, grouped: boolean, items: DraftTick[]}} DraftPart
 * @typedef {Omit<RepoSection, 'parts'> & {parts: DraftPart[]}} DraftSection
 */

/** A tone the page has no colour for reads as muted. @param {string | undefined} tone */
const toneOf = (tone) => (tone === undefined || tone === 'neutral' ? 'muted' : tone)

/**
 * The page's repository sections: each declared section the spec fills, in declaration order,
 * with its count (checklist items, else rows). A checklist item's id is `sec-<section>-<item>`,
 * which is its anchor and its tick key; its `specLinks` are joined to page items by the caller.
 * @param {SectionDeclaration[]} declared
 * @param {import('./verify-proof-spec.mjs').SpecSection[]} filled
 * @returns {DraftSection[]}
 */
export function fillSections(declared, filled) {
  return declared.flatMap((declaration) => {
    const section = filled.find((entry) => entry.id === declaration.id)
    if (!section) return []
    /** @type {DraftPart[]} */
    const parts = section.parts.map((part) => {
      if (part.kind === 'table')
        return {
          ...part,
          pills: part.columns.map((_, i) => part.rows.every((row) => row.cells[i].pill)),
          rows: part.rows.map((row) =>
            row.cells.map((cell) => (cell.pill ? { ...cell, pill: toneOf(cell.pill) } : cell)),
          ),
        }
      if (part.kind === 'facts')
        return {
          ...part,
          rows: part.rows.map((row) => (row.tone ? { ...row, tone: toneOf(row.tone) } : row)),
        }
      let last = /** @type {string | undefined} */ (undefined)
      return {
        kind: /** @type {const} */ ('checklist'),
        numbered: part.numbered,
        grouped: part.items.some((item) => item.group),
        items: part.items.map((item) => {
          const first = item.group !== last
          last = item.group
          return {
            id: `sec-${section.id}-${item.id.toLowerCase().replace(/[^a-z0-9]+/g, '-')}`,
            ref: item.id,
            text: item.text,
            ...(item.context ? { context: item.context } : {}),
            ...(item.expect ? { expect: item.expect } : {}),
            ...(item.group && first ? { group: item.group } : {}),
            ...(item.tag ? { tag: { word: item.tag, tone: toneOf(item.tone) } } : {}),
            specLinks: item.links,
          }
        }),
      }
    })
    const ticks = parts.reduce(
      (n, part) => n + (part.kind === 'checklist' ? part.items.length : 0),
      0,
    )
    const rows = parts.reduce(
      (n, part) => n + (part.kind === 'checklist' ? 0 : part.rows.length),
      0,
    )
    return [
      {
        id: declaration.id,
        title: declaration.title,
        after: declaration.after ?? null,
        ...(section.sub ? { sub: section.sub } : {}),
        count: ticks || rows,
        open: ticks,
        parts,
      },
    ]
  })
}
