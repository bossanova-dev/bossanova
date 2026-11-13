// The approver of a verify approval record: the principal bossd's socket caller gate attributed
// the approval to. bossd passes it to `verify-gate.mjs approve` as BOSS_APPROVER_* environment, and
// the record carries it on a hidden approver line above the approve marker. The approve marker
// stays the record's last line, so a toolbox that predates the approver line reads the record
// unchanged. The line grants nothing on its own: verify still counts only trusted writers' records.
// It decides one thing, in operatorDecision: an older approval of the same head adds its codes to
// the newest one's only when its line names the operator.

const FIELD = /^[A-Za-z0-9._:-]{1,128}$/

// [record key, marker name, environment suffix]
const KEYS = [
  ['kind', 'kind', 'KIND'],
  ['role', 'role', 'ROLE'],
  ['chat', 'chat', 'CHAT'],
  ['session', 'session', 'SESSION'],
  ['forwardedFrom', 'forwarded-from', 'FORWARDED_FROM'],
]

const MARKER = /^<!-- boss-approver ((?:[a-z-]+=\S+ ?)+) -->$/

/**
 * The approver bossd named in `env`: `{kind: 'operator'|'agent-chat', role?, chat?, session?,
 * forwardedFrom?}`, or null when it named none (a direct toolbox run) or named one malformed. An
 * agent-chat approver always names its chat.
 */
export function approverFromEnv(env = {}) {
  const approver = {}
  for (const [key, , suffix] of KEYS) {
    const value = env[`BOSS_APPROVER_${suffix}`]
    if (typeof value === 'string' && FIELD.test(value)) approver[key] = value
  }
  if (approver.kind === 'operator') {
    delete approver.role
    delete approver.chat
    delete approver.session
    return approver
  }
  return approver.kind === 'agent-chat' && approver.chat ? approver : null
}

/** The hidden approver line naming `approver`. */
export function approverMarker(approver) {
  const fields = KEYS.filter(([key]) => approver[key]).map(
    ([key, name]) => `${name}=${approver[key]}`,
  )
  return `<!-- boss-approver ${fields.join(' ')} -->`
}

/** The approver an approval record's body names, or null for a record without a valid one. */
export function parseApprover(body) {
  const lines = typeof body === 'string' ? body.split('\n').map((line) => line.trim()) : []
  const match = lines.map((line) => line.match(MARKER)).find(Boolean)
  if (!match) return null
  /** @type {Map<string, string>} */
  const named = new Map()
  for (const pair of match[1].trim().split(' ')) {
    const [name, value = ''] = pair.split('=')
    named.set(name, value)
  }
  return approverFromEnv(
    Object.fromEntries(
      KEYS.map(([, name, suffix]) => [`BOSS_APPROVER_${suffix}`, named.get(name)]),
    ),
  )
}

/** `{approver}` for a marker entry whose record names one, else `{}`. */
export function approverEntry(body) {
  const approver = parseApprover(body)
  return approver ? { approver } : {}
}

function heading(approver) {
  const via = approver?.forwardedFrom
    ? `, through \`boss --host\` from \`${approver.forwardedFrom}\``
    : ''
  if (approver?.kind !== 'agent-chat') return `**Approved by the operator${via}**`
  const grant =
    approver.role === 'coordinator' ? 'the coordinator approval grant' : 'a chat role grant'
  const session = approver.session ? ` (session \`${approver.session}\`)` : ''
  return `**Approved by ${approver.role || 'agent'} chat \`${approver.chat}\`${session} under ${grant}${via}**`
}

/**
 * The approval record's body: a heading naming who approved, the scope sentence, the approver line
 * when bossd named an approver, and the approve marker last.
 */
export function approvalRecord(approver, scope, marker) {
  return [heading(approver), scope, ...(approver ? [approverMarker(approver)] : []), marker].join(
    '\n\n',
  )
}

/**
 * The approval markers that stand together on `newest.head`, whose newest decision is the approval
 * `newest`: `newest` itself, plus every operator-attributed approval of that head posted after the
 * head's newest send-back. The judge's code set can differ between runs of one unchanged head, so
 * each operator approval of it adds to what stands rather than replacing it. `markers` are the
 * trusted operator markers; another head's approval never stands.
 */
function standingApprovals(markers, newest) {
  const later = (a, b) => a.at > b.at || (a.at === b.at && a.id > b.id)
  const onHead = (Array.isArray(markers) ? markers : []).filter((m) => m?.head === newest.head)
  const sentBack = onHead
    .filter((m) => m.kind === 'send-back')
    .reduce((last, m) => (!last || later(m, last) ? m : last), null)
  return onHead.filter(
    (m) =>
      m === newest ||
      (m.kind === 'approve' &&
        m.approver?.kind === 'operator' &&
        (!sentBack || later(m, sentBack))),
  )
}

/**
 * The operator's decision when `newest` is an approval: `{decision: 'approved', waives, covers,
 * url}`. `waives` is `newest`'s own codes; `covers` is every code standingApprovals waive, as the
 * comma list `parseCodes` (verify's park-code parser) orders, or '' when they waive nothing.
 */
export function approvalDecision(markers, newest, parseCodes) {
  const codes = standingApprovals(markers, newest)
    .map((m) => m.waives)
    .filter(Boolean)
  const covers = codes.length > 0 ? (parseCodes(codes.join(',')) ?? []).join(',') : ''
  return { decision: 'approved', waives: newest.waives, covers, url: newest.url ?? '' }
}

/** The approver bossd named for this run, read from `deps.env` or the process environment. */
export function approverIn(deps = {}) {
  return approverFromEnv(deps.env ?? process.env)
}
