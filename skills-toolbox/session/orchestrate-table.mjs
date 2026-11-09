// The orchestrate snapshot as the attended chat reads it: the rows that need action first, then
// the PRs waiting on the operator, the done PRs, shared reds, extension items, an open outage
// record, errors and the extension ledger.

const ACTION_ORDER = [
  'question-chat',
  'waive-cap',
  'unblock',
  'stalled-chat',
  'upstream-red',
  'mark-ready',
  'refresh-park',
  'ask-operator',
  'repair',
  'verify',
  'stalled',
  'stale-session',
  'grace',
  'wait-ci',
  'waiting',
  'working',
  'held',
  'awaiting-operator',
  'finalizing',
  'ok',
  'dependabot',
  'done',
  'skip',
]

export function printTable({
  rows,
  errors,
  sharedReds: shared = [],
  rules = [],
  operatorItems = [],
  ledger = [],
  outage = null,
}) {
  const sorted = rows
    .filter((row) => row.action !== 'skip')
    .sort((a, b) => ACTION_ORDER.indexOf(a.action) - ACTION_ORDER.indexOf(b.action))
  const counts = {}
  for (const row of sorted) counts[row.action] = (counts[row.action] ?? 0) + 1
  const short = (row) =>
    row.pr ? `${String(row.repo).split('/').pop()}#${row.pr}` : String(row.repo)
  for (const row of sorted) {
    if (row.action === 'done' || row.action === 'awaiting-operator') continue
    process.stdout.write(
      `${row.action.padEnd(13)} ${row.host.padEnd(7)} ${row.session} ${row.state.padEnd(18)} ${short(row)}\n` +
        `    ${(row.title ?? '').slice(0, 90)}\n    why: ${row.why}\n` +
        (row.classifiedBy ? `    classified by: ${row.classifiedBy}\n` : '') +
        (row.next ? `    next: ${row.next}\n` : ''),
    )
  }
  // Parked and done rows are re-judged every round; list them so a regression or a gap shows.
  const list = (action) => sorted.filter((row) => row.action === action)
  const awaiting = list('awaiting-operator')
  if (awaiting.length) {
    process.stdout.write(`\nwaiting on the operator (verified as far as policy allows):\n`)
    for (const row of awaiting)
      // gate-region-ok: a display cap on one reason line, not a section window.
      process.stdout.write(`    ${short(row).padEnd(22)} ${row.why.slice(0, 100)}\n`)
  }
  const done = list('done')
  if (done.length)
    process.stdout.write(`\ndone (verified, ready for review): ${done.map(short).join(' ')}\n`)
  for (const group of shared) {
    const prs = group.prs.map((p) => `#${p.pr} (${p.session}, ${p.action})`).join(', ')
    const origin =
      (group.inherited ? ' (inherited from the base, per a repair residual)' : '') +
      (group.infra ? ' (infrastructure: an outage, not a fix; see orchestrate-outage.mjs)' : '')
    process.stdout.write(
      `\nshared red: ${group.repo} check "${group.check}" fails on ${prs}${origin}; elect one fixer, hold the rest\n`,
    )
  }
  if (operatorItems.length) {
    process.stdout.write(`\nfrom extensions, for the operator:\n`)
    for (const item of operatorItems)
      process.stdout.write(`    ${item.title} (${item.extension})\n      ${item.detail}\n`)
  }
  for (const rule of rules) process.stdout.write(`\nrules: ${rule.name} ${rule.skillPath}\n`)
  process.stdout.write(
    `\nsummary: ${Object.entries(counts)
      .map(([k, v]) => `${k}=${v}`)
      .join(' ')}\n`,
  )
  if (outage)
    process.stdout.write(
      `\noutage paused since ${outage.since} (${outage.reason}): ${outage.crons?.length ?? 0} cron(s) off, ${outage.sessions?.length ?? 0} session(s) held; probe with orchestrate-outage.mjs status, then resume\n`,
    )
  for (const error of errors) process.stdout.write(`error: ${error}\n`)
  for (const line of ledger) process.stdout.write(`${line}\n`)
}
