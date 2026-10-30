// Pure sweep settlement signals: quiet cron ticks and handled event heads.

/**
 * Was this tick quiet, so an unchanged next tick may skip? Discovery was complete (`skippable`) and
 * found no candidate and no `unknown:` skip, nothing was undispatchable, and every record is a
 * remote-session skip or a merge retry that settled as a write-free wait.
 */
export function tickWasQuiet(payload, records, undispatchable) {
  if (
    payload?.skippable !== true ||
    !(typeof payload?.fingerprint === 'string' && payload.fingerprint.trim() !== '')
  )
    return false
  if (!Array.isArray(payload.candidates) || payload.candidates.length > 0) return false
  const skipped = Array.isArray(payload.skipped) ? payload.skipped : []
  if (skipped.some((s) => String(s?.reason ?? '').startsWith('unknown:'))) return false
  if (undispatchable) return false
  return records.every((r) => r?.remote === true || r?.quiet === true)
}

/** An event head is settled only after deliberate exclusion or a completed handling route. */
export function eventWasSettled(payload, records, undispatchable) {
  return (
    !undispatchable &&
    !(payload.skipped ?? []).some((s) => String(s?.reason ?? '').startsWith('unknown:')) &&
    records.every(
      (r) =>
        r.remote === true ||
        (r.discovery === true && r.quiet === true) ||
        ['merged', 'parked', 'dispatched'].includes(r.outcome),
    )
  )
}
