// Proof sections come from repository extensions or core packs. Repo declarations override packs
// by id and come first; trusted base config may hide either. A PR cannot rewrite its instructions.
import fs from 'node:fs'
import path from 'node:path'

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
/** Known packs may be hidden even when the running core ships no section extensions. */
export const PACK_SECTION_IDS = Object.freeze(['testing', 'deploy'])

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

/** Section ids hidden by literal false in the trusted base config. @param {unknown} config */
export function hiddenSectionIds(config) {
  const defaults = isRecord(config) ? config.proofDefaults : undefined
  const sections = isRecord(defaults) ? defaults.sections : undefined
  return isRecord(sections)
    ? Object.keys(sections).filter((id) => SECTION_ID.test(id) && sections[id] === false)
    : []
}

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
 * Repo sections precede built-ins, preserving each group's discovery order. Repo ids silently
 * replace packs; other collisions are named in `problems`. Hidden ids remove either source.
 * @param {{name: string, builtin?: boolean, sections?: SectionDeclaration[], instructions?: string}[]} extensions
 * @param {{hidden?: string[]}} options
 * @returns {{sections: DeclaredSection[], problems: string[]}}
 */
export function readProofSections(extensions, { hidden = [] } = {}) {
  /** @type {DeclaredSection[]} */
  const sections = []
  /** @type {string[]} */
  const problems = []
  const repoIds = new Set()
  const ordered = [
    ...extensions.filter((extension) => !extension.builtin),
    ...extensions.filter((extension) => extension.builtin),
  ]
  for (const extension of ordered)
    for (const section of extension.sections ?? []) {
      if (extension.builtin && repoIds.has(section.id)) continue
      if (sections.some((kept) => kept.id === section.id)) {
        problems.push(`sections: ${extension.name} declares ${section.id} again, dropped`)
        continue
      }
      sections.push({ ...section, instructions: extension.instructions ?? '' })
      if (!extension.builtin) repoIds.add(section.id)
    }
  for (const id of hidden)
    if (!sections.some((section) => section.id === id) && !PACK_SECTION_IDS.includes(id))
      problems.push(`sections: proofDefaults.sections hides ${id}, which nothing declares`)
  return { sections: sections.filter((section) => !hidden.includes(section.id)), problems }
}

/** @param {string} root @returns {unknown} */
function loadBaseConfig(root) {
  try {
    return JSON.parse(fs.readFileSync(path.join(root, '.boss-skills.json'), 'utf8'))
  } catch (error) {
    if (error?.code === 'ENOENT') return {}
    throw error
  }
}

/**
 * The sections the trusted tree for `baseSha` declares. `discover` is the extension discovery
 * (`discoverExtensions`); the tree is never the PR head. Never throws: an unavailable tree
 * declares nothing and says so in `problems`.
 * @param {{baseSha: string, cwd?: string, read?: unknown, trustedTree: (options: {baseSha: string, cwd?: string, read?: any}) => {ok: boolean, root?: string} | null | undefined,
 *   discover: (options: {core: string, role: string, root: string}) => {extensions?: any[]}, loadConfig?: (root: string) => unknown}} options
 * @returns {{sections: DeclaredSection[], problems: string[]}}
 */
export function baseProofSections({
  baseSha,
  cwd,
  read,
  trustedTree,
  discover,
  loadConfig = loadBaseConfig,
}) {
  try {
    const trusted = trustedTree({ baseSha, cwd, read })
    if (!trusted?.ok || !trusted.root)
      return { sections: [], problems: ['sections: the base tree is unavailable'] }
    const { extensions = [] } = discover({
      core: 'boss-verify',
      role: SECTION_ROLE,
      root: trusted.root,
    })
    /** @type {unknown} */
    let config = {}
    const problems = []
    try {
      config = loadConfig(trusted.root)
    } catch {
      problems.push('sections: the base config could not be read')
    }
    const result = readProofSections(extensions, { hidden: hiddenSectionIds(config) })
    return { sections: result.sections, problems: [...result.problems, ...problems] }
  } catch {
    return { sections: [], problems: ['sections: the base tree could not be read'] }
  }
}
