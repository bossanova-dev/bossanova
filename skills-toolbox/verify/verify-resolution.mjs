import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { buildBlockers } from './verify-blockers.mjs'

const sha = (s) => typeof s === 'string' && /^[a-f0-9]{40}$/.test(s)
const single = (s) => typeof s === 'string' && s.trim() !== '' && !/[\r\n]/.test(s)
const reviewId = (s) => typeof s === 'string' && /^review:[a-f0-9]{12}$/.test(s)
const statuses = new Set(['resolved', 'present', 'unsure'])
const forkHead = (facts) => facts.prClass === 'fork' || facts.tier?.class === 'fork'
const digestOf = (s) => createHash('sha256').update(s).digest('hex').slice(0, 16)

/** Missing assertion provenance cannot establish that a finding is stale. */
export function staleFindingBlockers(blockers, { reportHead, headSha, resolved, assertions }) {
  return blockers.filter((b) => {
    const asserted = assertions?.get(b.id) ?? b.assertionHead ?? reportHead
    return (
      b.kind === 'review-finding' && sha(asserted) && asserted !== headSha && !resolved?.has(b.id)
    )
  })
}

/** Invalid evidence records uncertainty, never a passing test or a discharge. */
export function validateResolution(file, { headSha, ids, treeHas, lineCount }) {
  try {
    const input = typeof file === 'string' ? JSON.parse(readFileSync(file, 'utf8')) : file
    if (
      input?.version !== 1 ||
      input.head !== headSha ||
      !sha(headSha) ||
      !Array.isArray(input.entries)
    )
      return new Map()
    const known = new Set(ids),
      result = new Map()
    for (const e of input.entries) {
      if (!e || !reviewId(e.id) || !known.has(e.id) || result.has(e.id)) return new Map()
      let valid = statuses.has(e.status)
      if (e.status === 'resolved')
        valid &&=
          single(e.file) &&
          !e.file.startsWith('/') &&
          !e.file.split('/').includes('..') &&
          Number.isSafeInteger(e.line) &&
          e.line > 0 &&
          single(e.test) &&
          single(e.command) &&
          e.exitCode === 0 &&
          treeHas(e.file) &&
          e.line <= lineCount(e.file)
      result.set(e.id, valid ? { ...e } : { id: e.id, status: 'unsure' })
    }
    return result
  } catch {
    return new Map()
  }
}

export function splitResolved(blockers, resolved) {
  const remaining = [],
    removed = []
  for (const b of blockers) {
    const entry = b.kind === 'review-finding' ? resolved?.get(b.id) : null
    if (entry?.status === 'resolved') removed.push(entry)
    else remaining.push(b)
  }
  return { blockers: remaining, resolved: removed }
}

export function resolutionComment(head, entries) {
  const sorted = [...entries].sort((a, b) => a.id.localeCompare(b.id))
  const payload = JSON.stringify({ version: 1, head, entries: sorted })
  const digest = digestOf(payload)
  return {
    digest,
    body: [
      '### Review findings checked on this head',
      '',
      ...sorted.map(
        (e) =>
          `- ${e.id}: ${e.status}${e.status === 'resolved' ? ` — ${e.file}:${e.line}; ${e.test}` : ''}`,
      ),
      '',
      `<!-- boss-verify-resolution head=${head} digest=${digest} v1 ${Buffer.from(payload).toString('base64url')} -->`,
    ].join('\n'),
  }
}

/** The caller establishes author trust; digest and payload must agree with the marker. */
export function parseResolutionLine(last) {
  const m =
    typeof last === 'string' &&
    last.match(
      /^<!-- boss-verify-resolution head=([a-f0-9]{40}) digest=([a-f0-9]{16}) v1 ([A-Za-z0-9_-]+) -->$/,
    )
  if (!m) return null
  try {
    const raw = Buffer.from(m[3], 'base64url').toString('utf8'),
      p = JSON.parse(raw)
    if (digestOf(raw) !== m[2] || p.version !== 1 || p.head !== m[1] || !Array.isArray(p.entries))
      return null
    const ids = p.entries.map((e) => e?.id)
    if (
      new Set(ids).size !== ids.length ||
      p.entries.some(
        (e) =>
          !reviewId(e?.id) ||
          !statuses.has(e.status) ||
          (e.status === 'resolved' &&
            (!single(e.file) ||
              !Number.isSafeInteger(e.line) ||
              e.line < 1 ||
              !single(e.test) ||
              !single(e.command) ||
              e.exitCode !== 0)),
      )
    )
      return null
    return { kind: 'resolution', head: p.head, digest: m[2], entries: p.entries }
  } catch {
    return null
  }
}

/** A new report supersedes proof published before it, including proof on the same head. */
export function resolutionMarkersAfter(markers, report = null) {
  return (markers ?? []).filter(
    (m) =>
      m.kind === 'resolution' &&
      (!report || m.at > report.at || (m.at === report.at && m.id > report.id)),
  )
}

export function resolvedOn(markers, headSha, report = null) {
  const result = new Map()
  const sorted = resolutionMarkersAfter(markers, report)
    .filter((m) => m.head === headSha)
    .sort((a, b) => (a.at ?? 0) - (b.at ?? 0) || (a.id ?? 0) - (b.id ?? 0))
  for (const m of sorted) for (const e of m.entries) result.set(e.id, e)
  return result
}

const standingReport = (markers) =>
  (markers ?? [])
    .filter((m) => m.kind === 'review-findings')
    .sort((a, b) => b.at - a.at || b.id - a.id)[0]

function treeEvidence(read, headSha) {
  const files = new Map()
  const content = (file) => {
    if (!files.has(file)) {
      const spec = `${headSha}:${file}`
      const kind = read('git', ['cat-file', '-t', spec])
      files.set(
        file,
        kind.ok && String(kind.output).trim() === 'blob'
          ? read('git', ['show', spec], { raw: true })
          : { ok: false },
      )
    }
    return files.get(file)
  }
  return {
    treeHas: (file) => content(file).ok,
    lineCount: (file) =>
      content(file).ok && String(content(file).output).length > 0
        ? String(content(file).output).replace(/\n$/, '').split('\n').length
        : 0,
  }
}

export function resolutionFacts(facts, report, markers, file, read) {
  const assertions = new Map()
  for (const f of report?.findings ?? [])
    for (const b of buildBlockers({ reviewFindings: [{ ...f, verified: true }] }))
      assertions.set(b.id, f.assertionHead ?? report.head)
  const resolved = resolvedOn(markers, facts.headSha, report)
  if (file !== undefined && !forkHead(facts)) {
    const entries = validateResolution(file, {
      headSha: facts.headSha,
      ids: [...assertions.keys()],
      ...treeEvidence(read, facts.headSha),
    })
    for (const [id, entry] of entries) resolved.set(id, entry)
  }
  return {
    resolution: resolved,
    resolutionAttempted: file !== undefined,
    assertionHeads: assertions,
    reportHead: report?.head,
  }
}

/** Return only after accepted findings have been removed by the operator-acceptance path. */
export function decideResolution(blockers, facts) {
  if (forkHead(facts)) return { blockers }
  const split = splitResolved(blockers, facts.resolution)
  const stale = facts.resolutionAttempted
    ? []
    : staleFindingBlockers(split.blockers, {
        reportHead: facts.reportHead,
        headSha: facts.headSha,
        resolved: facts.resolution,
        assertions: facts.assertionHeads,
      })
  return {
    blockers: split.blockers,
    ...(split.resolved.length ? { resolved: split.resolved } : {}),
    ...(stale.length
      ? {
          verdict: 'resolution-required',
          reason: 'findings-unconfirmed',
          resolutionEnvelope: {
            version: 1,
            pr: facts.pr,
            headSha: facts.headSha,
            findings: stale.map((b) => ({
              id: b.id,
              severity: b.severity,
              file: b.file,
              line: b.line,
              title: b.summary,
              evidence: b.text,
              assertionHead: facts.assertionHeads?.get(b.id) ?? facts.reportHead,
            })),
          },
        }
      : {}),
  }
}

export function checkResolutionOption(options, UsageError) {
  if (options.resolution !== undefined && ['claim', 'wait'].includes(options.verdict))
    throw new UsageError('verify-gate: --resolution is valid only on pass|blocked|defect|human')
}

/** A resolution publication is cosmetic; unreadable history or write failure never changes verdict. */
export function recordResolution(ctx, file, { readOperatorMarkers, postPrComment }) {
  if (file === undefined) return {}
  try {
    const history = readOperatorMarkers(ctx.read, ctx.repo, ctx.pr)
    if (!history.readable) return { resolutionRecord: 'unknown' }
    const report = standingReport(history.markers)
    const f = resolutionFacts({ headSha: ctx.headSha }, report, [], file, ctx.read)
    if (!f.resolution.size) return { resolutionRecord: 'invalid' }
    const note = resolutionComment(ctx.headSha, [...f.resolution.values()])
    if (
      resolutionMarkersAfter(history.markers, report).some(
        (m) => m.head === ctx.headSha && m.digest === note.digest,
      )
    )
      return { resolutionRecord: 'current' }
    ctx.writes.push({ kind: 'pr-comment', body: note.body })
    return {
      resolutionRecord: ctx.dryRun
        ? 'planned'
        : postPrComment(ctx.read, ctx.repo, ctx.pr, note.body)
          ? 'posted'
          : 'failed',
    }
  } catch {
    return { resolutionRecord: 'failed' }
  }
}

export function resolvedSection(body, file, ctx, history) {
  if (file === undefined || !history?.readable) return body
  const f = resolutionFacts(
    { headSha: ctx.headSha },
    standingReport(history.markers),
    [],
    file,
    ctx.read,
  )
  const entries = [...f.resolution.values()].filter((e) => e.status === 'resolved')
  return entries.length
    ? [
        '### Resolved on this head',
        '',
        ...entries.map((e) => `- ${e.id}: ${e.file}:${e.line}; ${e.test}`),
        '',
        body,
      ].join('\n')
    : body
}
