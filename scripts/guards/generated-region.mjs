import { spawnSync } from 'node:child_process'
import fs from 'node:fs'
import os from 'node:os'
import path from 'node:path'

/** Replace a named generated region; reject ambiguous markers.
 * @param {string} text
 * @param {string} body
 * @param {string} name
 * @param {string} updateCommand
 */
export function spliceRegion(text, body, name, updateCommand) {
  const BEGIN_MARKER = `<!-- BEGIN GENERATED ${name}: run \`${updateCommand}\`, do not edit -->`
  const END_MARKER = `<!-- END GENERATED ${name} -->`
  const lines = text.split('\n')
  const find = (marker) =>
    lines.flatMap((line, index) => (line.trim().startsWith(marker) ? [index] : []))
  const begins = find(`<!-- BEGIN GENERATED ${name}`)
  const ends = find(`<!-- END GENERATED ${name}`)
  if (begins.length !== 1) {
    throw new Error(`expected exactly one BEGIN ${name} marker line, found ${begins.length}`)
  }
  if (ends.length !== 1) {
    throw new Error(`expected exactly one END ${name} marker line, found ${ends.length}`)
  }
  const [begin] = begins
  const [end] = ends
  if (end < begin) throw new Error(`the END ${name} marker comes before the BEGIN marker`)
  return [
    ...lines.slice(0, begin),
    BEGIN_MARKER,
    ...body.replace(/\n$/, '').split('\n'),
    END_MARKER,
    ...lines.slice(end + 1),
  ].join('\n')
}

export function renderDrift({ actual, expected, label }, updateCommand) {
  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'rules-table-diff-'))
  try {
    const actualPath = path.join(tmp, 'actual')
    const expectedPath = path.join(tmp, 'expected')
    fs.writeFileSync(actualPath, actual)
    fs.writeFileSync(expectedPath, expected)
    const diff = spawnSync(
      'diff',
      ['-u', '--label', label, actualPath, '--label', `${label} (expected)`, expectedPath],
      { encoding: 'utf8' },
    )
    return [
      `ERROR: ${label} is out of sync with generated inputs:\n`,
      diff.stdout,
      diff.stderr,
      `Run: ${updateCommand}\n`,
    ]
      .filter(Boolean)
      .join('')
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 })
  }
}
