// The `proof-section` extension role: a repository declares proof-page sections of its own in
// its extension's marker, and the extension's body tells the proof author what goes in them. The
// core knows only the generic parts a section is filled from (see `validateProofSpec`); what a
// section is about is the repository's business. Like every proof extension, the declaration is
// read only from the trusted base tree, so a PR can never rewrite its own instructions.
export const SECTION_ROLE = 'proof-section'
export const SECTION_ID = /^[a-z][a-z0-9-]{1,23}$/
export const SECTION_TITLE_CHARS = 40
export const SECTION_INSTRUCTIONS_CHARS = 12000
/** The core section anchors a repository section may follow, in page order; `a-<area>` is any area's. */
export const CORE_ANCHORS = Object.freeze([
  'risks',
  'decide',
  'ticket',
  'evidence',
  'findings',
  'after',
  'files',
])
export const AREA_ANCHOR = /^a-[a-z0-9][a-z0-9:-]*$/

/**
 * @typedef {{id: string, title: string, after?: string}} SectionDeclaration
 * @typedef {SectionDeclaration & {instructions: string}} DeclaredSection
 */

/** An extension body as the author's instructions: trimmed, at most 12,000 characters. @param {string} body */
export const sectionInstructions = (body) => {
  const text = body.trim()
  return text.length > SECTION_INSTRUCTIONS_CHARS
    ? `${text.slice(0, SECTION_INSTRUCTIONS_CHARS - 1)}…`
    : text
}

/** @param {unknown} value @returns {value is Record<string, unknown>} */
const isRecord = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

/**
 * A marker's `sections` list: each entry a valid `id` and a one-line `title` of at most 40
 * characters, the ids unique. An `after` that names no core anchor is left out, which places the
 * section just before Files. Null when the list is absent, empty or has an invalid entry.
 * @param {unknown} value @returns {SectionDeclaration[] | null}
 */
export function parseSectionsMarker(value) {
  if (!Array.isArray(value) || value.length === 0) return null
  /** @type {SectionDeclaration[]} */
  const sections = []
  for (const entry of value) {
    if (!isRecord(entry)) return null
    const { id, title, after } = entry
    if (typeof id !== 'string' || !SECTION_ID.test(id)) return null
    if (sections.some((section) => section.id === id)) return null
    if (
      typeof title !== 'string' ||
      !title.trim() ||
      title.length > SECTION_TITLE_CHARS ||
      /[\r\n]/.test(title)
    )
      return null
    const known =
      typeof after === 'string' && (CORE_ANCHORS.includes(after) || AREA_ANCHOR.test(after))
    sections.push({ id, title: title.trim(), ...(known ? { after } : {}) })
  }
  return sections
}

/**
 * The sections the discovered `proof-section` extensions declare, in discovery order, each with
 * its extension's instructions. A section whose id an earlier extension already declared is
 * dropped and named in `problems`.
 * @param {{name: string, sections?: SectionDeclaration[], instructions?: string}[]} extensions
 * @returns {{sections: DeclaredSection[], problems: string[]}}
 */
export function readProofSections(extensions) {
  /** @type {DeclaredSection[]} */
  const sections = []
  /** @type {string[]} */
  const problems = []
  for (const extension of extensions)
    for (const section of extension.sections ?? []) {
      if (sections.some((kept) => kept.id === section.id)) {
        problems.push(`sections: ${extension.name} declares ${section.id} again, dropped`)
        continue
      }
      sections.push({ ...section, instructions: extension.instructions ?? '' })
    }
  return { sections, problems }
}

/**
 * The sections the trusted tree for `baseSha` declares. `discover` is the extension discovery
 * (`discoverExtensions`); the tree is never the PR head. Never throws: an unavailable tree
 * declares nothing and says so in `problems`.
 * @param {{baseSha: string, cwd?: string, read?: unknown, trustedTree: (options: {baseSha: string, cwd?: string, read?: any}) => {ok: boolean, root?: string} | null | undefined,
 *   discover: (options: {core: string, role: string, root: string}) => {extensions?: any[]}}} options
 * @returns {{sections: DeclaredSection[], problems: string[]}}
 */
export function baseProofSections({ baseSha, cwd, read, trustedTree, discover }) {
  try {
    const trusted = trustedTree({ baseSha, cwd, read })
    if (!trusted?.ok || !trusted.root)
      return { sections: [], problems: ['sections: the base tree is unavailable'] }
    const { extensions = [] } = discover({
      core: 'boss-verify',
      role: SECTION_ROLE,
      root: trusted.root,
    })
    return readProofSections(extensions)
  } catch {
    return { sections: [], problems: ['sections: the base tree could not be read'] }
  }
}
