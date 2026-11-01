// Verify extension applicability: run each extension's `applies` script and split the discovered
// extensions by its exit code, keeping the optional one-line note a not-applicable check printed.
import { join } from 'node:path'
import { validNotApplicable } from './verify-not-applicable.mjs'

const text = (value) => typeof value === 'string' && value.trim() !== ''

// The applicability-check exit code that means "this extension does not apply". Every other
// outcome — exit 0, exit 1 (also Node's uncaught-exception code), a crash, a timeout, a missing
// script — applies: a check can only add work, never remove a park.
export const APPLIES_NOT_APPLICABLE_EXIT = 10

const NOTE_RUNES = 200

/**
 * The note an applies script left on its last non-blank stdout line, a JSON object with a string
 * `note`: whitespace runs collapsed to single spaces, trimmed and capped at 200 runes with a
 * trailing `…`. Earlier stdout lines are diagnostics and are ignored. Returns null for a missing,
 * empty, non-string or unparseable note, and never throws.
 *
 * @param {unknown} output the script's stdout
 * @returns {string | null}
 */
export function appliesNote(output) {
  try {
    const last = String(output ?? '')
      .split(/\r?\n/)
      .filter((line) => line.trim())
      .at(-1)
    if (last === undefined) return null
    const note = JSON.parse(last)?.note
    if (typeof note !== 'string') return null
    const runes = Array.from(note.replace(/\s+/g, ' ').trim())
    if (!runes.length) return null
    return runes.length > NOTE_RUNES
      ? `${runes.slice(0, NOTE_RUNES - 1).join('')}…`
      : runes.join('')
  } catch {
    return null
  }
}

/**
 * Splits discovered verify extensions into `{applicable, notApplicable, notes}` by running each
 * declared `applies` script with node, the would-be extension envelope on stdin. The script is
 * resolved under `root` (the trusted base tree) when given, so it and everything it imports are
 * base code, while the process cwd stays the checkout for its git reads. An extension with no
 * `applies` always applies. Only the exit code decides applicability; `notes` is
 * `[{extension, note}]`, in discovery order, for each not-applicable extension whose output
 * carried a usable note.
 *
 * @param {Array<{name?: string, applies?: string}>} extensions
 * @param {unknown} context the extension envelope, sent as JSON on stdin
 * @param {{read: Function, nodeBin?: string, root?: string | null}} deps
 * @returns {{applicable: object[], notApplicable: object[], notes: {extension: string, note: string}[]}}
 */
export function partitionApplicable(
  extensions,
  context,
  { read, nodeBin = process.execPath, root = null },
) {
  const applicable = []
  const notApplicable = []
  const notes = []
  for (const ext of extensions) {
    const result = text(ext?.applies)
      ? read(nodeBin, [text(root) ? join(root, ext.applies) : ext.applies], {
          input: JSON.stringify(context),
        })
      : null
    const skip = result?.exit === APPLIES_NOT_APPLICABLE_EXIT
    ;(skip ? notApplicable : applicable).push(ext)
    const entry = skip ? { extension: ext.name, note: appliesNote(result.output) } : null
    if (entry && validNotApplicable([entry])) notes.push(entry)
  }
  return { applicable, notApplicable, notes }
}
