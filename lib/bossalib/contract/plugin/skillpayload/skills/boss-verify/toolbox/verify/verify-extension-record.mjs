// The extension record: what a clean extension run concluded on a head, so an approval of the park
// it led to can settle without running the extensions again. Serialized as `<name>:<outcome>,…`,
// where an outcome is `pass`, `abstain` or the humanReasonsAllowed code the extension parked on.
const RECORD_NAME = /^[A-Za-z0-9._-]+$/

/**
 * The record of an aggregate's outcomes, or null when the run is not recordable: any malfunction
 * (an optional extension's included), finding or wait, or a name the record cannot carry.
 * @returns {string | null}
 */
export function extensionRecord(agg, humanReasonsAllowed) {
  if (agg.failed.length > 0 || agg.findings.length > 0 || agg.waitReason) return null
  const entries = []
  for (const o of agg.outcomes) {
    if (!RECORD_NAME.test(o.extension)) return null
    if (o.outcome === 'abstain' && o.reason === 'abstain') entries.push(`${o.extension}:abstain`)
    else if (o.outcome === 'ok' && o.verdict === 'pass')
      entries.push(`${o.extension}:${o.recorded === true ? 'recorded' : 'pass'}`)
    else if (
      o.outcome === 'ok' &&
      o.verdict === 'human' &&
      humanReasonsAllowed.includes(o.humanReason)
    )
      entries.push(`${o.extension}:${o.humanReason}`)
    else return null
  }
  return entries.length > 0 ? entries.join(',') : null
}

/**
 * Parses a serialized extension record into `[{extension, outcome}]`, or null when any entry is
 * malformed, a name repeats, or the record is empty: a partly understood record is never reused.
 */
export function parseExtensionRecord(value, humanReasonsAllowed) {
  if (typeof value !== 'string' || value === '') return null
  const judged = []
  for (const entry of value.split(',')) {
    const at = entry.indexOf(':')
    const extension = entry.slice(0, at)
    const outcome = entry.slice(at + 1)
    if (at < 1 || !RECORD_NAME.test(extension) || judged.some((e) => e.extension === extension))
      return null
    if (
      outcome !== 'pass' &&
      outcome !== 'recorded' &&
      outcome !== 'abstain' &&
      !humanReasonsAllowed.includes(outcome)
    )
      return null
    judged.push({ extension, outcome })
  }
  return judged
}

/**
 * The aggregate a recorded run stands for, judged against the extensions that apply now:
 * `{outcomes, humanReasons}`, or null when an applicable extension is missing from the record (it
 * was added since, so it has not judged this head). Entries for an extension whose check now says
 * it does not apply are dropped, as its results would be.
 */
export function reuseExtensionRecord(judged, applicable, notApplicable = [], humanReasonsAllowed) {
  if (!Array.isArray(judged)) return null
  const recorded = new Set(judged.map((e) => e.extension))
  if (!applicable.every((ext) => recorded.has(ext?.name))) return null
  const skip = new Set(notApplicable)
  const kept = judged.filter((e) => !skip.has(e.extension))
  const outcomes = kept.map(({ extension, outcome }) =>
    outcome === 'abstain'
      ? { extension, outcome: 'abstain', reason: 'abstain', verdict: null }
      : outcome === 'pass' || outcome === 'recorded'
        ? {
            extension,
            outcome: 'ok',
            reason: 'pass',
            verdict: 'pass',
            ...(outcome === 'recorded' ? { recorded: true } : {}),
          }
        : { extension, outcome: 'ok', reason: 'human', verdict: 'human', humanReason: outcome },
  )
  const humanReasons = humanReasonsAllowed.filter((code) => kept.some((e) => e.outcome === code))
  return { outcomes, humanReasons }
}
