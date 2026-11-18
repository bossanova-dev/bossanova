// dbt model proof from immutable git refs; configured terms are trusted only at the base.
import path from 'node:path'
import { isMainModule } from '../main-module.mjs'
import {
  blockFact,
  describerMain,
  envelopeDescriber,
  item,
  plural,
  runCli,
  signalFact,
} from './describer-kit.mjs'
import { MAX_TABLE_COLUMNS } from './verify-describe-blocks.mjs'
import {
  folderConfig,
  materialization,
  modelConfig,
  modelDirOf,
  nameType,
  readProject,
  schemaEntries,
  selectColumns,
} from './verify-dbt-parse.mjs'

/** @typedef {import('./describer-kit.mjs').Git} Git */
/** @typedef {import('./describer-kit.mjs').Change} Change */
/** @typedef {{name: string, type: string, removed?: boolean}} Column */
/** @typedef {{name: string, path: string, materialization?: string, schema?: string, columns: Column[], resolved: boolean, schemaPath?: string}} Model */

const DESCRIPTOR_FIRST = new Set(['count', 'has', 'is', 'num', 'percent', 'rate'])
const DESCRIPTOR_LAST = new Set([
  'count',
  'disposition',
  'domain',
  'kind',
  'label',
  'length',
  'match',
  'matched',
  'outcome',
  'rate',
  'share',
  'source',
  'status',
  'type',
  'valid',
  'verified',
])

/** Words naming a place or a business, whose name, address or phone is not a person's. */
const NOT_A_PERSON = new Set([
  'clinic',
  'company',
  'facility',
  'fund',
  'hospital',
  'location',
  'organisation',
  'organization',
  'practice',
  'provider',
  'site',
  'specialty',
])

const PERSON_NAME = new Set([
  'family',
  'first',
  'full',
  'given',
  'last',
  'legal',
  'middle',
  'patient',
  'preferred',
])

/** Each personal identifier kind and the rule that names it, by snake_case word. */
/** @type {[string, (words: string[], person: boolean) => boolean][]} */
const PII_RULES = [
  [
    'name',
    (words, person) =>
      person &&
      (words.includes('surname') ||
        words.includes('forename') ||
        (['name', 'names'].includes(words.at(-1)) && PERSON_NAME.has(words.at(-2)))),
  ],
  [
    'date of birth',
    (words) =>
      words.includes('dob') ||
      words.includes('birthdate') ||
      words.includes('birthday') ||
      words.includes('birth'),
  ],
  [
    'address',
    (words, person) =>
      person &&
      !words.includes('email') &&
      !words.includes('ip') &&
      (words.includes('address') || words.includes('street')),
  ],
  [
    'phone',
    (words, person) =>
      person &&
      (words.includes('phone') ||
        words.includes('telephone') ||
        (words.includes('mobile') && words.includes('number'))),
  ],
  ['email', (words) => words.includes('email')],
]

/** @param {string} column @param {string[]} [terms] */
export function piiKind(column, terms = []) {
  const words = column.toLowerCase().split('_').filter(Boolean)

  if (words.length === 0 || DESCRIPTOR_FIRST.has(words[0]) || DESCRIPTOR_LAST.has(words.at(-1))) {
    return null
  }

  for (const term of terms) {
    const tokens = term
      .toLowerCase()
      .split(/[\s_-]+/)
      .filter(Boolean)
    if (tokens.length && tokens.every((token) => words.includes(token))) return tokens.join(' ')
  }
  const person = !words.some((word) => NOT_A_PERSON.has(word))

  return PII_RULES.find(([, rule]) => rule(words, person))?.[0] ?? null
}

/** @param {Git} git @param {string} ref @param {string} projectFile */
function modelsAt(git, ref, projectFile) {
  const text = git.fileAt(ref, projectFile)
  const project = text ? readProject(projectFile, text) : null
  const files = project ? git.filesAt(ref, project.modelDirs) : []
  const projectOf = (/** @type {string} */ file) =>
    project && modelDirOf(project, file) ? project : null
  /** @type {Map<string, string>} */
  const models = new Map()
  /** @type {string[]} */
  const schemaFiles = []

  for (const file of files) {
    if (!projectOf(file)) continue
    if (file.endsWith('.sql')) models.set(path.posix.basename(file, '.sql'), file)
    if (/\.ya?ml$/.test(file)) schemaFiles.push(file)
  }

  /** @type {Map<string, string>} */
  let sources
  /** @type {Map<string, {columns: string[], materialized?: string, path: string}>} */
  let schemaIndex
  /** @param {string} name */
  const schemaEntry = (name) => {
    if (!schemaIndex) {
      schemaIndex = new Map()

      for (const [file, text] of git.readFiles(ref, schemaFiles)) {
        for (const [model, entry] of schemaEntries(text)) {
          schemaIndex.set(model, { ...entry, path: file })
        }
      }
    }

    return schemaIndex.get(name)
  }
  /** @type {Map<string, Model>} */
  const cache = new Map()

  /** @param {string} name @returns {Model | null} */
  const model = (name) => {
    const file = models.get(name)

    if (!file) return null
    if (cache.has(name)) return cache.get(name)

    sources ??= git.readFiles(ref, [...models.values()])

    const sql = sources.get(file)
    const own = modelConfig(sql)
    const entry = schemaEntry(name)
    const folder = folderConfig(projectOf(file), file)
    const selected = selectColumns(sql)
    const names = new Set(selected.columns.map((column) => column.name))
    const columns = [...selected.columns]

    if (!selected.resolved) {
      for (const column of entry?.columns ?? []) {
        if (!names.has(column)) {
          columns.push({
            name: column,
            type: nameType(column),
          })
        }
      }
    }

    const described = {
      columns: columns.filter(
        (column, index) => columns.findIndex((other) => other.name === column.name) === index,
      ),
      materialization: materialization(
        own.materialized ?? entry?.materialized ?? folder.materialized ?? 'view',
      ),
      name,
      path: file,
      resolved: selected.resolved,
      schema: own.schema ?? folder.schema,
      schemaPath: entry?.path,
    }

    cache.set(name, described)

    return described
  }

  return { model, models, projectOf, schemaEntry }
}

/** @param {Model | null} model */
const exposed = (model) => model && model.materialization !== 'ephemeral'

/** The relation schema, without reading deployment profiles. */
/** @param {Model} model */
const readableBy = (model) =>
  model.schema ? `in the ${model.schema} schema` : 'in the target schema'

/**
 * One model's change as `{block, materializationMoved, model}`, or null when nothing a
 * reader sees moved. `sqlChanged` says the model's own file is in the diff.
 */
/** @param {Model | null} before @param {Model | null} after @param {boolean} sqlChanged @param {string[]} terms */
function modelChange(before, after, sqlChanged, terms) {
  const model = after ?? before
  const beforeColumns = new Map((before?.columns ?? []).map((column) => [column.name, column]))
  const afterColumns = new Map((after?.columns ?? []).map((column) => [column.name, column]))
  const added = [...afterColumns.values()].filter((column) => !beforeColumns.has(column.name))
  const removed = [...beforeColumns.values()].filter((column) => !afterColumns.has(column.name))
  const changed = [...afterColumns.values()].filter(
    (column) =>
      beforeColumns.has(column.name) && beforeColumns.get(column.name).type !== column.type,
  )
  const changedNames = new Set(changed.map((column) => column.name))
  const materializationMoved = before && after && before.materialization !== after.materialization
  const schemaMoved = before && after && before.schema !== after.schema

  if (
    before &&
    after &&
    !sqlChanged &&
    !materializationMoved &&
    !schemaMoved &&
    added.length + removed.length + changed.length === 0
  ) {
    return null
  }

  const newlyExposed = exposed(after)
    ? after.columns.filter(
        (column) =>
          piiKind(column.name, terms) &&
          (!exposed(before) || schemaMoved || !beforeColumns.has(column.name)),
      )
    : []
  const exposedNames = new Set(newlyExposed.map((column) => column.name))
  /** @type {Column[]} */
  const shown = !before
    ? after.columns
    : !after
      ? []
      : [
          ...added,
          ...changed,
          ...newlyExposed.filter(
            (column) => beforeColumns.has(column.name) && !changedNames.has(column.name),
          ),
          ...removed.map((column) => ({ ...column, removed: true })),
        ]
  const columns = shown.map((column) => {
    const pii = piiKind(column.name, terms)
    const change =
      !before || !after
        ? undefined
        : column.removed
          ? 'removed'
          : !beforeColumns.has(column.name)
            ? 'added'
            : changedNames.has(column.name)
              ? 'changed'
              : undefined

    const notes = [
      ...(change === 'changed' ? [`was ${beforeColumns.get(column.name).type}`] : []),
      ...(exposedNames.has(column.name)
        ? [
            `${pii}, ${readableBy(after)}${before && !exposed(before) ? ` (was ${before.materialization})` : ''}`,
          ]
        : []),
    ]

    return {
      name: column.name,
      type: column.type,
      ...(change ? { change } : {}),
      ...(pii ? { pii: true } : {}),
      ...(notes.length ? { note: notes.join('; ') } : {}),
    }
  })
  /** @param {{pii?: boolean, note?: string}} column */
  const marked = (column) => column.pii || column.note

  return {
    block: {
      kind: 'table',
      name: model.name,
      relation: 'dbt-model',
      ...(model.materialization ? { materialization: model.materialization } : {}),
      change: !before ? 'added' : !after ? 'removed' : 'changed',
      columns: [...columns.filter(marked), ...columns.filter((column) => !marked(column))]
        .slice(0, MAX_TABLE_COLUMNS)
        .sort((a, b) => columns.indexOf(a) - columns.indexOf(b)),
    },
    columnChanges: columns,
    materializationMoved: Boolean(materializationMoved),
    schemaMoved: Boolean(schemaMoved),
    model,
  }
}

/** The Database signal over every model change. */
/** @param {ReturnType<typeof modelChange>[]} changes @param {string[]} paths */
function databaseSignal(changes, paths) {
  /** @param {string} change */
  const count = (change) => changes.filter((entry) => entry.block.change === change).length
  /** @param {string} change */
  const columnsMoved = (change) =>
    changes.flatMap((entry) => entry.columnChanges.filter((column) => column.change === change))
      .length
  const parts = [
    count('added') ? `${plural(count('added'), 'model')} added` : null,
    count('changed') ? `${count('changed')} changed` : null,
    count('removed') ? `${count('removed')} removed` : null,
    columnsMoved('added') ? `${plural(columnsMoved('added'), 'column')} added` : null,
    columnsMoved('removed') ? `${plural(columnsMoved('removed'), 'column')} removed` : null,
    columnsMoved('changed') ? `${plural(columnsMoved('changed'), 'column')} changed` : null,
  ].filter(Boolean)
  const moved = changes.filter((entry) => entry.materializationMoved)
  const relocated = changes.filter((entry) => entry.schemaMoved)

  return signalFact(
    {
      key: 'db',
      label: 'Database',
      state:
        count('removed') ||
        columnsMoved('removed') ||
        columnsMoved('changed') ||
        moved.length ||
        relocated.length
          ? 'review'
          : 'additive',
      summary: `dbt: ${parts.join(', ') || 'models changed'}${
        moved.length ? `; ${plural(moved.length, 'model')} rematerialised` : ''
      }${relocated.length ? `; ${plural(relocated.length, 'model')} relocated` : ''}`,
    },
    paths,
  )
}

/** Every dbt fact over one range's changes. */
/** @param {{git: Git, base: string, head: string, changes: Change[]}} input @param {string} projectFile @param {string[]} terms */
function collectProject({ git, base, head, changes }, projectFile, terms) {
  const before = modelsAt(git, base, projectFile)
  const after = modelsAt(git, head, projectFile)
  const changedPaths = new Set(changes.flatMap((change) => [change.path, change.previousPath]))
  /** @type {Set<string>} */
  const candidates = new Set()
  /** @type {Set<string>} */
  const sqlChanged = new Set()

  for (const reader of [before, after]) {
    for (const [name, file] of reader.models) {
      if (changedPaths.has(file)) {
        candidates.add(name)
        sqlChanged.add(name)
      }

      const entry = reader.schemaEntry(name)
      const project = reader.projectOf(file)

      if (
        (entry && changedPaths.has(entry.path)) ||
        changedPaths.has(path.posix.join(project.root, 'dbt_project.yml'))
      ) {
        candidates.add(name)
      }
    }
  }

  const facts = []
  /** @type {ReturnType<typeof modelChange>[]} */
  const described = []

  for (const name of [...candidates].sort()) {
    const was = before.model(name)
    const now = after.model(name)
    const change = modelChange(was, now, sqlChanged.has(name), terms)

    if (!change) continue

    const project = (now ? after : before).projectOf((now ?? was).path)
    const paths = [
      ...new Set([
        was?.path,
        now?.path,
        was?.schemaPath,
        now?.schemaPath,
        path.posix.join(project.root, 'dbt_project.yml'),
      ]),
    ].filter((file) => file && changedPaths.has(file))

    described.push(change)
    facts.push(blockFact('dbt', change.block, paths))

    if (change.schemaMoved) {
      facts.push(item('dbt', `${name}: was ${readableBy(was)}, now ${readableBy(now)}`, paths))
    }

    if (change.materializationMoved) {
      facts.push(
        item(
          'dbt',
          `${name}: materialised as ${was.materialization}, now ${now.materialization}`,
          paths,
        ),
      )
    }
  }

  return { facts, described }
}

/** Tolerant read of the one setting this describer consumes. @param {any} config @returns {string[]} */
export function piiTermsFrom(config) {
  const terms = config?.proofDefaults?.dbt?.piiTerms
  return Array.isArray(terms) ? terms.filter((term) => typeof term === 'string' && term.trim()) : []
}

/** Nearest project at either ref for each changed path, including rename sources.
 * @param {Git} git @param {string} base @param {string} head @param {Change[]} changes */
function projectFiles(git, base, head, changes) {
  const projects = new Set()
  const cached = new Map()
  for (const file of changes
    .flatMap((change) => [change.path, change.previousPath])
    .filter(Boolean)) {
    for (const ref of [base, head]) {
      let dir = path.posix.dirname(file)
      for (;;) {
        const candidate = path.posix.join(dir, 'dbt_project.yml')
        const key = `${ref}:${candidate}`
        if (!cached.has(key)) cached.set(key, !!git.fileAt(ref, candidate))
        if (cached.get(key)) {
          projects.add(candidate)
          break
        }
        if (dir === '.') break
        dir = path.posix.dirname(dir)
      }
    }
  }
  return [...projects].sort()
}

/** Every model fact, with one Database signal across all discovered projects.
 * @param {{git: Git, base: string, head: string, changes: Change[]}} input */
export function collectItems(input) {
  const { git, base, head, changes } = input
  const projects = projectFiles(git, base, head, changes)
  if (!projects.length) return []
  let terms = []
  try {
    terms = piiTermsFrom(JSON.parse(git.fileAt(base, '.boss-skills.json')))
  } catch {
    /* Missing or malformed config uses generic defaults. */
  }
  const results = projects.map((file) => collectProject(input, file, terms))
  const described = results.flatMap((result) => result.described)
  if (!described.length) return []
  described.sort((a, b) => a.model.name.localeCompare(b.model.name))
  const facts = results.flatMap((result) => result.facts)
  facts.sort((a, b) => (a.block?.name ?? a.text).localeCompare(b.block?.name ?? b.text))
  return [
    ...facts,
    databaseSignal(
      described,
      changes.flatMap((change) => [change.path, change.previousPath]).filter(Boolean),
    ),
  ]
}

export const describeEnvelope = envelopeDescriber(collectItems)
export const main = () => describerMain(describeEnvelope)
if (isMainModule(import.meta.url)) runCli(import.meta.url, 'verify-describe-dbt', collectItems)
