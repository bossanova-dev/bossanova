import { createHash } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { isValidOperatorText } from './verify-blockers.mjs'
import { labelName, stateName } from '../skill-config.mjs'

export const acceptMarker = ({ head, blocker, ticket }) =>
  `<!-- boss-accept head=${head} blocker=${blocker} ticket=${ticket} -->`
export const followUpMarker = ({ repo, pr, blocker }) =>
  `<!-- boss-accept-followup repo=${repo} pr=${pr} blocker=${blocker} -->`

/** The caller establishes writer trust; only complete last-line records parse. */
export function parseAcceptLine(last) {
  if (typeof last !== 'string') return null
  const accept = last.match(
    /^<!-- boss-accept head=([a-f0-9]{40}) blocker=(review:[a-f0-9]{12}) ticket=([A-Za-z][A-Za-z0-9]*-[1-9][0-9]*) -->$/i,
  )
  if (accept)
    return { kind: 'accept', head: accept[1].toLowerCase(), blocker: accept[2], ticket: accept[3] }
  const note = last.match(
    /^<!-- boss-accepted-note head=([a-f0-9]{40}) digest=([a-f0-9]{16}) -->$/i,
  )
  return note
    ? { kind: 'accepted-note', head: note[1].toLowerCase(), digest: note[2].toLowerCase() }
    : null
}

/** A map of blocker id to its newest trusted acceptance on this exact head. */
export function acceptedOn(markers, head) {
  const accepted = new Map()
  for (const marker of Array.isArray(markers) ? markers : []) {
    if (marker?.kind !== 'accept' || marker.head !== head) continue
    const previous = accepted.get(marker.blocker)
    if (
      !previous ||
      marker.at > previous.at ||
      (marker.at === previous.at && marker.id > previous.id)
    )
      accepted.set(marker.blocker, marker)
  }
  return accepted
}

export function newestAcceptance(markers, head) {
  const values = [...acceptedOn(markers, head).values()]
    .map((marker) => marker.at)
    .filter(Number.isFinite)
  return values.length ? Math.max(...values) : null
}

export function splitAccepted(blockers, accepted) {
  const remaining = [],
    removed = []
  for (const blocker of blockers) {
    const marker = blocker.kind === 'review-finding' ? accepted?.get(blocker.id) : null
    if (marker) removed.push({ id: blocker.id, ticket: marker.ticket })
    else remaining.push(blocker)
  }
  return { blockers: remaining, accepted: removed }
}

const fenced = (text) => {
  const fence = '`'.repeat(
    Math.max(3, ...Array.from(String(text).matchAll(/`+/g), (m) => m[0].length + 1)),
  )
  return `${fence}\n${text}\n${fence}`
}

export function followUpIssue({ repo, pr, head, blocker, reason, prUrl, ticket }) {
  const title = `Follow-up from PR #${pr}: ${blocker.summary}`
  const marker = followUpMarker({ repo, pr, blocker: blocker.id })
  return {
    title: Array.from(title).length > 256 ? Array.from(title).slice(0, 255).join('') + '…' : title,
    description: [
      `${blocker.severity}${blocker.file ? ` — ${blocker.file}${blocker.line === null ? '' : `:${blocker.line}`}` : ''}`,
      blocker.summary,
      fenced(blocker.text),
      `PR: ${prUrl}\nAccepted head: ${head}`,
      ...(ticket ? [`Tracker issue: ${ticket.url} (${ticket.id})`] : []),
      `Operator reason:\n${fenced(reason)}`,
      marker,
    ].join('\n\n'),
    marker,
  }
}

function acceptanceValues(acceptances) {
  return [...(acceptances instanceof Map ? acceptances.values() : (acceptances ?? []))].sort(
    (a, b) => String(a.blocker).localeCompare(String(b.blocker)),
  )
}

export function acceptedLines(markers, head) {
  const acceptances = markers instanceof Map ? markers : acceptedOn(markers, head)
  return acceptanceValues(acceptances).map(
    (marker) =>
      `- \`${marker.blocker}\` — ${marker.ticket}${marker.url ? ` ([acceptance](${marker.url}))` : ''}`,
  )
}

export function acceptedNote({ headSha, acceptances }) {
  const lines = acceptedLines(
    new Map(acceptanceValues(acceptances).map((marker) => [marker.blocker, marker])),
  )
  const content = [
    `### Accepted by the operator on \`${headSha.slice(0, 12)}\``,
    '',
    ...lines,
  ].join('\n')
  const digest = createHash('sha256').update(content).digest('hex').slice(0, 16)
  return {
    digest,
    body: `${content}\n\n<!-- boss-accepted-note head=${headSha} digest=${digest} -->`,
  }
}

/** Operator-only verb: resolve the follow-up before publishing the head-bound waiver. */
export async function runAccept(options, deps, actions) {
  const {
    UsageError,
    isSha,
    text,
    writeAwareReader,
    commandReader,
    repoFor,
    readPr,
    readOperatorMarkers,
    newestFor,
    resolveAdapter,
    loadConfig,
    postPrComment,
    parseTicketLine,
  } = actions
  options = {
    ...options,
    reasonFile: options.reasonFile ?? options['reason-file'],
    dryRun: options.dryRun ?? options['dry-run'] === true,
  }
  const head = typeof options.head === 'string' ? options.head.toLowerCase() : ''
  if (!isSha(head) || !text(options.blocker) || !text(options.reasonFile))
    throw new UsageError(
      'verify-gate: accept needs --head <40-hex sha>, --blocker <id> and --reason-file <file>',
    )
  let reason
  try {
    reason = readFileSync(options.reasonFile, 'utf8')
    if (!isValidOperatorText(reason)) throw new Error('invalid operator reason')
  } catch (error) {
    throw new UsageError(`verify-gate: accept: ${error.message}`)
  }
  const refuse = (reason, detail = undefined) => ({
    accepted: false,
    reason,
    head,
    ...(detail ? { detail } : {}),
  })
  const read = writeAwareReader(deps.read ?? commandReader(deps)),
    repo = repoFor(read, options.repo)
  const view = readPr(read, options.pr, repo)
  if (!view.ok) return refuse('pr-unreadable')
  if (view.payload?.state !== 'OPEN') return refuse('not-open')
  if (String(view.payload.headRefOid ?? '').toLowerCase() !== head) return refuse('head-moved')
  const markers = readOperatorMarkers(read, repo, options.pr)
  if (!markers.readable) return refuse('comments-unreadable')
  const standing = newestFor(markers.markers, head, (kind) => kind === 'blockers')?.blockerFile
  if (!standing || standing.pr !== options.pr) return refuse('no-blockers')
  const blocker = standing.blockers.find((b) => b.id === options.blocker)
  if (!blocker) return refuse('blocker-not-listed')
  if (blocker.kind !== 'review-finding') return refuse('blocker-not-acceptable', blocker.kind)
  if (standing.blockers.some((b) => b.kind === 'operator-note')) return refuse('sent-back')
  const previous = acceptedOn(markers.markers, head).get(blocker.id)
  if (previous)
    return {
      accepted: true,
      already: true,
      head,
      blocker: blocker.id,
      ticket: { identifier: previous.ticket },
      commentUrl: previous.url,
    }
  const adapter = resolveAdapter(deps)
  if (typeof adapter?.applyIssueCreate !== 'function') return refuse('tracker-create-unavailable')
  let issue
  try {
    const config = loadConfig(deps)
    issue = {
      ...followUpIssue({
        repo,
        pr: options.pr,
        head,
        blocker,
        reason,
        prUrl: view.payload.url ?? `https://github.com/${repo}/pull/${options.pr}`,
        ticket: parseTicketLine(view.payload.body),
      }),
      stateName: stateName(config, 'unplanned'),
      labels: [labelName(config, 'agentPlan')],
    }
  } catch (error) {
    return refuse('tracker-create-unavailable', error.code ?? error.message)
  }
  const comment = (identifier, url) =>
    [
      '**Accepted by the operator**',
      `**${blocker.severity}** ${blocker.summary} (\`${blocker.id}\`)`,
      `Follow-up: ${url ? `[${identifier}](${url})` : identifier}`,
      fenced(reason),
      acceptMarker({ head, blocker: blocker.id, ticket: identifier }),
    ].join('\n\n')
  if (options.dryRun)
    return {
      accepted: false,
      dryRun: true,
      writes: [
        { kind: 'tracker-issue', ...issue },
        { kind: 'pr-comment', body: comment('<follow-up identifier>', null) },
      ],
    }
  let created
  try {
    created = await adapter.applyIssueCreate(issue)
  } catch (error) {
    return refuse('tracker-create-unavailable', error.code ?? error.message)
  }
  if (created?.outcome === 'indeterminate')
    return refuse('tracker-create-indeterminate', created.reason)
  if (!created?.ok || created.outcome !== 'ok' || !created.issue?.identifier || !created.issue?.url)
    return refuse('tracker-create-failed', created?.reason)
  let commentUrl
  try {
    commentUrl = await postPrComment(
      read,
      repo,
      options.pr,
      comment(created.issue.identifier, created.issue.url),
    )
  } catch {
    return refuse('comment-failed')
  }
  if (!commentUrl) return refuse('comment-failed')
  return {
    accepted: true,
    head,
    blocker: blocker.id,
    ticket: { identifier: created.issue.identifier, url: created.issue.url },
    commentUrl,
    ...(created.existing === true ? { existingTicket: true } : {}),
  }
}
