import { RECEIPT_CONTEXT } from '../commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS as D } from '../verify/verify-vocabulary.mjs'
import { parseParkDescription } from '../verify/verify-gate.mjs'
import { invokedSkill, STAGE_SKILL } from '../stage-chain.mjs'

export const HOLD_MINUTES = 15
export const JUDGE_GRACE_MINUTES = 30
export const CRON_STALE_MINUTES = 60
export const SECOND_MODEL_MIN_REVIEWS = 5
export const SECOND_MODEL_MIN_RAN_SHARE = 0.5
export const MAX_PRS = 200
export const MAX_HOLD_HEADS = 50
export const RATE_WINDOW_DAYS = 28
export const VERDICT_CLASSES = ['passed', 'approved', 'parked', 'sent-back', 'none']

/** @typedef {Record<string, any>} Data */
/** @typedef {{start?: string, end?: string}} Window */
/** @param {Data} connection @returns {Data[]} */
const nodes = (connection) => connection?.nodes ?? []
/** @param {Data} pr */
const commits = (pr) => nodes(pr.commits).map((entry) => entry.commit)
/** @param {Data | undefined} commit */
const contexts = (commit) => nodes(commit?.statusCheckRollup?.contexts)
/** @param {Data} status */
const successfulReceipt = (status) =>
  status.context === RECEIPT_CONTEXT && status.state?.toUpperCase() === 'SUCCESS'
/** @param {Data} pr */
export const factory = (pr) =>
  commits(pr).some((commit) => contexts(commit).some(successfulReceipt))
/** @param {Data[]} statuses */
const verifyStatuses = (statuses) => statuses.filter((status) => status.context === VERIFY_CONTEXT)
/** @param {string | number} value */
const time = (value) => (typeof value === 'number' ? value : Date.parse(value))
/** @param {Data[]} statuses */
const latest = (statuses) =>
  [...statuses].sort(
    (a, b) => time(b.createdAt ?? b.created_at) - time(a.createdAt ?? a.created_at),
  )[0]
/** @param {string | undefined | null} description */
export function verdictClass(description) {
  if (description === D.verified) return 'passed'
  if (description === D.verifiedApproved) return 'approved'
  if (description?.startsWith(D.needsHuman)) return 'parked'
  if ([D.defect, D.blocked].some((prefix) => description?.startsWith(prefix))) return 'sent-back'
  return 'none'
}
/** @param {Data} status */
const terminal = (status) => verdictClass(status?.description) !== 'none'
/** @param {Data} pr */
export function mergeVerdict(pr) {
  const statuses = contexts(commits(pr).find((commit) => commit.oid === pr.headRefOid))
  return {
    number: pr.number,
    mergedAt: pr.mergedAt,
    verifyDescription: latest(verifyStatuses(statuses))?.description ?? null,
  }
}
/** @param {Data[]} statuses */
export const hasTerminalVerdict = (statuses) => verifyStatuses(statuses).some(terminal)
/** @param {string} at @param {Window} [window] */
const inWindow = (at, { start, end } = {}) =>
  Boolean(at) && (!start || time(at) >= time(start)) && (!end || time(at) <= time(end))
/** @param {Data[]} rows */
const ok = (rows) => ({ status: 'ok', rows })

/** Receipt heads are the commits build or repair explicitly handed to verify. */
/** @param {Data[]} prs @param {Window} [window] */
export function receiptHeads(prs, window = {}) {
  return prs.filter(factory).flatMap((pr) =>
    commits(pr).flatMap((commit) => {
      const statuses = contexts(commit)
      const receipt = latest(statuses.filter(successfulReceipt))
      if (!receipt || !inWindow(receipt.createdAt, window)) return []
      return [{ number: pr.number, head: commit.oid, receiptAt: receipt.createdAt, statuses }]
    }),
  )
}

/** Factory heads whose latest verify status is an approved park. */
/** @param {Data[]} prs @param {Window} [window] */
export function approvedHeads(prs, window = {}) {
  return prs.filter(factory).flatMap((pr) =>
    commits(pr).flatMap((commit) => {
      const statuses = contexts(commit)
      const approval = latest(verifyStatuses(statuses))
      if (
        approval?.description !== D.verifiedApproved ||
        !inWindow(approval.createdAt ?? approval.created_at, window)
      )
        return []
      return [{ number: pr.number, head: commit.oid, statuses }]
    }),
  )
}

/** @param {Data[]} prs @param {Window} [window] */
export function mergesWithoutVerdict(prs, window = {}) {
  return ok(
    prs
      .filter((pr) => factory(pr) && inWindow(pr.mergedAt, window))
      .flatMap((pr) => {
        const statuses = contexts(commits(pr).find((commit) => commit.oid === pr.headRefOid))
        if (hasTerminalVerdict(statuses)) return []
        const description = latest(verifyStatuses(statuses))?.description ?? ''
        const headState = description.startsWith(D.verifying)
          ? 'verifying'
          : description.startsWith(D.waiting)
            ? 'waiting'
            : 'absent'
        return [
          {
            number: pr.number,
            title: pr.title,
            mergedAt: pr.mergedAt,
            head: pr.headRefOid,
            headState,
          },
        ]
      }),
  )
}

/** @param {Data[]} prs @param {{now?: string | number, graceMinutes?: number, window?: Window}} [options] */
export function unjudgedHeads(
  prs,
  { now = Date.now(), graceMinutes = JUDGE_GRACE_MINUTES, window = {} } = {},
) {
  const rows = []
  for (const pr of prs.filter(factory)) {
    const heads = receiptHeads([pr]).sort((a, b) => time(a.receiptAt) - time(b.receiptAt))
    for (const [index, head] of heads.entries()) {
      const ends = [now, window.end, heads[index + 1]?.receiptAt, pr.mergedAt, pr.closedAt]
        .filter((at) => at != null)
        .map(time)
      const end = Math.min(...ends)
      if (window.start && end <= time(window.start)) continue
      const minutes = (end - time(head.receiptAt)) / 60_000
      if (minutes < graceMinutes || hasTerminalVerdict(head.statuses)) continue
      rows.push({
        number: pr.number,
        head: head.head,
        receiptAt: head.receiptAt,
        minutes,
        lastVerify: latest(verifyStatuses(head.statuses))?.description ?? null,
      })
    }
  }
  return ok(rows)
}

/** @param {string} value @param {number} start @param {number} end */
const between = (value, start, end) => time(value) > start && time(value) < end
/** @param {Data} pr @param {number} start @param {number} end */
function touched(pr, start, end) {
  const review = nodes(pr.reviews).some(
    (entry) =>
      entry.author?.__typename === 'User' &&
      between(entry.submittedAt, start, end) &&
      (entry.state === 'CHANGES_REQUESTED' ||
        (entry.state === 'COMMENTED' && Boolean(entry.body?.trim()))),
  )
  return (
    review ||
    nodes(pr.reviewThreads).some((thread) =>
      nodes(thread.comments).some(
        (comment) =>
          comment.author?.__typename === 'User' && between(comment.createdAt, start, end),
      ),
    )
  )
}

/** @param {Data[]} heads @param {Data[]} prs @param {{holdMinutes?: number, window?: Window}} [options] */
export function unnecessaryHolds(heads, prs, { holdMinutes = HOLD_MINUTES, window = {} } = {}) {
  const rows = []
  /** @type {Record<string, number>} */
  const byCodes = {}
  for (const head of heads) {
    const pr = prs.find((pr) => pr.number === head.number)
    if (!pr || !factory(pr)) continue
    const statuses = verifyStatuses(head.statuses)
    const approval = latest(statuses.filter((status) => status.description === D.verifiedApproved))
    const parks = statuses
      .filter((status) => status.description?.startsWith(D.needsHuman))
      .sort((a, b) => time(a.createdAt ?? a.created_at) - time(b.createdAt ?? b.created_at))
    if (!approval || !parks.length) continue
    const start = time(parks[0].createdAt ?? parks[0].created_at)
    const end = time(approval.createdAt ?? approval.created_at)
    if (!inWindow(approval.createdAt ?? approval.created_at, window)) continue
    const minutes = (end - start) / 60_000
    if (
      !Number.isFinite(minutes) ||
      minutes < 0 ||
      minutes > holdMinutes ||
      touched(pr, start, end)
    )
      continue
    const park = parseParkDescription(parks[0].description)
    if (!park) continue
    const key = park.codes ? [...park.codes].sort().join(',') : park.codesText
    byCodes[key] = (byCodes[key] ?? 0) + 1
    rows.push({
      number: head.number,
      head: head.head,
      codes: park.codes,
      label: park.label,
      minutes,
    })
  }
  return { ...ok(rows), byCodes }
}

/** @param {Data[]} jobs @param {{now?: string | number, heads?: Data[]}} [options] */
export function verifyCronHealth(jobs, { now = Date.now(), heads = [] } = {}) {
  const rows = jobs
    .filter((job) => invokedSkill(job.prompt) === STAGE_SKILL.verify)
    .map((job) => {
      let health = 'ok'
      if (job.enabled === false) health = 'disabled'
      else if (['fire_failed', 'gate_failed'].includes(job.last_run_outcome)) health = 'failing'
      else if (
        !job.last_run_at ||
        (job.next_run_at && time(now) - time(job.next_run_at) > CRON_STALE_MINUTES * 60_000)
      )
        health = 'stale'
      return { ...job, health }
    })
  return {
    ...ok(rows),
    health: rows.length ? (rows.every((row) => row.health === 'ok') ? 'ok' : 'degraded') : 'none',
    verifySilent: heads.length > 0 && !heads.some((head) => hasTerminalVerdict(head.statuses)),
  }
}

/** @param {Data[]} prs */
export function secondModelHealth(prs) {
  let ran = 0,
    skipped = 0,
    error = 0,
    absent = 0
  /** @type {Record<string, number>} */
  const byAgent = {}
  /** @type {Record<string, number>} */
  const skipReasons = {}
  for (const pr of prs.filter(factory)) {
    const line = /^## Cross-model review\s*\r?\n([^\r\n]+)/m.exec(pr.body ?? '')?.[1]?.trim()
    const agent = /^ran \((.+)\)$/.exec(line ?? '')?.[1]
    if (agent) {
      ran++
      byAgent[agent] = (byAgent[agent] ?? 0) + 1
    } else if (line?.startsWith('skipped:')) {
      skipped++
      const reason = line.slice('skipped:'.length).trim()
      skipReasons[reason] = (skipReasons[reason] ?? 0) + 1
    } else if (line?.startsWith('error:')) error++
    else absent++
  }
  const reviewed = ran + skipped + error
  const health =
    reviewed >= SECOND_MODEL_MIN_REVIEWS && ran / reviewed < SECOND_MODEL_MIN_RAN_SHARE
      ? 'degraded'
      : 'ok'
  const topSkipReasons = Object.entries(skipReasons)
    .sort(([a, n], [b, m]) => m - n || a.localeCompare(b))
    .slice(0, 3)
    .map(([reason, count]) => ({ reason, count }))
  return { ...ok([]), health, reviewed, ran, skipped, error, absent, byAgent, topSkipReasons }
}

/** @param {Data[] | Data} records @param {Window} window */
export function escapeRows(records, window) {
  if (!Array.isArray(records)) return records
  const pairs = new Set()
  const rows = records
    .filter((record) => {
      const key = `${record.original.number}/${record.fix.number}`
      if (!inWindow(record.fix.mergedAt, window) || pairs.has(key)) return false
      pairs.add(key)
      return true
    })
    .sort((a, b) => time(b.fix.mergedAt) - time(a.fix.mergedAt))
    .map(({ original, fix, basis, ticket, areas, areaBasis, signals }) => ({
      original: { number: original.number, title: original.title, mergedAt: original.mergedAt },
      fix,
      basis,
      ...(ticket ? { ticket } : {}),
      areas,
      areaBasis,
      signals,
    }))
  return ok(rows)
}

/** @param {Data[] | Data} merges @param {Data[] | Data} records @param {Window} window */
export function laterFixRates(merges, records, window) {
  if (!Array.isArray(records)) return records
  if (!Array.isArray(merges)) return merges
  const originals = new Set(records.map((record) => record.original.number))
  const merged = new Map(
    merges
      .filter((merge) => inWindow(merge.mergedAt, window))
      .map((merge) => [merge.number, merge]),
  )
  const rows = VERDICT_CLASSES.map((verdict) => {
    const group = [...merged.values()].filter(
      (merge) => verdictClass(merge.verifyDescription) === verdict,
    )
    const escapes = group.filter((merge) => originals.has(merge.number)).length
    return {
      class: verdict,
      merges: group.length,
      escapes,
      rate: group.length ? escapes / group.length : null,
    }
  })
  return {
    ...ok(rows),
    by: 'verdict',
    window,
    unmatched: [...originals].filter((number) => !merged.has(number)).length,
  }
}

/**
 * Cap episodes overlapping the window: capped by its end and not cleared before its start.
 * A clear after the window end reads as active for that window.
 * @param {Data[] | Data} episodes @param {Window} [window]
 */
export function areaCaps(episodes, window = {}) {
  if (!Array.isArray(episodes)) return episodes
  const start = window.start ? time(window.start) : -Infinity
  const end = window.end ? time(window.end) : Infinity
  const rows = episodes
    .filter(
      (episode) =>
        time(episode.cappedAt) <= end && (!episode.clearedAt || time(episode.clearedAt) >= start),
    )
    .map(({ key, label, cappedAt, escapes = [], clearedAt, clearedBy, reason }) => {
      const cleared = Boolean(clearedAt) && time(clearedAt) <= end
      return {
        key,
        label,
        state: cleared ? 'cleared' : 'active',
        cappedAt,
        escapes: escapes.map((/** @type {Data} */ { original, fix }) => ({ original, fix })),
        ...(cleared ? { clearedAt, clearedBy, reason } : {}),
      }
    })
    .sort(
      (a, b) =>
        Number(a.state === 'cleared') - Number(b.state === 'cleared') ||
        time(b.cappedAt) - time(a.cappedAt) ||
        a.key.localeCompare(b.key),
    )
  return { ...ok(rows), active: rows.filter((row) => row.state === 'active').length }
}

/** @param {{window: Window, truncated?: boolean, sections?: Record<string, Data>}} parts */
export function buildReport({ window, truncated = false, sections = {} }) {
  return { v: 1, window, truncated, sections }
}

/** @type {Record<string, string>} */
const sectionTitles = {
  mergesWithoutVerdict: 'Merges without a verdict',
  unjudgedHeads: 'Heads never judged',
  unnecessaryHolds: 'Unnecessary holds',
  verifyCronHealth: 'Verify cron health',
  secondModelHealth: 'Second-model health',
  escapeRows: 'Escapes',
  laterFixRates: 'Later-fix rate',
  areaCaps: 'Area caps',
}
/** @param {unknown} value */
const oneLine = (value) => String(value ?? '').replace(/[\r\n]+/g, ' ')
/** @param {Data} row */
const display = (row) =>
  Object.entries(row)
    .map(
      ([key, value]) =>
        `${key}: ${oneLine(typeof value === 'object' ? JSON.stringify(value) : value)}`,
    )
    .join('; ')

/** @param {{window?: Window, truncated?: boolean, sections: Record<string, Data>}} report */
export function renderReport(report) {
  const lines = [
    '## Verify outcomes',
    '',
    `Window: ${oneLine(report.window?.start)} → ${oneLine(report.window?.end)}`,
  ]
  if (report.truncated) lines.push('Read limits reached; results are truncated.')
  const names = [
    ...Object.keys(sectionTitles),
    ...Object.keys(report.sections).filter((key) => !(key in sectionTitles)),
  ]
  for (const name of names) {
    const section = report.sections[name]
    if (!section) continue
    lines.push('', `### ${sectionTitles[name] ?? oneLine(name)}`, '')
    if (section.status !== 'ok') {
      lines.push(
        `${oneLine(section.status)}: ${oneLine(section.reason)}${section.detail ? ` (${oneLine(section.detail)})` : ''}`,
      )
      continue
    }
    if (section.health) lines.push(`Health: ${section.health}`)
    if (section.verifySilent)
      lines.push('Verify silent: receipt heads in this window have no terminal verdict.')
    if (name === 'secondModelHealth')
      lines.push(
        `Ran: ${section.ran}; skipped: ${section.skipped}; error: ${section.error}; absent: ${section.absent}.`,
        `Agents: ${display(section.byAgent ?? {}) || 'none'}.`,
        ...(section.topSkipReasons ?? []).map((/** @type {Data} */ row) => `- ${display(row)}`),
      )
    const rows = section.rows ?? []
    if (!rows.length && !section.health) lines.push('0 rows.')
    if (name === 'laterFixRates') lines.push(`Unmatched: ${section.unmatched}`)
    if (name === 'escapeRows' && Object.keys(section.skipped ?? {}).length)
      lines.push(`Skipped: ${oneLine(JSON.stringify(section.skipped))}`)
    lines.push(
      ...rows.slice(0, 10).map((/** @type {Data} */ row) => {
        if (name === 'escapeRows') {
          const { findings, secondModel, size, verify } = row.signals
          return `- #${row.original.number} ← #${row.fix.number} (${oneLine(row.basis)}); areas: ${row.areas.map((/** @type {Data} */ area) => oneLine(area.area)).join(', ')}; blocking: ${findings.blocking}; unapplied: ${findings.unapplied}; max severity: ${oneLine(findings.maxSeverity ?? 'none')}; second model: ${oneLine(secondModel)}; +${size.additions}/-${size.deletions} across ${size.changedFiles} files; verify: ${oneLine(verify)}`
        }
        if (name === 'laterFixRates')
          return `- ${row.class}: merges: ${row.merges}; escapes: ${row.escapes}; rate: ${row.rate === null ? 'n/a' : `${(row.rate * 100).toFixed(2).replace(/\.?0+$/, '')}%`}`
        if (name === 'areaCaps') {
          const state =
            row.state === 'cleared'
              ? `cleared ${oneLine(row.clearedAt)} by ${oneLine(row.clearedBy)}: ${oneLine(row.reason)}`
              : 'active'
          const escapes = row.escapes
            .map((/** @type {Data} */ e) => `#${e.original} (fixed by #${e.fix})`)
            .join(', ')
          return `- ${oneLine(row.label ?? row.key)} (capped ${oneLine(row.cappedAt)}): ${state}; escapes ${escapes || 'none'}`
        }
        return `- ${display(row)}`
      }),
    )
    if (rows.length > 10) lines.push(`+${rows.length - 10} more`)
    if (name === 'unnecessaryHolds' && Object.keys(section.byCodes ?? {}).length)
      lines.push(`By codes: ${display(section.byCodes)}`)
  }
  return `${lines.join('\n')}\n`
}

/** @param {{window?: Window, truncated?: boolean, sections: Record<string, Data>}} report */
export function outcomeCounts(report) {
  return {
    window: report.window,
    truncated: report.truncated,
    sections: Object.fromEntries(
      Object.entries(report.sections).map(([name, section]) => [
        name,
        section.status === 'ok'
          ? {
              status: section.status,
              count:
                name === 'laterFixRates'
                  ? (section.rows ?? []).reduce(
                      (/** @type {number} */ sum, /** @type {Data} */ row) => sum + row.merges,
                      0,
                    )
                  : (section.rows?.length ?? 0),
              ...(name === 'areaCaps' ? { active: section.active } : {}),
            }
          : { status: section.status },
      ]),
    ),
  }
}
