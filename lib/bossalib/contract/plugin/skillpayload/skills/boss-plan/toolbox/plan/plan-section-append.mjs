import { chmodSync, readFileSync, renameSync, rmSync, statSync, writeFileSync } from 'node:fs'
import { randomUUID } from 'node:crypto'
import { dirname, join } from 'node:path'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'

/** @param {string} text */
function contentLines(text) {
  /** @type {{start: number, end: number, heading?: string, bullet?: RegExpExecArray, section: string}[]} */
  const lines = []
  let section = ''
  /** @type {{char: string, length: number} | null} */
  let fence = null
  for (const match of text.matchAll(/[^\r\n]*(?:\r\n|\n|$)/g)) {
    if (!match[0]) continue
    const raw = match[0]
    const line = raw.replace(/\r?\n$/, '')
    if (fence) {
      if (new RegExp(`^ {0,3}${fence.char}{${fence.length},}\\s*$`).test(line)) fence = null
      continue
    }
    const opening = /^ {0,3}(`{3,}|~{3,})/.exec(line)
    if (opening) {
      fence = { char: opening[1][0], length: opening[1].length }
      continue
    }
    const heading = /^##\s+(.+?)\s*#*\s*$/.exec(line)?.[1]
    if (heading) section = heading
    lines.push({
      start: match.index,
      end: match.index + raw.length,
      section,
      heading,
      bullet: /^([-*+])\s+(.+?)\s*$/.exec(line) ?? undefined,
    })
    if (section === 'Original notes') break
  }
  return lines
}

/**
 * Read the stored section's marker, ignoring fenced examples and the reporter's tail.
 * @param {string} text
 * @param {string} heading
 * @returns {string}
 */
export function sectionListMarker(text, heading) {
  return listMarker(contentLines(text), heading)
}

/** @param {ReturnType<typeof contentLines>} lines @param {string} heading */
function listMarker(lines, heading) {
  return (
    (
      lines.find((line) => line.section === heading && line.bullet) ??
      lines.find((line) => line.bullet)
    )?.bullet?.[1] ?? '-'
  )
}

/** @param {string} text @param {number} start @param {number} limit */
function itemEnd(text, start, limit) {
  let end = start
  let separated = false
  for (const match of text.slice(start, limit).matchAll(/[^\r\n]*(?:\r\n|\n|$)/g)) {
    if (!match[0]) continue
    const line = match[0].replace(/\r?\n$/, '')
    if (!line.trim()) {
      separated = true
      continue
    }
    const indented = /^(?: {2,}|\t)/.test(line)
    if (!indented && (separated || /^(?:#{1,6}\s|[-*+]\s|\d+[.)]\s|`{3,}|~{3,}|>)/.test(line)))
      break
    end = start + match.index + match[0].length
    separated = false
  }
  return end
}

/**
 * Append new bullet text without reformatting the stored description.
 * @param {string} text
 * @param {{heading: string, items: string[]}} options
 * @returns {{text: string, appended: string[], skipped: string[], marker: string, created: boolean}}
 */
export function appendSectionBullets(text, { heading, items }) {
  if (!heading.trim() || /[\r\n]/.test(heading) || heading.trim() === 'Original notes')
    throw new Error('heading must name an editable section')
  if (items.some((item) => !item.trim() || /[\r\n]/.test(item)))
    throw new Error('items must be non-empty single lines')
  const lines = contentLines(text)
  const header = lines.find((line) => line.heading === heading)
  const end = header
    ? (lines.find((line) => line.start > header.start && line.heading)?.start ?? text.length)
    : text.length
  const bullets = header
    ? lines.filter((line) => line.start >= header.end && line.start < end && line.bullet)
    : []
  const existing = new Set(bullets.map((line) => line.bullet?.[2]))
  const marker = listMarker(lines, heading)
  const appended = [],
    skipped = []
  for (const item of items) {
    if (existing.has(item)) skipped.push(item)
    else {
      appended.push(item)
      existing.add(item)
    }
  }
  const created = !header && appended.length > 0
  if (!appended.length) return { text, appended, skipped, marker, created }
  const eol = text.includes('\r\n') ? '\r\n' : '\n'
  const body = appended.map((item) => `${marker} ${item}`).join(eol) + eol
  let index, insertion
  if (header) {
    const last = bullets.at(-1)
    index = last ? itemEnd(text, last.end, end) : header.end
    if (!last && text.slice(index).startsWith(eol)) index += eol.length
    insertion =
      (index > 0 && text[index - 1] !== '\n' ? eol : '') +
      (!last && index === header.end ? eol : '') +
      body
  } else {
    const anchor =
      lines.find(
        (line) => line.heading === (heading === 'Open Questions' ? 'Planning' : 'Original notes'),
      ) ?? lines.find((line) => line.heading === 'Original notes')
    index = anchor?.start ?? text.length
    const before = text.slice(0, index)
    const gap = !before || before.endsWith(eol + eol) ? '' : before.endsWith(eol) ? eol : eol + eol
    insertion = `${gap}## ${heading}${eol}${eol}${body}${anchor ? eol : ''}`
  }
  let output = text.slice(0, index) + insertion + text.slice(index)
  if (!text.endsWith('\n') && output.endsWith(eol)) output = output.slice(0, -eol.length)
  return { text: output, appended, skipped, marker, created }
}

function main() {
  let values
  try {
    ;({ values } = parseArgs({
      strict: true,
      options: {
        file: { type: 'string' },
        heading: { type: 'string' },
        item: { type: 'string', multiple: true },
      },
    }))
    if (!values.file || !values.heading || !values.item?.length)
      throw new Error('expected --file, --heading and --item')
  } catch (error) {
    console.error(`plan-section-append: ${error.message}`)
    return 64
  }
  let temporary
  try {
    const file = values.file
    const text = readFileSync(file, 'utf8')
    const result = appendSectionBullets(text, { heading: values.heading, items: values.item })
    if (result.text !== text) {
      temporary = join(dirname(file), `.plan-section-${randomUUID()}.tmp`)
      writeFileSync(temporary, result.text, { flag: 'wx' })
      chmodSync(temporary, statSync(file).mode & 0o7777)
      renameSync(temporary, file)
    }
    const { text: _text, ...receipt } = result
    console.log(JSON.stringify(receipt))
    return 0
  } catch (error) {
    console.error(`plan-section-append: ${error.message}`)
    return 1
  } finally {
    if (temporary) rmSync(temporary, { force: true })
  }
}

if (isMainModule(import.meta.url)) process.exitCode = main()
