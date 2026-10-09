// Bounded, offline-testable human-correction signals collected when a retro runs.
import { spawnSync } from 'node:child_process'
import { createHash, randomUUID } from 'node:crypto'
import fs from 'node:fs'
import path from 'node:path'
import { RECEIPT_CONTEXT } from './commit-status.mjs'
import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS } from './verify-gate.mjs'
import { formatRunLine, isSecretBearing } from './bs-record-notes.mjs'
import { resolveBossBinary } from './boss-binary.mjs'
import { isMainModule } from './main-module.mjs'

export const PAGE = 20
export const MAX_PRS = 100
export const MAX_REVERTS = 20
export const MAX_NOTES_PER_COLLECTOR = 10
export const MAX_WINDOW_DAYS = 30
export const REPEATED_CI_MIN_PRS = 3
export const SKIP_REASONS = Object.freeze([
  'gh-unavailable',
  'gh-unauthenticated',
  'graphql-error',
  'rate-limited',
  'timeout',
  'git-unavailable',
  'no-base-ref',
  'no-repo',
  'boss-unavailable',
])
const names = ['reverts', 'reviews', 'parks', 'ci']
const day = 86400000
const vocabulary = {
  reviews: [
    'retro-signals: a human left review feedback on an agent PR',
    'human corrections identify gaps in agent guidance',
    'turn the correction into a gate, helper or guidance so the agent gets it right first',
  ],
  parks: [
    'retro-signals: verify parked an agent PR for a human',
    'human intervention delays otherwise automated work',
    'remove the cause of the park, or make the parked path automatable',
  ],
  approval: [
    'retro-signals: a human approved an agent PR that verify had parked',
    'human approvals identify paths that still need intervention',
    'remove the cause of the park, or make the parked path automatable',
  ],
  ci: [
    'retro-signals: a CI check failed on several agent PRs',
    'repeated remote failures waste review and repair cycles',
    'make this check fast enough to run locally before pushing, or fix its flake',
  ],
  reverts: [
    'retro-signals: a merged agent PR was reverted',
    'a revert exposes a defect missed before merge',
    'find what the pre-merge gates missed and add the check that would have caught it',
  ],
}
const clean = (value) =>
  String(value ?? '')
    .replace(/\s+/g, ' ')
    .trim()
const nodes = (connection) => connection?.nodes ?? []
const commits = (pr) => nodes(pr.commits).map((entry) => entry.commit)
const contexts = (commit) => nodes(commit?.statusCheckRollup?.contexts)
const allContexts = (pr) => commits(pr).flatMap(contexts)
const agentPr = (pr) =>
  allContexts(pr).some((c) => c.context === RECEIPT_CONTEXT && c.state === 'SUCCESS')
const inWindow = (at, { start, end } = {}) =>
  (!start || Date.parse(at) >= Date.parse(start)) && (!end || Date.parse(at) <= Date.parse(end))

function occurrence(collector, where, pr, at, excerpt = '', kind = collector) {
  const runId = `pr:${pr.number}`
  const [statement, why, fix] = vocabulary[kind]
  const body = [
    statement,
    `Where: ${clean(where)}`,
    `Why it matters: ${why}. Detail: PR #${pr.number} ${clean(pr.title)}${excerpt ? ` — ${clean(excerpt).slice(0, 200)}` : ''}`,
    `Suggested fix: ${fix}`,
    formatRunLine(['retro-signals', collector, 'headless'], runId),
  ].join('\n')
  const idempotencyKey = `retro-signals:${createHash('sha256')
    .update([collector, clean(where), runId].join('|'))
    .digest('hex')
    .slice(0, 32)}`
  return {
    collector,
    where: clean(where),
    runId,
    idempotencyKey,
    body,
    at,
    secret: isSecretBearing(excerpt),
  }
}
function bounded(candidates) {
  const dropped = { secretShape: 0, overCap: 0 }
  const unique = new Map()
  for (const note of candidates.sort((a, b) => Date.parse(b.at) - Date.parse(a.at))) {
    if (note.secret || isSecretBearing(note.body)) {
      dropped.secretShape++
      continue
    }
    if (!unique.has(note.idempotencyKey)) unique.set(note.idempotencyKey, note)
  }
  dropped.overCap = Math.max(0, unique.size - MAX_NOTES_PER_COLLECTOR)
  return {
    notes: [...unique.values()].slice(0, MAX_NOTES_PER_COLLECTOR),
    found: unique.size,
    dropped,
  }
}
export function collectReviews(prs, options = {}) {
  const candidates = []
  const human = (author) =>
    author &&
    author.__typename !== 'Bot' &&
    !String(author.login).endsWith('[bot]') &&
    !(options.agentAuthors ?? []).some(
      (login) => login.toLowerCase() === String(author.login).toLowerCase(),
    )
  for (const pr of prs) {
    for (const thread of nodes(pr.reviewThreads)) {
      const c = nodes(thread.comments)[0]
      if (c && human(c.author) && inWindow(c.createdAt, options))
        candidates.push(
          occurrence('reviews', path.posix.dirname(c.path || '.'), pr, c.createdAt, c.body),
        )
    }
    for (const review of nodes(pr.reviews)) {
      if (
        human(review.author) &&
        review.state !== 'PENDING' &&
        (clean(review.body) || review.state === 'CHANGES_REQUESTED') &&
        inWindow(review.submittedAt ?? pr.updatedAt, options)
      )
        candidates.push(
          occurrence(
            'reviews',
            'pull request (review body)',
            pr,
            review.submittedAt ?? pr.updatedAt,
            review.body,
          ),
        )
    }
  }
  return bounded(candidates)
}
function parkCode(c) {
  if (c.context !== VERIFY_CONTEXT) return null
  const desc = c.description ?? ''
  if (c.state === 'PENDING' && desc.startsWith(VERIFY_DESCRIPTIONS.needsHuman))
    return desc.slice(VERIFY_DESCRIPTIONS.needsHuman.length).trim()
  if (
    c.state === 'FAILURE' &&
    desc.startsWith(VERIFY_DESCRIPTIONS.defect) &&
    desc.endsWith(VERIFY_DESCRIPTIONS.defectParkedSuffix)
  )
    return 'defect'
  return null
}
export function collectParks(prs, options = {}) {
  const candidates = []
  for (const pr of prs.filter((p) => inWindow(p.updatedAt, options))) {
    for (const c of allContexts(pr)) {
      const code = parkCode(c)
      if (code !== null)
        candidates.push(
          occurrence('parks', `${VERIFY_CONTEXT} needs human: ${code}`, pr, pr.updatedAt),
        )
      if (c.context === VERIFY_CONTEXT && c.description === VERIFY_DESCRIPTIONS.verifiedApproved)
        candidates.push(
          occurrence(
            'parks',
            `${VERIFY_CONTEXT} approval: verified-approved`,
            pr,
            pr.updatedAt,
            '',
            'approval',
          ),
        )
    }
    if (
      pr.state === 'MERGED' &&
      contexts(commits(pr).find((c) => c.oid === pr.headRefOid)).some((c) => parkCode(c) !== null)
    )
      candidates.push(
        occurrence(
          'parks',
          `${VERIFY_CONTEXT} approval: merged-by-human`,
          pr,
          pr.updatedAt,
          '',
          'approval',
        ),
      )
  }
  return bounded(candidates)
}
export function collectCi(prs, options = {}) {
  const failed = new Map()
  for (const pr of prs.filter((p) => inWindow(p.updatedAt, options)))
    for (const c of allContexts(pr)) {
      const name = c.name ?? c.context
      if (
        !name ||
        name.startsWith('boss/') ||
        !(
          ['FAILURE', 'TIMED_OUT', 'STARTUP_FAILURE'].includes(c.conclusion) ||
          ['FAILURE', 'ERROR'].includes(c.state)
        )
      )
        continue
      if (!failed.has(name)) failed.set(name, new Map())
      failed.get(name).set(pr.number, pr)
    }
  return bounded(
    [...failed].flatMap(([name, prs]) =>
      prs.size < REPEATED_CI_MIN_PRS
        ? []
        : [...prs.values()].map((pr) => occurrence('ci', name, pr, pr.updatedAt)),
    ),
  )
}
export function areaOf(paths) {
  const dirs = paths.map((file) =>
    path.posix
      .dirname(file)
      .split('/')
      .filter((s) => s !== '.'),
  )
  if (!dirs.length) return '.'
  let count = 0
  while (count < Math.min(3, dirs[0].length) && dirs.every((dir) => dir[count] === dirs[0][count]))
    count++
  return dirs[0].slice(0, count).join('/') || '.'
}
export function collectReverts(reverts, options = {}) {
  return bounded(
    reverts
      .filter(
        (entry) =>
          entry.pr &&
          agentPr(entry.pr) &&
          entry.pr.state === 'MERGED' &&
          inWindow(entry.at, options),
      )
      .map(({ pr, at }) =>
        occurrence('reverts', areaOf(nodes(pr.files).map((file) => file.path)), pr, at),
      ),
  )
}

const commitSelection = `commits(last:10){pageInfo{hasPreviousPage} nodes{commit{oid statusCheckRollup{contexts(first:50){pageInfo{hasNextPage} nodes{__typename ... on CheckRun{name conclusion} ... on StatusContext{context state description}}}}}}}`
const prSelection = `number title state updatedAt mergedAt headRefOid files(first:100){pageInfo{hasNextPage} nodes{path}} ${commitSelection} reviews(first:20){pageInfo{hasNextPage} nodes{state body submittedAt author{login __typename}}} reviewThreads(first:50){pageInfo{hasNextPage} nodes{comments(first:1){nodes{path body createdAt author{login __typename}}}}}`
function readReason(result, tool) {
  if (result.error?.code === 'ENOENT')
    return tool === 'git'
      ? 'git-unavailable'
      : tool === 'gh'
        ? 'gh-unavailable'
        : 'boss-unavailable'
  if (result.error?.code === 'ETIMEDOUT' || result.signal === 'SIGTERM') return 'timeout'
  const message = String(result.stderr ?? '')
  if (/rate.?limit/i.test(message)) return 'rate-limited'
  if (/authenticat|not logged|login required/i.test(message)) return 'gh-unauthenticated'
  return tool === 'git' ? 'no-base-ref' : 'graphql-error'
}
function checked(run, tool, args) {
  const result = run(tool, args, { timeout: 30000 })
  if (result.error || result.status !== 0)
    throw Object.assign(new Error(readReason(result, tool)), { reason: readReason(result, tool) })
  return String(result.stdout ?? '')
}
function graphql(run, query, fields) {
  const args = [
    'api',
    'graphql',
    '-f',
    `query=${query}`,
    ...Object.entries(fields).flatMap(([k, v]) => ['-F', `${k}=${v}`]),
  ]
  let data
  try {
    data = JSON.parse(checked(run, 'gh', args))
  } catch (error) {
    if (error.reason) throw error
    throw Object.assign(error, { reason: 'graphql-error' })
  }
  if (data.errors?.length)
    throw Object.assign(new Error('GraphQL read failed'), {
      reason: /rate.?limit/i.test(JSON.stringify(data.errors)) ? 'rate-limited' : 'graphql-error',
    })
  if (!data.data)
    throw Object.assign(new Error('missing GraphQL data'), { reason: 'graphql-error' })
  return data.data
}
function nestedTruncated(pr) {
  return (
    pr.files?.pageInfo?.hasNextPage ||
    pr.commits?.pageInfo?.hasPreviousPage ||
    pr.reviews?.pageInfo?.hasNextPage ||
    pr.reviewThreads?.pageInfo?.hasNextPage ||
    commits(pr).some((c) => c.statusCheckRollup?.contexts?.pageInfo?.hasNextPage)
  )
}
export function fetchAgentPrs({ repo, start }, { run }) {
  const prs = []
  let cursor = ''
  let truncated = false
  const query = `query($search:String!,$cursor:String){search(query:$search,type:ISSUE,first:${PAGE},after:$cursor){pageInfo{hasNextPage endCursor} nodes{... on PullRequest{${prSelection}}}}}`
  for (let page = 0; page < MAX_PRS / PAGE; page++) {
    const data = graphql(run, query, {
      search: `repo:${repo} is:pr updated:>=${start} sort:updated-desc`,
      ...(cursor ? { cursor } : {}),
    })
    const connection = data.search
    if (!Array.isArray(connection?.nodes) || typeof connection.pageInfo?.hasNextPage !== 'boolean')
      throw Object.assign(new Error('malformed search'), { reason: 'graphql-error' })
    const rows = connection.nodes.slice(0, MAX_PRS - prs.length)
    prs.push(...rows)
    truncated ||= rows.some(nestedTruncated)
    if (!connection.pageInfo.hasNextPage) break
    if (prs.length >= MAX_PRS || page === MAX_PRS / PAGE - 1) {
      truncated = true
      break
    }
    if (!connection.pageInfo.endCursor || connection.pageInfo.endCursor === cursor)
      throw Object.assign(new Error('missing pagination cursor'), { reason: 'graphql-error' })
    cursor = connection.pageInfo.endCursor
  }
  return { prs: prs.filter(agentPr), truncated }
}
function readReverts({ repo, base, start, end }, { run }) {
  checked(run, 'git', ['rev-parse', '--verify', `origin/${base}^{commit}`])
  const log = checked(run, 'git', [
    'log',
    `origin/${base}`,
    `--since=${start}`,
    `--until=${end}`,
    '-n',
    '500',
    '--format=%H%x00%cI%x00%s%x00%b%x00%x1e',
  ])
  const records = log
    .split('\x1e')
    .map((line) => line.trim())
    .filter(Boolean)
  const reverts = []
  let examined = 0
  let truncated = records.length >= 500
  const cache = new Map()
  for (const record of records) {
    const [, at, subject, body] = record.split('\0')
    const sha = /This reverts commit ([a-f0-9]{7,40})/i.exec(body)?.[1]
    const escapedRepo = repo.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
    let number = Number(
      new RegExp(`Reverts ${escapedRepo}#(\\d+)`, 'i').exec(body)?.[1] ??
        /^Revert ".* \(#(\d+)\)"/.exec(subject)?.[1],
    )
    if (!sha && !number) continue
    if (++examined > MAX_REVERTS) {
      truncated = true
      break
    }
    if (!number) {
      const associated = JSON.parse(
        checked(run, 'gh', ['api', `repos/${repo}/commits/${sha}/pulls`]),
      )
      number = associated.find((p) => p.merged_at)?.number
    }
    if (!number) continue
    if (!cache.has(number)) {
      const [owner, name] = repo.split('/')
      cache.set(
        number,
        graphql(
          run,
          `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){${prSelection}}}}`,
          { owner, name, number },
        ).repository?.pullRequest,
      )
    }
    const pr = cache.get(number)
    if (pr && nestedTruncated(pr)) truncated = true
    reverts.push({ pr, at })
  }
  return { reverts, truncated }
}
function flags(argv, warn) {
  const options = { only: names, agentAuthors: [] }
  if (argv[0] !== 'collect') throw new Error('expected collect')
  for (let i = 1; i < argv.length; i++) {
    const flag = argv[i]
    if (flag === '--dry-run') options.dryRun = true
    else if (['--repo', '--base', '--since', '--only', '--agent-author'].includes(flag)) {
      const value = argv[++i]
      if (!value || value.startsWith('--')) throw new Error(`missing value for ${flag}`)
      if (flag === '--only') {
        options.only = [
          ...new Set(
            value.split(',').filter((name) => {
              if (names.includes(name)) return true
              warn(`retro-signals: ignoring unknown collector ${name}\n`)
              return false
            }),
          ),
        ]
      } else if (flag === '--agent-author') options.agentAuthors.push(value)
      else options[flag.slice(2)] = value
    } else warn(`retro-signals: ignoring unknown option ${flag}\n`)
  }
  return options
}
function readWatermark(file, fsImpl, warn) {
  if (!file) return { v: 1, collectors: {} }
  try {
    const data = JSON.parse(fsImpl.readFileSync(file, 'utf8'))
    if (
      data.v !== 1 ||
      !data.collectors ||
      typeof data.collectors !== 'object' ||
      Array.isArray(data.collectors) ||
      Object.values(data.collectors).some(
        (v) => typeof v !== 'string' || !Number.isFinite(Date.parse(v)),
      )
    )
      throw new Error('malformed watermark')
    return data
  } catch (error) {
    if (error.code !== 'ENOENT')
      warn('retro-signals: unreadable or malformed watermark; treating as absent\n')
    return { v: 1, collectors: {} }
  }
}

export function collect(options, deps = {}) {
  const fsImpl = deps.fs ?? fs
  const run = deps.run ?? ((cmd, args, opts) => spawnSync(cmd, args, { encoding: 'utf8', ...opts }))
  const warn = deps.stderr ?? ((s) => process.stderr.write(s))
  const end = new Date((deps.now ?? (() => new Date()))()).toISOString()
  // The notes CLI serializes RFC3339 timestamps at whole-second precision.
  const writeEpoch = Math.floor(Date.now() / 1000) * 1000
  const failures = []
  const collectors = {}
  const planned = []
  let file
  try {
    file = path.resolve(
      checked(run, 'git', ['rev-parse', '--git-common-dir']).trim(),
      'boss-retro-signals.json',
    )
  } catch (error) {
    warn(`retro-signals: watermark unavailable (${error.reason ?? 'git-unavailable'})\n`)
  }
  const watermark = readWatermark(file, fsImpl, warn)
  const starts = Object.fromEntries(
    options.only.map((name) => {
      const date = Date.parse(options.since ?? watermark.collectors[name] ?? '')
      return [
        name,
        new Date(
          Math.min(
            Date.parse(end),
            Math.max(
              Date.parse(end) - MAX_WINDOW_DAYS * day,
              Number.isFinite(date) ? date : Date.parse(end) - 14 * day,
            ),
          ),
        ).toISOString(),
      ]
    }),
  )
  const start = Object.values(starts).sort()[0] ?? end
  const result = {
    window: { start, end },
    truncated: false,
    collectors,
    failures,
    ...(options.dryRun ? { planned } : {}),
  }
  let repo = options.repo
  let base = options.base
  let ghReason
  try {
    const auth = run('gh', ['auth', 'status'], { timeout: 30000 })
    if (auth.error || auth.status !== 0)
      throw Object.assign(new Error('auth read failed'), {
        reason: auth.error ? readReason(auth, 'gh') : 'gh-unauthenticated',
      })
  } catch (error) {
    ghReason = error.reason ?? 'graphql-error'
  }
  if (!repo && !ghReason)
    try {
      repo = JSON.parse(
        checked(run, 'gh', ['repo', 'view', '--json', 'nameWithOwner']),
      ).nameWithOwner
    } catch (error) {
      ghReason = error.reason ?? 'no-repo'
    }
  if (!repo) ghReason ??= 'no-repo'
  if (!base && !ghReason)
    try {
      base = JSON.parse(checked(run, 'gh', ['repo', 'view', '--json', 'defaultBranchRef']))
        .defaultBranchRef?.name
    } catch {
      /* git fallback below */
    }
  if (!base)
    try {
      base = checked(run, 'git', ['symbolic-ref', 'refs/remotes/origin/HEAD'])
        .trim()
        .replace(/^refs\/remotes\/origin\//, '')
    } catch {
      base = 'main'
    }
  const prNames = options.only.filter((name) => name !== 'reverts')
  let fetched
  let prReason = ghReason
  if (prNames.length && !prReason)
    try {
      fetched = fetchAgentPrs(
        { repo, start: prNames.map((name) => starts[name]).sort()[0] },
        { run },
      )
      result.truncated ||= fetched.truncated
    } catch (error) {
      prReason = error.reason ?? 'graphql-error'
    }
  let binary
  for (const name of options.only) {
    const stats = (collectors[name] = {
      status: 'ok',
      found: 0,
      recorded: 0,
      existing: 0,
      dropped: { secretShape: 0, overCap: 0 },
    })
    const reason = name === 'reverts' ? (!repo ? ghReason : null) : prReason
    if (reason) {
      Object.assign(stats, { status: 'skipped', reason })
      continue
    }
    let complete = true
    let truncated = false
    try {
      const window = { start: starts[name], end, agentAuthors: options.agentAuthors }
      let collection
      if (name === 'reverts') {
        const read = readReverts({ repo, base, ...window }, { run })
        truncated = read.truncated
        collection = (deps.collectors?.reverts ?? collectReverts)(read.reverts, window)
      } else {
        truncated = fetched.truncated
        collection = (
          deps.collectors?.[name] ??
          { reviews: collectReviews, parks: collectParks, ci: collectCi }[name]
        )(fetched.prs, window)
      }
      result.truncated ||= truncated
      stats.found = collection.found
      stats.dropped = collection.dropped
      if (options.dryRun) {
        planned.push(...collection.notes)
        continue
      }
      for (const note of collection.notes) {
        try {
          binary ??= (deps.resolveBoss ?? resolveBossBinary)(deps.env ?? process.env, {
            fs: fsImpl,
          })
          if (!binary.ok) {
            Object.assign(stats, { status: 'skipped', reason: 'boss-unavailable' })
            complete = false
            break
          }
          const written = run(
            binary.path,
            [
              'notes',
              'add',
              '--tag',
              'improvement',
              '--json',
              '--idempotency-key',
              note.idempotencyKey,
              '--',
              note.body,
            ],
            { timeout: 30000 },
          )
          if (written.error || written.status !== 0) throw new Error('note write failed')
          const data = JSON.parse(written.stdout)
          if (!data?.id) throw new Error('note write returned no id')
          if (data.existing === true || Date.parse(data.created_at) < writeEpoch) stats.existing++
          else stats.recorded++
        } catch {
          complete = false
          failures.push({ collector: name, reason: 'note-write-failed' })
        }
      }
      if (stats.status === 'ok' && complete && !truncated) watermark.collectors[name] = end
    } catch (error) {
      if (SKIP_REASONS.includes(error.reason))
        Object.assign(stats, { status: 'skipped', reason: error.reason })
      else {
        complete = false
        Object.assign(stats, { status: 'skipped', reason: 'graphql-error' })
        failures.push({ collector: name, reason: 'collector-failed' })
      }
    }
  }
  if (!options.dryRun && file) {
    const temp = `${file}.${randomUUID()}.tmp`
    try {
      fsImpl.writeFileSync(temp, JSON.stringify(watermark) + '\n')
      fsImpl.renameSync(temp, file)
    } catch {
      warn('retro-signals: watermark write failed\n')
      try {
        fsImpl.unlinkSync(temp)
      } catch {
        /* absent temp */
      }
    }
  }
  return result
}
export function main(argv, deps = {}) {
  const stdout = deps.stdout ?? ((s) => process.stdout.write(s))
  const stderr = deps.stderr ?? ((s) => process.stderr.write(s))
  try {
    stdout(JSON.stringify(collect(flags(argv, stderr), deps)) + '\n')
  } catch {
    stdout(
      JSON.stringify({
        window: null,
        truncated: false,
        collectors: {},
        failures: [{ reason: 'collect-failed' }],
      }) + '\n',
    )
  }
  return 0
}
if (isMainModule(import.meta.url)) process.exitCode = main(process.argv.slice(2))
