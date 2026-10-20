import fs from 'node:fs'
import { parse } from 'yaml'

export const LAYERS = ['go', 'proto', 'ts', 'table']
const required = ['id', 'noun', 'code', 'table', 'proto', 'ui', 'definition', 'bans']
const banFields = [
  'match',
  'kind',
  'layers',
  'position',
  'except_after',
  'paths',
  'exclude_paths',
  'use',
]
const strings = (value) =>
  Array.isArray(value) && value.every((item) => typeof item === 'string' && item.length > 0)
const object = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

/** Load and validate the shared identifier vocabulary without throwing on malformed input.
 * @param {string} file
 */
export function loadEntities(file) {
  const errors = []
  const warnings = []
  let data
  try {
    data = parse(fs.readFileSync(file, 'utf8'))
  } catch (error) {
    return { entities: [], verbs: [], errors: [`${file}: ${error.message}`], warnings }
  }
  if (!object(data))
    return { entities: [], verbs: [], errors: ['entity data must be a mapping'], warnings }
  for (const key of Object.keys(data))
    if (!['verbs', 'entities'].includes(key)) warnings.push(`unknown top-level field: ${key}`)
  if (!strings(data.verbs)) errors.push('verbs must be an array of nonempty strings')
  if (!Array.isArray(data.entities)) errors.push('entities must be an array')
  const entities = Array.isArray(data.entities) ? data.entities : []
  const ids = new Set()
  entities.forEach((entity, index) => {
    const at = `entities[${index}]`
    if (!object(entity)) {
      errors.push(`${at} must be a mapping`)
      return
    }
    for (const key of required) {
      if (!(key in entity)) errors.push(`${at} is missing ${key}`)
      else if (
        key !== 'bans' &&
        !(typeof entity[key] === 'string' && entity[key].length > 0) &&
        !(['table', 'proto', 'ui'].includes(key) && entity[key] === null)
      )
        errors.push(
          `${at}.${key} must be a nonempty string${['table', 'proto', 'ui'].includes(key) ? ' or null' : ''}`,
        )
    }
    if (ids.has(entity.id)) errors.push(`${at}: duplicate id ${entity.id}`)
    ids.add(entity.id)
    for (const key of Object.keys(entity))
      if (![...required, 'notes'].includes(key)) warnings.push(`${at}: unknown field ${key}`)
    if ('notes' in entity && typeof entity.notes !== 'string')
      errors.push(`${at}.notes must be a string`)
    if (!Array.isArray(entity.bans)) {
      errors.push(`${at}.bans must be an array`)
      return
    }
    entity.bans.forEach((ban, index) => {
      const atBan = `${at}.bans[${index}]`
      if (!object(ban)) {
        errors.push(`${atBan} must be a mapping`)
        return
      }
      if (typeof ban.match !== 'string' || !ban.match.length)
        errors.push(`${atBan}.match must be a nonempty string`)
      if (
        !strings(ban.layers) ||
        !ban.layers.length ||
        ban.layers.some((layer) => !LAYERS.includes(layer))
      )
        errors.push(`${atBan}.layers contains an unknown layer or is empty`)
      if (ban.kind !== undefined && !['words', 'text'].includes(ban.kind))
        errors.push(`${atBan}.kind is unknown`)
      if (ban.position !== undefined && !['any', 'whole', 'after-verb'].includes(ban.position))
        errors.push(`${atBan}.position is unknown`)
      for (const key of ['except_after', 'paths', 'exclude_paths'])
        if (ban[key] !== undefined && !strings(ban[key]))
          errors.push(`${atBan}.${key} must be an array of nonempty strings`)
      if (ban.use !== undefined && (typeof ban.use !== 'string' || !ban.use.length))
        errors.push(`${atBan}.use must be a nonempty string`)
      for (const key of Object.keys(ban))
        if (!banFields.includes(key)) warnings.push(`${atBan}: unknown field ${key}`)
    })
  })
  return { entities, verbs: strings(data.verbs) ? data.verbs : [], errors, warnings }
}
