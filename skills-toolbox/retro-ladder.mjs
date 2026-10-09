// Pure remedy classification and closed-ticket follow-through contracts.
export const RUNGS = Object.freeze(
  [
    {
      id: 'prevent',
      meaning:
        'change code, types or structure so the mistake cannot be written (one blessed way to do it, enforced boundaries, remove the variant)',
      requiredEvidence: 'the variant or boundary to remove',
    },
    {
      id: 'check',
      meaning:
        'a fast deterministic check: lint rule, type rule, test, pre-commit or CI gate; with a baseline that blocks new instances when existing ones remain',
      requiredEvidence: 'a sketch of the grep or AST pattern, or the assertion',
    },
    {
      id: 'helper',
      meaning: 'turn a procedure agents keep improvising into a script or CLI the skill calls',
      requiredEvidence: 'the repeated steps',
    },
    {
      id: 'lens',
      meaning:
        'a narrow model-based check (review lens or verify extension) when no mechanical check can express it',
      requiredEvidence: 'why check cannot express it',
    },
    {
      id: 'rule',
      meaning: 'a line in an agent rules file (AGENTS.md / CLAUDE.md or equivalent) or skill prose',
      requiredEvidence: 'why every stronger rung is impossible',
    },
    {
      id: 'context',
      meaning: 'the agent lacked information: a connector, a doc or a feature map',
      requiredEvidence: 'what the agent had to ask for, or guessed',
    },
  ].map(Object.freeze),
)
export const RUNG_IDS = Object.freeze(RUNGS.map(({ id }) => id))
export const MAX_TARGET_LENGTH = 200
export const MAX_SUGGESTED_FIXES = 3
export const DEFAULT_MIN_RUNS = 2
export const DEFAULT_FOLLOW_THROUGH_LIMIT = 5
const text = (value) => (typeof value === 'string' ? value.trim() : '')
export function rungStrength(id) {
  const index = RUNG_IDS.indexOf(id)
  return index < 0 ? RUNGS.length : index
}

/** Evidence describes the rung's required justification; uncited choices abstain. */
export function normalizeLadder(entry = {}) {
  entry = entry && typeof entry === 'object' ? entry : {}
  const problems = []
  let rung = entry.rung
  const rungEvidence = text(entry.rungEvidence)
  if (!rung) problems.push('missing-rung')
  else if (!RUNG_IDS.includes(rung)) problems.push('unknown-rung')
  if (!rungEvidence) {
    problems.push('missing-evidence')
    if (rung === 'rule') problems.push('rule-unjustified')
    if (rung === 'lens') problems.push('lens-unjustified')
  }
  const flagged = problems.length > 0
  if (flagged) rung = 'rule'
  const existingCheck = text(entry.existingCheck) || null
  if (entry.existingCheck != null && typeof entry.existingCheck !== 'string')
    problems.push('invalid-existing-check')
  let instances = 0
  let instanceExamples = []
  if (Number.isInteger(entry.instances) && entry.instances >= 0) instances = entry.instances
  else if (Array.isArray(entry.instances) && entry.instances.every((value) => text(value))) {
    instanceExamples = entry.instances.map(text)
    instances = instanceExamples.length
  } else problems.push('invalid-instances')
  let supersedes = text(entry.supersedes) || null
  if (supersedes && rungStrength(rung) >= rungStrength('rule')) {
    supersedes = null
    problems.push('supersedes-ignored')
  }
  const target = (text(entry.target) || rungEvidence)
    .replace(/[\r\n]+/g, ' ')
    .slice(0, MAX_TARGET_LENGTH)
  return {
    rung,
    rungEvidence,
    existingCheck,
    instances,
    instanceExamples,
    supersedes,
    target,
    flagged: flagged || problems.length > 0,
    problems,
  }
}

export function distinctRunCount(notes = []) {
  const runs = new Set()
  notes.forEach((note, index) =>
    runs.add(note.run_id != null ? `run:${note.run_id}` : `note:${note.id ?? index}`),
  )
  return runs.size
}
export function strongestSuggestedFixes(notes = [], limit = MAX_SUGGESTED_FIXES) {
  const groups = new Map()
  for (const note of notes) {
    const fix = text(note.suggestedFix).replace(/\s+/g, ' ')
    if (!fix || /^(unknown|none|n\/a)$/i.test(fix)) continue
    const key = fix.toLowerCase()
    if (!groups.has(key)) groups.set(key, { fix, notes: [] })
    groups.get(key).notes.push(note)
  }
  const newest = (group) => Math.max(...group.notes.map((note) => Date.parse(note.created_at) || 0))
  return [...groups.values()]
    .sort(
      (a, b) =>
        distinctRunCount(b.notes) - distinctRunCount(a.notes) ||
        newest(b) - newest(a) ||
        a.fix.localeCompare(b.fix),
    )
    .slice(0, Math.max(0, Math.floor(limit)))
    .map(({ fix }) => fix)
}
function resolveNumber(argument, environmentValue, fallback, minimum) {
  for (const candidate of [argument, environmentValue]) {
    if (candidate == null || String(candidate).trim() === '') continue
    const parsed = Number(candidate)
    if (Number.isFinite(parsed)) return Math.max(minimum, Math.floor(parsed))
  }
  return fallback
}
export function resolveMinRuns(argument, env = {}) {
  return resolveNumber(argument, env.BOSS_RETRO_MIN_RUNS, DEFAULT_MIN_RUNS, 1)
}
export function resolveFollowThroughLimit(argument, env = {}) {
  return resolveNumber(
    argument,
    env.BOSS_RETRO_FOLLOW_THROUGH_LIMIT,
    DEFAULT_FOLLOW_THROUGH_LIMIT,
    0,
  )
}

// Markdown escapes and list prefixes are presentation, not marker identity.
function markerLine(line) {
  return line
    .replace(/\\([!"#$%&'()*+,\-./:;<=>?@[\]\^_`{|}~])/g, '$1')
    .trim()
    .replace(/^[*-]\s+/, '')
}
export function parsePromiseRecords(description = '') {
  const records = [],
    enforced = [],
    problems = []
  for (const raw of String(description).split(/\r?\n/)) {
    const line = markerLine(raw)
    const enforcement = /^Notes-enforced:\s+(\S+)\s+(\S+)\s*$/.exec(line)
    if (enforcement) {
      if (Number.isNaN(Date.parse(enforcement[2])))
        problems.push({ key: enforcement[1], problem: 'invalid-enforced-date' })
      else enforced.push({ key: enforcement[1], at: enforcement[2] })
      continue
    }
    const match = /^Notes-promise:\s+(\S+)\s+(.*)$/.exec(line)
    if (!match) continue
    const [, key, rest] = match
    const targetMatch = /(?:^|\s)target=(.*)$/.exec(rest)
    const fields = Object.fromEntries(
      [
        ...rest.slice(0, targetMatch?.index ?? rest.length).matchAll(/(?:^|\s)([\w-]+)=([^\s]+)/g),
      ].map((m) => [m[1], m[2]]),
    )
    const errors = []
    if (!RUNG_IDS.includes(fields.rung)) errors.push('unknown-rung')
    for (const name of ['round', 'instances']) {
      if (
        !/^\d+$/.test(fields[name] ?? '') ||
        !Number.isSafeInteger(Number(fields[name])) ||
        (name === 'round' && Number(fields[name]) < 1)
      )
        errors.push(`invalid-${name}`)
    }
    if (errors.length) problems.push(...errors.map((problem) => ({ key, problem })))
    else
      records.push({
        key,
        rung: fields.rung,
        round: Number(fields.round),
        instances: Number(fields.instances),
        refiles: fields.refiles && fields.refiles !== 'none' ? fields.refiles : null,
        target: text(targetMatch?.[1]).slice(0, MAX_TARGET_LENGTH),
      })
  }
  return { records, enforced, problems }
}
export function markerKeysIn(description = '') {
  return String(description)
    .split(/\r?\n/)
    .map(markerLine)
    .flatMap((line) => {
      const match = /^Notes:\s+(\S+)\s*$/.exec(line)
      return match ? [match[1]] : []
    })
}

export function followThroughDue(markedIssues, { limit = DEFAULT_FOLLOW_THROUGH_LIMIT } = {}) {
  const result = {
    due: [],
    overLimit: [],
    waiting: [],
    canceled: [],
    enforced: [],
    superseded: [],
    problems: [],
  }
  const themes = new Map()
  for (const issue of markedIssues) {
    const parsed = parsePromiseRecords(issue.description)
    result.problems.push(
      ...parsed.problems.map((problem) => ({ ...problem, issueId: issue.identifier })),
    )
    for (const record of parsed.records) {
      const entry = {
        key: record.key,
        issueId: issue.identifier,
        title: issue.title ?? '',
        rung: record.rung,
        target: record.target,
        instances: record.instances,
        round: record.round,
        resolvedAt: issue.resolvedAt ?? null,
        markerKeys: markerKeysIn(issue.description),
      }
      const candidate = {
        entry,
        issue,
        enforced: parsed.enforced.some(({ key }) => key === record.key),
      }
      if (!themes.has(record.key)) themes.set(record.key, [])
      themes.get(record.key).push(candidate)
    }
  }
  const dates = (value) => Date.parse(value) || 0
  const due = []
  for (const candidates of themes.values()) {
    candidates.sort(
      (a, b) =>
        b.entry.round - a.entry.round ||
        dates(b.issue.createdAt) - dates(a.issue.createdAt) ||
        String(b.entry.issueId).localeCompare(String(a.entry.issueId)),
    )
    const [current, ...older] = candidates
    result.superseded.push(...older.map(({ entry }) => entry))
    if (current.enforced) result.enforced.push(current.entry)
    else if (current.issue.resolution === 'canceled') result.canceled.push(current.entry)
    else if (current.issue.resolution !== 'done') result.waiting.push(current.entry)
    else due.push(current)
  }
  due.sort(
    (a, b) =>
      dates(a.entry.resolvedAt) - dates(b.entry.resolvedAt) ||
      dates(a.issue.createdAt) - dates(b.issue.createdAt) ||
      a.entry.key.localeCompare(b.entry.key),
  )
  const bound = resolveFollowThroughLimit(limit)
  result.due = due.slice(0, bound).map(({ entry }) => entry)
  result.overLimit = due.slice(bound).map(({ entry }) => entry)
  return result
}

export function applyFollowThrough(due, verdicts) {
  const entries = new Map(due.map((entry) => [entry.key, entry]))
  const seen = new Set()
  const result = { retire: [], refile: [], unconfirmed: [] }
  for (const verdict of verdicts) {
    if (!entries.has(verdict.key)) throw new Error(`Unknown follow-through key: ${verdict.key}`)
    if (seen.has(verdict.key)) throw new Error(`Duplicate follow-through key: ${verdict.key}`)
    if (!['enforced', 'prose-only', 'regressed'].includes(verdict.outcome))
      throw new Error(`Unknown follow-through outcome: ${verdict.outcome}`)
    seen.add(verdict.key)
    const promise = entries.get(verdict.key)
    const evidence = text(verdict.evidence),
      cleanup = text(verdict.cleanup)
    const landedRung = verdict.landedRung ?? promise.rung
    const problems = []
    if (!evidence) problems.push('missing-evidence')
    if (verdict.outcome === 'enforced' && !RUNG_IDS.includes(landedRung))
      problems.push('unknown-landed-rung')
    if (verdict.outcome === 'enforced' && promise.instances > 0 && !cleanup)
      problems.push('missing-cleanup')
    const entry = { ...promise, outcome: verdict.outcome, evidence, cleanup, landedRung, problems }
    if (problems.length) {
      result.unconfirmed.push(entry)
      continue
    }
    const weaker = rungStrength(landedRung) > rungStrength(promise.rung)
    const downgrade = {
      justification: text(verdict.downgrade?.justification),
      source: text(verdict.downgrade?.source),
    }
    const acceptedDowngrade = weaker && !!downgrade.justification && !!downgrade.source
    if (verdict.outcome === 'enforced' && (!weaker || acceptedDowngrade)) {
      result.retire.push({
        ...entry,
        acceptedDowngrade,
        ...(acceptedDowngrade ? { downgrade } : {}),
      })
    } else {
      const rung =
        rungStrength(verdict.rung) < rungStrength(promise.rung) ? verdict.rung : promise.rung
      result.refile.push({
        ...entry,
        rung,
        promisedRung: promise.rung,
        reason: verdict.outcome === 'enforced' ? 'weaker-rung-unjustified' : verdict.outcome,
      })
    }
  }
  for (const key of entries.keys())
    if (!seen.has(key)) throw new Error(`Missing follow-through verdict: ${key}`)
  return result
}

export function markEnforced(description, key, { at } = {}) {
  if (!/^\S+$/.test(key) || !text(at) || /\s/.test(at) || Number.isNaN(Date.parse(at)))
    throw new Error('Enforcement marker requires a key and ISO date')
  if (parsePromiseRecords(description).enforced.some((record) => record.key === key))
    return description
  const lines = description.split(/\r?\n/)
  const index = lines.findIndex((line) => markerLine(line).startsWith(`Notes-promise: ${key} `))
  if (index < 0) throw new Error(`Missing promise record: ${key}`)
  lines.splice(index, 0, `Notes-enforced: ${key} ${at}`)
  return lines.join(description.includes('\r\n') ? '\r\n' : '\n')
}
