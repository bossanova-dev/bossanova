// Render dependency-scan notes for the Planning section without changing the scan.

/** @param {unknown} value @returns {value is Record<string, unknown>} */
function record(value) {
  return value !== null && typeof value === 'object' && !Array.isArray(value)
}

/** @param {unknown} value @returns {Record<string, unknown>[]} */
function records(value) {
  return Array.isArray(value) ? value.filter(record) : []
}

/** @param {unknown} value @returns {string[]} */
function sharedPaths(value) {
  return Array.isArray(value)
    ? [...new Set(value.filter((path) => typeof path === 'string' && path !== ''))].sort()
    : []
}

/** @param {string[]} paths */
function sharedText(paths) {
  return [...paths.slice(0, 5), ...(paths.length > 5 ? [`+${paths.length - 5} more`] : [])].join(
    ', ',
  )
}

/** @param {string} a @param {string} b */
function compareIds(a, b) {
  return a.localeCompare(b, 'en', { numeric: true }) || (a < b ? -1 : a > b ? 1 : 0)
}

/**
 * Render planning notes after the scan has appended its open-PR notes. Other
 * planning notes retain their input order; overlap groups have stable ordering.
 * @param {unknown} result
 * @returns {string[]}
 */
export function renderPlanningLines(result) {
  if (!record(result) || !Array.isArray(result.notes)) return []
  const owners = [...records(result.edges), ...records(result.skipped)]
  const lines = []
  /** @type {Map<string, {paths: string[], identifiers: Set<string>}>} */
  const groups = new Map()
  /** @type {{number: number, text: string}[]} */
  const prs = []

  for (const note of records(result.notes)) {
    if (note.destination !== 'planning') continue
    if (note.reason === 'file-overlap') {
      if (typeof note.candidate !== 'string' || note.candidate === '') continue
      const owner =
        owners.find((row) => row.note === note) ??
        owners.find((row) => (row.identifier || row.id) === note.candidate)
      const paths = sharedPaths(owner?.shared)
      const key = JSON.stringify(paths)
      let group = groups.get(key)
      if (!group) {
        group = { paths, identifiers: new Set() }
        groups.set(key, group)
      }
      group.identifiers.add(note.candidate)
    } else if (note.reason === 'open-pr-overlap') {
      if (
        typeof note.pr !== 'number' ||
        !Number.isSafeInteger(note.pr) ||
        note.pr <= 0 ||
        typeof note.candidate !== 'string' ||
        note.candidate === '' ||
        (note.level !== 'file' && note.level !== 'directory')
      )
        continue
      const paths = sharedPaths(note.shared)
      prs.push({
        number: note.pr,
        text: `#${note.pr} for ${note.candidate} (${note.level} level${paths.length ? `; shared: ${sharedText(paths)}` : ''})`,
      })
    } else if (typeof note.text === 'string') {
      lines.push(note.text)
    }
  }

  const files = [...groups.values()]
    .map(({ paths, identifiers }) => ({ paths, ids: [...identifiers].sort(compareIds) }))
    .sort((a, b) => compareIds(a.ids[0], b.ids[0]))
    .map(
      ({ paths, ids }) =>
        `${ids.join(', ')}${paths.length ? ` (shared: ${sharedText(paths)})` : ''}`,
    )
  if (files.length) {
    lines.push(`Related, non-blocking file overlap (rebase risk only): ${files.join('; ')}.`)
  }
  if (prs.length) {
    prs.sort((a, b) => a.number - b.number || compareIds(a.text, b.text))
    lines.push(
      `Open PRs on the same files (rebase risk only): ${prs.map((pr) => pr.text).join('; ')}.`,
    )
  }
  return lines
}
