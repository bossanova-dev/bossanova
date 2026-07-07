#!/usr/bin/env node
// Deterministic expected-fire counter for cron audit reports.
// Supports the 5-field schedules used by bossd plus @hourly/@daily/@weekly.

const MACROS = new Map([
  ['@hourly', '0 * * * *'],
  ['@daily', '0 0 * * *'],
  ['@weekly', '0 0 * * 0'],
])

function parseField(field, min, max, { allowSevenAsSunday = false } = {}) {
  const values = new Set()
  for (const rawPart of field.split(',')) {
    const part = rawPart.trim()
    if (!part) return null
    const [rangePart, stepPart] = part.split('/')
    if (part.split('/').length > 2) return null
    const step = stepPart === undefined ? 1 : Number(stepPart)
    if (!Number.isInteger(step) || step <= 0) return null

    let start
    let end
    if (rangePart === '*') {
      start = min
      end = max
    } else if (rangePart.includes('-')) {
      const [a, b, ...rest] = rangePart.split('-')
      if (rest.length) return null
      start = Number(a)
      end = Number(b)
    } else {
      start = Number(rangePart)
      end = start
    }

    if (allowSevenAsSunday) {
      if (start === 7) start = 0
      if (end === 7) end = 0
    }
    if (!Number.isInteger(start) || !Number.isInteger(end)) return null
    if (start < min || start > max || end < min || end > max) return null
    if (start > end) return null
    for (let n = start; n <= end; n += step) values.add(n)
  }
  return values
}

function parseSchedule(schedule) {
  const spec = MACROS.get(String(schedule ?? '').trim()) ?? String(schedule ?? '').trim()
  const fields = spec.split(/\s+/).filter(Boolean)
  if (fields.length !== 5) return null
  const [minute, hour, dom, month, dow] = fields
  const parsed = {
    minute: parseField(minute, 0, 59),
    hour: parseField(hour, 0, 23),
    dom: parseField(dom, 1, 31),
    month: parseField(month, 1, 12),
    dow: parseField(dow, 0, 6, { allowSevenAsSunday: true }),
    domAny: dom === '*',
    dowAny: dow === '*',
  }
  return Object.values(parsed).some((v) => v === null) ? null : parsed
}

function startMinute(date) {
  const ms = date.getTime()
  const minute = 60_000
  return new Date(ms % minute === 0 ? ms : ms + (minute - (ms % minute)))
}

function dayMatches(parsed, date) {
  const domMatch = parsed.dom.has(date.getUTCDate())
  const dowMatch = parsed.dow.has(date.getUTCDay())
  if (parsed.domAny || parsed.dowAny) return domMatch && dowMatch
  return domMatch || dowMatch
}

function matches(parsed, date) {
  return (
    parsed.minute.has(date.getUTCMinutes()) &&
    parsed.hour.has(date.getUTCHours()) &&
    parsed.month.has(date.getUTCMonth() + 1) &&
    dayMatches(parsed, date)
  )
}

export function expectedFires(schedule, fromISO, toISO) {
  const parsed = parseSchedule(schedule)
  const from = new Date(fromISO)
  const to = new Date(toISO)
  if (
    parsed === null ||
    Number.isNaN(from.getTime()) ||
    Number.isNaN(to.getTime()) ||
    from.getTime() >= to.getTime()
  ) {
    return null
  }

  let count = 0
  for (let t = startMinute(from); t.getTime() < to.getTime(); t = new Date(t.getTime() + 60_000)) {
    if (matches(parsed, t)) count += 1
  }
  return count
}

if (import.meta.url === `file://${process.argv[1]}`) {
  const [schedule, from, to] = process.argv.slice(2)
  const count = expectedFires(schedule, from, to)
  if (count === null) {
    process.exitCode = 2
  }
  console.log(count === null ? 'null' : String(count))
}
