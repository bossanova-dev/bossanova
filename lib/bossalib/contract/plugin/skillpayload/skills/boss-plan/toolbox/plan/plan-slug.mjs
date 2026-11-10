// Plan naming for boss-plan's plan-file path. issueSlug returns the whole
// `<ID>-<title-slug>` stem; the CLI composes it through the declared scratch family.
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

/**
 * Return the whole plan-file stem `<ID>-<title-slug>`, already prefixed with the id.
 * @param {string} id Tracker issue identifier.
 * @param {string} title
 * @returns {string}
 */
export function issueSlug(id, title) {
  if (typeof id !== 'string' || !id.trim()) {
    const hint =
      id !== null && typeof id === 'object'
        ? '; pass id and title positionally rather than an issue object'
        : ''
    throw new TypeError(
      `issueSlug(id, title): id must be a non-blank string; received ${typeof id}${hint}`,
    )
  }
  if (typeof title !== 'string' || !title.trim()) {
    throw new TypeError(
      `issueSlug(id, title): title must be a non-blank string; received ${typeof title}`,
    )
  }
  return `${id.toUpperCase()}-${titleSlug(title)}`
}

/** @param {string} title */
export function titleSlug(title) {
  if (typeof title !== 'string' || !title.trim()) {
    throw new TypeError('titleSlug(title): title must be a non-blank string')
  }
  return title
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

async function main() {
  let values
  try {
    const { positionals, values: parsed } = parseArgs({
      allowPositionals: true,
      options: {
        'run-id': { type: 'string' },
        'issue-json': { type: 'string' },
        id: { type: 'string' },
        title: { type: 'string' },
      },
    })
    const [cmd] = positionals
    if (cmd !== 'plan-path' || positionals.length !== 1) throw new Error('expected plan-path')
    const fromFile = parsed['issue-json'] !== undefined
    const direct = parsed.id !== undefined || parsed.title !== undefined
    if (
      parsed['run-id'] === undefined ||
      fromFile === direct ||
      (direct && (parsed.id === undefined || parsed.title === undefined))
    ) {
      throw new Error('provide --run-id and either --issue-json or --id with --title')
    }
    values = parsed
  } catch (error) {
    process.stderr.write(`${String(error.message).replace(/[\r\n]+/g, ' ')}\n`)
    return 64
  }
  try {
    let id = values.id
    let title = values.title
    if (values['issue-json'] !== undefined) {
      const issue = JSON.parse(readFileSync(values['issue-json'], 'utf8'))
      id =
        typeof issue?.identifier === 'string' && issue.identifier.trim()
          ? issue.identifier
          : issue?.id
      title = issue?.title
    }
    const { planScratchPath } = await import('./plan-scratch-paths.mjs')
    const path = planScratchPath(values['run-id'], 'plan', {
      issueId: id,
      title,
    })
    process.stdout.write(`${path}\n`)
    return 0
  } catch (error) {
    process.stderr.write(`${String(error.message).replace(/[\r\n]+/g, ' ')}\n`)
    return 1
  }
}

if (isMainModule(import.meta.url))
  main().then((code) => {
    process.exitCode = code
  })
