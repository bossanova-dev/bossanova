import { isSecretBearing } from '../bs-record-notes.mjs'

export const CAP_WINDOW_DAYS = 7
export const CAP_ESCAPES = 2

/** @typedef {{area:string, paths:string[]}} HeadArea */
/** @typedef {{original:{number:number}, fix:{number:number, mergedAt:string}, levelAtMerge:string|null, areas:HeadArea[]}} EscapeInput */
/** @typedef {{original:number, fix:number, fixMergedAt:string}} CapEscape */
/** @typedef {{key:string, label:string, cappedAt:string, escapes:CapEscape[]}} AreaCap */
/** @typedef {{v:1, repo:string, key:string, clearedAt:string, clearedBy:string, reason:string, through:number}} CapClear */
/** @typedef {AreaCap & {clearedAt?:string, clearedBy?:string, reason?:string}} CapEpisode */
/** @typedef {{escapes:EscapeInput[], clears?:CapClear[]}} CapInput */
/** @typedef {{area:string, label:string, paths:string[], defaultAsk:'always', source:'cap', cap:AreaCap}} CappedArea */

/** @param {string} area @param {string[]} paths */
export function capKeys(area, paths) {
  if (area !== 'other') return [area]
  return [
    ...new Set(
      paths.map((path) => {
        const directories = path.split('/').slice(0, -1)
        return `other:${directories.slice(0, 2).join('/') || '.'}`
      }),
    ),
  ].sort()
}

/** @param {string} key */
const labelFor = (key) => (key.startsWith('other:') ? key.substring(6) : key)
/** @param {CapEscape} a @param {CapEscape} b */
const compareEscape = (a, b) =>
  Date.parse(a.fixMergedAt) - Date.parse(b.fixMergedAt) ||
  a.original - b.original ||
  a.fix - b.fix ||
  a.fixMergedAt.localeCompare(b.fixMergedAt)
/** @param {CapClear} a @param {CapClear} b */
const compareClear = (a, b) =>
  Date.parse(a.clearedAt) - Date.parse(b.clearedAt) ||
  JSON.stringify(a).localeCompare(JSON.stringify(b))

/** @param {EscapeInput[]} escapes */
function groupedEscapes(escapes) {
  /** @type {Map<string, CapEscape[]>} */
  const grouped = new Map()
  for (const escape of escapes) {
    if (escape.levelAtMerge !== 'high' || !Number.isFinite(Date.parse(escape.fix.mergedAt)))
      continue
    const keys = new Set(escape.areas.flatMap(({ area, paths }) => capKeys(area, paths)))
    for (const key of keys) {
      const entries = grouped.get(key) ?? []
      entries.push({
        original: escape.original.number,
        fix: escape.fix.number,
        fixMergedAt: escape.fix.mergedAt,
      })
      grouped.set(key, entries)
    }
  }
  return grouped
}

/** @param {string} key @param {CapEscape[]} entries @returns {AreaCap|null} */
function deriveCap(key, entries) {
  const ordered = [...entries].sort(compareEscape)
  const seen = new Set()
  const counted = ordered.filter((escape) => {
    if (seen.has(escape.original)) return false
    seen.add(escape.original)
    return true
  })
  const trigger = counted.find(
    (escape, index) =>
      index > 0 &&
      Date.parse(escape.fixMergedAt) - Date.parse(counted[index - 1].fixMergedAt) <=
        CAP_WINDOW_DAYS * 86400000,
  )
  return trigger
    ? { key, label: labelFor(key), cappedAt: trigger.fixMergedAt, escapes: counted.reverse() }
    : null
}

/** @param {CapInput} input @returns {AreaCap[]} */
export function activeCaps({ escapes, clears = [] }) {
  /** @type {AreaCap[]} */
  const caps = []
  for (const [key, entries] of groupedEscapes(escapes)) {
    const latest = clears
      .filter((clear) => clear.key === key)
      .sort(compareClear)
      .at(-1)
    const cap = deriveCap(
      key,
      entries.filter(
        (escape) => !latest || Date.parse(escape.fixMergedAt) > Date.parse(latest.clearedAt),
      ),
    )
    if (cap) caps.push(cap)
  }
  return caps.sort((a, b) => a.key.localeCompare(b.key))
}

/** @param {CapInput} input @returns {CapEpisode[]} */
export function capHistory({ escapes, clears = [] }) {
  /** @type {CapEpisode[]} */
  const episodes = []
  for (const [key, entries] of groupedEscapes(escapes)) {
    let boundary = -Infinity
    for (const clear of clears.filter((clear) => clear.key === key).sort(compareClear)) {
      const end = Date.parse(clear.clearedAt)
      const cap = deriveCap(
        key,
        entries.filter(
          (escape) =>
            Date.parse(escape.fixMergedAt) > boundary && Date.parse(escape.fixMergedAt) <= end,
        ),
      )
      if (cap)
        episodes.push({
          ...cap,
          clearedAt: clear.clearedAt,
          clearedBy: clear.clearedBy,
          reason: clear.reason,
        })
      boundary = end
    }
    const cap = deriveCap(
      key,
      entries.filter((escape) => Date.parse(escape.fixMergedAt) > boundary),
    )
    if (cap) episodes.push(cap)
  }
  return episodes.sort(
    (a, b) => Date.parse(a.cappedAt) - Date.parse(b.cappedAt) || a.key.localeCompare(b.key),
  )
}

/** @param {HeadArea[]} headAreas @param {AreaCap[]} caps @returns {CappedArea[]} */
export function capAreas(headAreas, caps) {
  /** @type {CappedArea[]} */
  const result = []
  for (const cap of [...caps].sort((a, b) => a.key.localeCompare(b.key))) {
    const matching = headAreas.filter(({ area, paths }) => capKeys(area, paths).includes(cap.key))
    if (!matching.length || result.some((area) => area.cap.key === cap.key)) continue
    const paths = [
      ...new Set(
        matching.flatMap((area) =>
          area.paths.filter((path) => capKeys(area.area, [path]).includes(cap.key)),
        ),
      ),
    ].sort()
    result.push({
      area: `capped:${cap.key}`,
      label: `${cap.label} (capped)`,
      paths,
      defaultAsk: 'always',
      source: 'cap',
      cap,
    })
  }
  return result
}

/** @param {AreaCap} cap */
export function capReason(cap) {
  const escapes = cap.escapes
    .map((escape) => `#${escape.original} (fixed by #${escape.fix})`)
    .join(' and ')
  return `${cap.label} is capped at Moderate since ${cap.cappedAt.substring(0, 10)} after escapes ${escapes}. An operator clearing the cap would raise this.`
}

/** @param {unknown} value @returns {value is CapClear} */
function validClear(value) {
  if (!value || typeof value !== 'object') return false
  const r = /** @type {Record<string, unknown>} */ (value)
  return (
    r.v === 1 &&
    typeof r.repo === 'string' &&
    /^[\w.-]+\/[\w.-]+$/.test(r.repo) &&
    typeof r.key === 'string' &&
    /^[^\x00-\x1f<>]+$/.test(r.key) &&
    typeof r.clearedAt === 'string' &&
    Number.isFinite(Date.parse(r.clearedAt)) &&
    typeof r.clearedBy === 'string' &&
    /^[\w-]+$/.test(r.clearedBy) &&
    typeof r.reason === 'string' &&
    !isSecretBearing(r.reason) &&
    typeof r.through === 'number' &&
    Number.isSafeInteger(r.through) &&
    r.through > 0
  )
}

/** @param {CapClear} clear */
export function renderClearComment(clear) {
  const record = { ...clear, reason: isSecretBearing(clear.reason) ? '' : clear.reason }
  if (!validClear(record)) throw new Error('Invalid area cap clear record')
  const payload = JSON.stringify(record).replaceAll('<', '\\u003c').replaceAll('>', '\\u003e')
  return `Area cap ${clear.key} cleared by ${clear.clearedBy}.${record.reason ? ` Reason: ${record.reason.replaceAll('<', '&lt;').replaceAll('>', '&gt;')}` : ''}\n\n<!-- boss-area-cap-clear v1 key=${clear.key} -->\n<!-- boss-area-cap-clear-record: ${payload} -->`
}

/** @param {string} body @returns {CapClear|null} */
export function parseClearComment(body) {
  try {
    const markers = [...body.matchAll(/<!-- boss-area-cap-clear v1 key=([^\r\n<>]+) -->/g)]
    const payloads = [...body.matchAll(/<!-- boss-area-cap-clear-record: (\{[^\n]*\}) -->/g)]
    if (markers.length !== 1 || payloads.length !== 1) return null
    const record = /** @type {unknown} */ (JSON.parse(payloads[0][1]))
    return validClear(record) && record.key === markers[0][1] ? record : null
  } catch {
    return null
  }
}
