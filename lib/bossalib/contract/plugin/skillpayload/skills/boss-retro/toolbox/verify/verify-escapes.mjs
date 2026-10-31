import { detectAreas } from './verify-areas.mjs'
import { isSecretBearing } from '../bs-record-notes.mjs'

export const BASES = Object.freeze(['revert', 'explicit-reference', 'followup-ticket', 'judge'])
export const REFERENCE_PATTERNS = Object.freeze([
  Object.freeze({
    name: 'fix-verb',
    pattern:
      /(?:^|\band\s+)(?:(?:this|it|also)\s+)?(?:fix(?:es|ed)?|repair(?:s|ed)?|correct(?:s|ed)?)\s+(?!(?:by|on|there|is|also|had|push|pull|round)\b)[^\n]*?(?:#\d+|https:\/\/github\.com\/[^\s]+\/pull\/\d+)/i,
  }),
  Object.freeze({
    name: 'defect-origin',
    pattern:
      /\b(?:regression|defect|bug|breakage)\s+(?:from|in|introduced in|introduced by|caused by)\s+(?:#\d+|https:\/\/github\.com\/[^\s]+\/pull\/\d+)/i,
  }),
  Object.freeze({
    name: 'provenance',
    pattern:
      /\b(?:added|introduced|shipped|broken|regressed)\s+(?:in|by)\s+(?:#\d+|https:\/\/github\.com\/[^\s]+\/pull\/\d+)/i,
  }),
])

/** @param {string} text */
function safeEvidence(text) {
  if (isSecretBearing(text)) return ''
  // gate-region-ok: The record caps a display excerpt; this does not select a source section.
  return text.slice(0, 200)
}
/** @param {string} text */
function sentences(text = '') {
  // A period in a GitHub hostname is not a sentence boundary.
  return text
    .split(/(?<=[.!?])\s+|\n+/)
    .map((s) => s.trim())
    .filter(Boolean)
}
/** @param {string} text */
export function extractReferences(text = '') {
  text = text.replace(
    /\b(?:known\s+)?(?:findings?|recs?|recommendations?|items?|notes?)\s*#\d+\b/gi,
    '',
  )
  return [
    ...text.matchAll(
      /https:\/\/github\.com\/([\w.-]+\/[\w.-]+)\/pull\/(\d+)\b|(?:([\w.-]+\/[\w.-]+))?#(\d+)\b/g,
    ),
  ].map((m) => ({ number: Number(m[2] ?? m[4]), repo: m[1] ?? m[3] ?? null }))
}
/** @param {string} text */
const keys = (text) => [...new Set(text.match(/\b[A-Z][A-Z0-9]*-\d+\b/g) ?? [])]
/** @param {any} pr */
export function ticketKeys(pr) {
  const prefix = pr.title?.match(/^\[([^\]]+)\]/)?.[1] ?? ''
  const tracker = pr.body?.match(/^Tracker issue:\s*(\S+)/m)?.[1] ?? ''
  const closing = pr.closingIssuesReferences?.nodes ?? pr.closingIssuesReferences ?? []
  return keys([prefix, tracker, ...closing.map((i) => `${i.title ?? ''} ${i.url ?? ''}`)].join(' '))
}
/** @param {string} description */
export function followupReferences(description = '') {
  return sentences(description)
    .filter((s) =>
      /\bverified\b[^.!?\n]*?\bP[0-3]\b|\b(?:independent|code) review\b[^.!?\n]*?\bP[0-3]\b|\bverified\s+(?:(?:non-?blocking|introduced)\s+)?(?:review\s+)?findings?\b|\b(?:review\s+findings?|found\s+(?:as|in|while))\b[^.!?\n]*?\b(?:introduced|defect|regression|bug)\b/i.test(
        s,
      ),
    )
    .map((evidence) => ({
      evidence,
      references: extractReferences(evidence),
      tickets: keys(evidence),
      severity: evidence.match(/\bP[0-3]\b/)?.[0],
    }))
}
/** @param {any} pr */
const isFix = (pr) =>
  /^(?:\[[^\]]+\]\s*)?fix(?:\b|\()/i.test(pr.title ?? '') ||
  (pr.labels?.nodes ?? pr.labels ?? []).some((l) => (typeof l === 'string' ? l : l.name) === 'bug')
/** @param {any} fix @param {any} original */
function eligible(fix, original) {
  return (
    original &&
    original.number !== fix.number &&
    original.mergedAt &&
    fix.mergedAt &&
    Date.parse(original.mergedAt) < Date.parse(fix.mergedAt) &&
    (original.repo ?? fix.repo) === fix.repo &&
    original.baseRefName === (fix.defaultBranch ?? 'main') &&
    fix.baseRefName === (fix.defaultBranch ?? 'main')
  )
}
/** @param {{fix:any, originals:any[], tickets?:Record<string,string>, ticketPrs?:Record<string,any[]>}} input */
export function linkFix({ fix, originals, tickets = {}, ticketPrs = {} }) {
  /** @type {Map<number, any>} */
  const links = new Map()
  /** @param {any} ref @param {string} basis @param {string} evidence @param {object} extra */
  const add = (ref, basis, evidence, extra = {}) => {
    const original = originals.find((o) => o.number === ref.number)
    if ((ref.repo && ref.repo !== fix.repo) || !eligible(fix, original)) return
    const prior = links.get(original.number)
    if (!prior || BASES.indexOf(basis) < BASES.indexOf(prior.basis))
      links.set(original.number, { original, basis, evidence, ...extra })
  }
  const reverted = fix.title?.match(/^Revert ".* \(#(\d+)\)"/)?.[1]
  if (reverted) add({ number: Number(reverted) }, 'revert', fix.title)
  for (const sentence of sentences(fix.body)) {
    for (const match of sentence.matchAll(/Reverts ([\w.-]+\/[\w.-]+)#(\d+)/gi))
      add({ repo: match[1], number: Number(match[2]) }, 'revert', sentence)
  }
  for (const sentence of sentences(`${fix.title ?? ''}\n${fix.body ?? ''}`)) {
    for (const { name, pattern } of REFERENCE_PATTERNS) {
      if (name === 'provenance' && !isFix(fix)) continue
      const match = sentence.match(pattern)
      if (!match || (name === 'fix-verb' && /\b(?:for example|such as)\b|\be\.g\./i.test(match[0])))
        continue
      // Include coordinated targets ("fixes #1 and #2"), without absorbing a later bare mention.
      const tail = sentence.slice((match.index ?? 0) + match[0].length)
      const coordinated =
        tail.match(
          /^(?:\s*(?:,\s*(?:and\s+)?|and\s+|\/)\s*(?:#[0-9]+|https:\/\/github\.com\/[\w.-]+\/[\w.-]+\/pull\/[0-9]+))+/,
        )?.[0] ?? ''
      for (const reference of extractReferences(match[0] + coordinated))
        add(reference, 'explicit-reference', sentence, { pattern: name })
    }
  }
  for (const [ticket, description] of Object.entries(tickets))
    for (const sentence of followupReferences(description)) {
      const extra = { ticket, ...(sentence.severity ? { severity: sentence.severity } : {}) }
      for (const ref of sentence.references) add(ref, 'followup-ticket', sentence.evidence, extra)
      for (const key of sentence.tickets) {
        const matches = ticketPrs[key] ?? []
        if (matches.length === 1) add(matches[0], 'followup-ticket', sentence.evidence, extra)
      }
    }
  return [...links.values()].sort((a, b) => a.original.number - b.original.number)
}
/** @param {any} pr */
const paths = (pr) =>
  (pr.files?.nodes ?? pr.files ?? []).map((f) => (typeof f === 'string' ? f : f.path))
/** @param {{fix:any, originals:any[]}} input */
export function weakCandidates({ fix, originals }) {
  if (!isFix(fix)) return []
  const linked = new Set(linkFix({ fix, originals }).map((l) => l.original.number))
  const refs = extractReferences(`${fix.title}\n${fix.body ?? ''}`)
  return originals
    .filter(
      (o) =>
        !linked.has(o.number) &&
        eligible(fix, o) &&
        refs.some((r) => r.number === o.number && (!r.repo || r.repo === fix.repo)) &&
        paths(o).some((p) => paths(fix).includes(p)),
    )
    .map((o) => {
      const evidence =
        sentences(fix.body).find((s) => extractReferences(s).some((r) => r.number === o.number)) ??
        fix.title
      return {
        fix: fix.number,
        original: o.number,
        evidence: safeEvidence(evidence),
      }
    })
}
/** @param {string} body @param {string} heading */
function section(body, heading) {
  return (
    body.match(new RegExp(`^## ${heading}\\s*\\n([\\s\\S]*?)(?=^## |$(?![\\s\\S]))`, 'm'))?.[1] ??
    ''
  )
}
/** @param {any} pr */
export function escapeSignals(pr) {
  const body = pr.body ?? ''
  const blocking = section(body, 'Review findings')
    .split('\n')
    .filter((s) => /^- \[ \]/.test(s))
  const unapplied = section(body, 'Unapplied review findings')
    .split('\n')
    .filter((s) => /^- \[ \]/.test(s))
  const severities = [...blocking, ...unapplied]
    .flatMap((s) => [...s.matchAll(/\*\*(P[0-3])\*\*/g)].map((m) => m[1]))
    .sort()
  const second = section(body, 'Cross-model review').trim()
  return {
    findings: {
      blocking: blocking.length,
      unapplied: unapplied.length,
      maxSeverity: severities[0] ?? null,
    },
    secondModel: /^ran\b/.test(second) ? 'ran' : /^skipped\b/.test(second) ? 'skipped' : 'unknown',
    size: {
      additions: pr.additions ?? 0,
      deletions: pr.deletions ?? 0,
      changedFiles: pr.changedFiles ?? paths(pr).length,
    },
    verify: pr.verifyState ?? 'absent',
    proof: null,
  }
}
/** @param {any} original @param {any} fix */
export function escapeAreas(original, fix) {
  const overlap = paths(original).filter((p) => paths(fix).includes(p))
  return {
    areas: detectAreas(overlap.length ? overlap : paths(original)).map(({ area, paths }) => ({
      area,
      paths,
    })),
    areaBasis: overlap.length ? 'overlap' : 'original',
  }
}
/** @param {{repo:string, fix:any, link:any, tracedAt:string}} input */
export function escapeRecord({ repo, fix, link, tracedAt }) {
  const original = link.original
  return {
    v: 1,
    repo,
    original: {
      number: original.number,
      title: original.title,
      mergedAt: original.mergedAt,
      mergeHeadSha: original.mergeHeadSha ?? original.headRefOid,
    },
    fix: { number: fix.number, title: fix.title, mergedAt: fix.mergedAt },
    basis: link.basis,
    evidence: safeEvidence(link.evidence),
    ...(link.ticket ? { ticket: link.ticket } : {}),
    ...(link.severity ? { severity: link.severity } : {}),
    ...escapeAreas(original, fix),
    levelAtMerge: null,
    signals: escapeSignals(original),
    tracedAt,
  }
}
/** @param {any} record */
export function renderEscapeComment(record) {
  return `This PR shipped a defect later fixed by #${record.fix.number} (${record.basis}${record.ticket ? ` ${record.ticket}` : ''}). Areas: ${record.areas.map((a) => a.area).join(', ')}.\n\n<!-- boss-escape v1 fix=${record.fix.number} -->\n<!-- boss-escape-record: ${JSON.stringify(record)} -->`
}
/** @param {string} body */
export function parseEscapeComment(body) {
  try {
    const marker = body.match(/<!-- boss-escape v1 fix=(\d+) -->/)
    const payload = body.match(/<!-- boss-escape-record: (\{[^\n]*\}) -->/)
    if (!marker || !payload) return null
    const r = JSON.parse(payload[1])
    if (
      r.v !== 1 ||
      r.fix?.number !== Number(marker[1]) ||
      !Number.isInteger(r.original?.number) ||
      typeof r.repo !== 'string' ||
      !/^[\w.-]+\/[\w.-]+$/.test(r.repo) ||
      !BASES.includes(r.basis) ||
      !Array.isArray(r.areas) ||
      r.areas.some(
        (a) =>
          typeof a.area !== 'string' ||
          !Array.isArray(a.paths) ||
          a.paths.some((p) => typeof p !== 'string'),
      ) ||
      !r.signals ||
      !Number.isFinite(Date.parse(r.tracedAt)) ||
      !Number.isFinite(Date.parse(r.fix.mergedAt)) ||
      !Number.isFinite(Date.parse(r.original.mergedAt)) ||
      typeof r.fix.title !== 'string' ||
      typeof r.original.title !== 'string' ||
      !['overlap', 'original'].includes(r.areaBasis) ||
      !r.signals.findings ||
      !r.signals.size ||
      !['ran', 'skipped', 'unknown'].includes(r.signals.secondModel) ||
      [
        r.signals.findings.blocking,
        r.signals.findings.unapplied,
        r.signals.size.additions,
        r.signals.size.deletions,
        r.signals.size.changedFiles,
      ].some((n) => !Number.isInteger(n) || n < 0) ||
      typeof r.original.mergeHeadSha !== 'string' ||
      r.levelAtMerge !== null ||
      r.signals.proof !== null ||
      typeof r.evidence !== 'string'
    )
      return null
    return r
  } catch {
    return null
  }
}
