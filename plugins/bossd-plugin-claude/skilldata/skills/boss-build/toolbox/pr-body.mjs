#!/usr/bin/env node
// Merge freshly composed `## ` sections into a PR body that is already published, so a resume or a
// repeated publish converges instead of growing. Each section in the update replaces the first
// section with the same heading and drops every later copy; untouched sections, the preamble and
// bot-maintained blocks (an auto-generated summary another app appends) survive. Bodies are spliced
// as arrays of lines, never through String#replace: a replacement string expands `$'`, `$&` and
// `` $` ``, and a `go test -run '^X$'` command in the text splices the rest of the body back in.
import defaultFs from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from './main-module.mjs'
import { markdownH2Heading, scanFences } from './skill-config.mjs'

const FOREIGN_OPEN = /^<!--\s*this is an auto-generated (?:description|comment)\b.*-->\s*$/i
const FOREIGN_CLOSE = /^<!--\s*end of auto-generated (?:description|comment)\b.*-->\s*$/i
const FENCE_CLOSERS = ['```', '~~~', '`'.repeat(16), '~'.repeat(16)]

export const sectionKey = (heading) =>
  String(heading ?? '')
    .replace(/^\s*#+\s*/, '')
    .replace(/\s+#+\s*$/, '')
    .replace(/[\s:]+$/, '')
    .replace(/\s+/g, ' ')
    .toLowerCase()

const trimBlankEdges = (lines) => {
  let start = 0
  let end = lines.length
  while (start < end && !lines[start].trim()) start += 1
  while (end > start && !lines[end - 1].trim()) end -= 1
  return lines.slice(start, end)
}

// Close a fence left open at the end of a block, so one block's code cannot swallow the headings
// of the blocks rendered after it.
function closeOpenFence(lines, where, warnings) {
  const content = trimBlankEdges(lines)
  if (!scanFences(content.join('\n')).unterminated) return lines
  const closer = FENCE_CLOSERS.find(
    (candidate) => !scanFences([...content, candidate].join('\n')).unterminated,
  )
  warnings.push(`closed an unterminated code fence in ${where}`)
  return closer ? [...content, closer] : content
}

/**
 * Parse a PR body into its preamble (everything before the first top-level `## ` heading), its
 * sections in order, and any bot-maintained blocks. CRLF and lone CR are read as LF. A `##` line or
 * bot marker inside a fenced code block is content; when a fence never closes, fences are ignored
 * and each block's open fence is closed at its end. A bot block runs from its opener to the closer
 * that balances it, so a nested pair stays inside the outer block; an opener that is never balanced
 * is ordinary text.
 *
 * Only `## ` headings and bot marker pairs delimit. Everything else belongs to the section above
 * it: a `# ` heading, a setext heading (`Title` over `---`), and unheaded text after the last
 * section are part of that section and go with it when an update replaces it. A writer that appends
 * to a body must give its text a `## ` heading or a marker pair to keep it across merges.
 * @returns {{ preamble: string[], sections: { heading: string, key: string, lines: string[] }[], foreign: string[][], warnings: string[] }}
 */
export function parsePrBody(text) {
  const lines = String(text ?? '').split(/\r\n|\r|\n/)
  const fences = scanFences(lines.join('\n'))
  const outside = fences.unterminated
    ? new Set(lines.keys())
    : new Set(fences.lines.map(({ index }) => index))
  const marker = (index) =>
    !outside.has(index)
      ? null
      : FOREIGN_OPEN.test(lines[index])
        ? 'open'
        : FOREIGN_CLOSE.test(lines[index])
          ? 'close'
          : null
  const warnings = []
  const preamble = []
  const sections = []
  const foreign = []
  let current = null
  for (let index = 0; index < lines.length; index += 1) {
    if (marker(index) === 'open') {
      let depth = 1
      let next = index + 1
      for (; next < lines.length; next += 1) {
        const kind = marker(next)
        if (kind) depth += kind === 'open' ? 1 : -1
        if (depth === 0) break
      }
      if (depth === 0) {
        foreign.push(lines.slice(index, next + 1))
        index = next
        continue
      }
    }
    const heading = outside.has(index) ? markdownH2Heading(lines[index]) : null
    if (heading) {
      current = { heading, key: sectionKey(heading), lines: [] }
      sections.push(current)
    } else {
      ;(current ? current.lines : preamble).push(lines[index])
    }
  }
  for (const section of sections)
    section.lines = closeOpenFence(section.lines, `"${section.heading}"`, warnings)
  return {
    preamble: closeOpenFence(preamble, 'the preamble', warnings),
    sections,
    foreign,
    warnings,
  }
}

function renderPrBody({ preamble, sections, foreign }) {
  const blocks = [
    trimBlankEdges(preamble).join('\n'),
    ...sections.map((s) => [s.heading, ...trimBlankEdges(s.lines)].join('\n')),
    ...foreign.map((block) => block.join('\n')),
  ].filter((block) => block.length > 0)
  return blocks.length ? `${blocks.join('\n\n')}\n` : ''
}

/**
 * Merge `update` into `current`. The update's sections replace same-heading sections (heading match
 * ignores case, spacing and a trailing colon) at the first one's position, later copies are
 * dropped, and sections the current body lacks are appended in the update's order. A section the
 * update does not name keeps its place, minus any later copy with identical content; copies that
 * still differ are kept and named in a warning. Every section whose heading is in `drop` is
 * removed, unless the update renders it. A non-blank update preamble replaces the current one, and
 * a bot block in the update replaces that bot's published block. Merging the result with the same
 * update and `drop` again returns it unchanged.
 * @param {string} current
 * @param {string} update
 * @param {{ drop?: string[] }} [options]
 * @returns {{ body: string, warnings: string[] }}
 */
export function mergePrBody(current, update, { drop = [] } = {}) {
  const base = parsePrBody(current)
  const next = parsePrBody(update)
  const warnings = [...base.warnings, ...next.warnings]
  const dropped = new Set(drop.map(sectionKey))
  const replacements = new Map()
  for (const section of next.sections) {
    if (replacements.has(section.key)) {
      warnings.push(`update repeats "${section.heading}"; the first copy is used`)
      continue
    }
    replacements.set(section.key, section)
  }
  for (const key of dropped)
    if (replacements.has(key))
      warnings.push(
        `--drop names "${replacements.get(key).heading}", which the update renders; the update's copy is kept`,
      )
  const emitted = new Set()
  const kept = new Map()
  const sections = []
  for (const section of base.sections) {
    const replacement = replacements.get(section.key)
    if (!replacement) {
      if (dropped.has(section.key)) continue
      const variants = kept.get(section.key) ?? new Map()
      const content = trimBlankEdges(section.lines).join('\n')
      if (!variants.has(content)) sections.push(section)
      variants.set(content, section.heading)
      kept.set(section.key, variants)
      continue
    }
    if (emitted.has(section.key)) continue
    emitted.add(section.key)
    sections.push(replacement)
  }
  for (const [key, section] of replacements) if (!emitted.has(key)) sections.push(section)
  for (const variants of kept.values())
    if (variants.size > 1)
      warnings.push(
        `"${[...variants.values()][0]}" survives in ${variants.size} copies with different content; render it in the update to replace them`,
      )
  const preamble = trimBlankEdges(next.preamble).length ? next.preamble : base.preamble
  const foreign = new Map()
  for (const block of [...base.foreign, ...next.foreign])
    foreign.set(block[0].trim().toLowerCase(), block)
  return { body: renderPrBody({ preamble, sections, foreign: [...foreign.values()] }), warnings }
}

const USAGE =
  'usage: node pr-body.mjs merge --body-file <current> --update-file <sections> [--drop <heading>]... [--out <path>]'

class UsageError extends Error {}

function parseCli(argv) {
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      options: {
        'body-file': { type: 'string' },
        'update-file': { type: 'string' },
        out: { type: 'string' },
        drop: { type: 'string', multiple: true },
      },
      allowPositionals: true,
    })
  } catch (error) {
    throw new UsageError(error.message)
  }
  const { positionals, values } = parsed
  const [command] = positionals
  if (positionals.length !== 1 || command !== 'merge')
    throw new UsageError(`unknown verb: ${positionals.join(' ') || '(none)'}`)
  if (!values['body-file'] || !values['update-file'])
    throw new UsageError('merge requires --body-file and --update-file')
  return values
}

export function main(argv, deps = {}) {
  const fs = deps.fs ?? defaultFs
  const stdout = deps.stdout ?? ((text) => process.stdout.write(text))
  const stderr = deps.stderr ?? ((text) => process.stderr.write(text))
  let values
  try {
    values = parseCli(argv)
  } catch (error) {
    stderr(`pr-body: ${error.message}\n${USAGE}\n`)
    return 2
  }
  let current
  let update
  try {
    current = fs.readFileSync(values['body-file'], 'utf8')
    update = fs.readFileSync(values['update-file'], 'utf8')
  } catch (error) {
    stderr(`pr-body: cannot read input: ${error.message}\n`)
    return 1
  }
  const { body, warnings } = mergePrBody(current, update, { drop: values.drop ?? [] })
  for (const warning of warnings) stderr(`pr-body: warning: ${warning}\n`)
  if (values.out) fs.writeFileSync(values.out, body)
  else stdout(body)
  return 0
}

if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
