import { spawnSync } from 'node:child_process'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { isMainModule } from '../main-module.mjs'
import { resolveTrackerAdapter } from '../tracker/adapter.mjs'
import { claimPublication } from './verify-escapes-publication.mjs'
import {
  extractReferences,
  ticketKeys,
  followupReferences,
  linkFix,
  weakCandidates,
  escapeRecord,
  renderEscapeComment,
  parseEscapeComment,
} from './verify-escapes.mjs'

export const MAX_WINDOW_DAYS = 31
export const MAX_FIX_PRS = 500
const DAY = 86400000
/** @param {string} cmd @param {string[]} args */
const defaultRun = (cmd, args) =>
  spawnSync(cmd, args, { encoding: 'utf8', timeout: 30000, maxBuffer: 16 * 1024 * 1024 })
/** @param {any} error */
function failure(error) {
  const text = String(error?.message ?? error)
  return (
    error?.reason ??
    (/ENOENT/.test(text)
      ? 'gh-unavailable'
      : /auth|log.?in|401/i.test(text)
        ? 'gh-unauthenticated'
        : /rate.?limit|429/i.test(text)
          ? 'rate-limited'
          : /timeout|ETIMEDOUT/i.test(text)
            ? 'timeout'
            : 'graphql-error')
  )
}
/** @param {any} deps */
function github(deps) {
  const run = deps.run ?? defaultRun
  /** @param {string[]} args */
  const checked = async (args) => {
    const r = await run('gh', args)
    // A bare #N may name an issue (or no object), rather than a PR. GitHub returns
    // this typed negative lookup with exit 1; it is not a failed history read.
    if (r.status !== 0 && !r.error && args[0] === 'api' && args[1] === 'graphql') {
      let response
      try {
        response = JSON.parse(r.stdout)
      } catch {
        /* Handle other failures below. */
      }
      if (
        response?.data?.repository?.pullRequest === null &&
        response.errors?.length &&
        response.errors.every(
          (error) =>
            error.type === 'NOT_FOUND' &&
            JSON.stringify(error.path) === '["repository","pullRequest"]' &&
            /^Could not resolve to a PullRequest with the number of \d+\.$/.test(error.message),
        )
      ) {
        return JSON.stringify({ data: response.data })
      }
    }
    if (r.status !== 0 || r.error)
      throw Object.assign(new Error(`${r.stderr ?? ''} ${r.error ?? ''}`), {
        reason: failure(r.error ?? r.stderr),
      })
    return r.stdout ?? ''
  }
  /** @param {string} query */
  const graphql = async (query) => {
    const response = JSON.parse(await checked(['api', 'graphql', '-f', `query=${query}`]))
    if (response.errors || !response.data)
      throw new Error(JSON.stringify(response.errors ?? 'missing GraphQL data'))
    return response.data
  }
  /** @param {string} query @param {number} limit */
  const search = async (query, limit) => {
    let cursor = null
    const nodes = []
    let truncated = false
    do {
      const page = (
        await graphql(
          `query { search(query:${JSON.stringify(query)},type:ISSUE,first:50${cursor ? `,after:${JSON.stringify(cursor)}` : ''}) { nodes { ... on PullRequest { number } } pageInfo { hasNextPage endCursor } } }`,
        )
      ).search
      if (!Array.isArray(page?.nodes) || !page.pageInfo) throw new Error('malformed search')
      nodes.push(...page.nodes.slice(0, limit - nodes.length))
      if (!page.pageInfo.hasNextPage) break
      if (nodes.length >= limit) {
        truncated = true
        break
      }
      if (!page.pageInfo.endCursor || page.pageInfo.endCursor === cursor)
        throw new Error('missing pagination cursor')
      cursor = page.pageInfo.endCursor
    } while (true)
    return { nodes, truncated }
  }
  /** @param {string} repo @param {number} number */
  const read = async (repo, number) => {
    const [owner, name] = repo.split('/')
    const prefix = `repository(owner:${JSON.stringify(owner)},name:${JSON.stringify(name)}) { pullRequest(number:${number})`
    const pr = (
      await graphql(
        `query { ${prefix} { number title body mergedAt baseRefName headRefOid additions deletions changedFiles labels(first:100){nodes{name}} closingIssuesReferences(first:100){nodes{title url}} files(first:100){nodes{path} pageInfo{hasNextPage endCursor}} comments(first:100){nodes{body} pageInfo{hasNextPage endCursor}} commits(last:1){nodes{commit{oid status{contexts{context state}}}}} } } }`,
      )
    ).repository?.pullRequest
    if (!pr) return null
    for (const field of ['files', 'comments']) {
      while (pr[field].pageInfo.hasNextPage) {
        const cursor = pr[field].pageInfo.endCursor
        if (!cursor) throw new Error(`missing ${field} cursor`)
        const next = (
          await graphql(
            `query { ${prefix} { ${field}(first:100,after:${JSON.stringify(cursor)}){nodes{${field === 'files' ? 'path' : 'body'}} pageInfo{hasNextPage endCursor}} } } }`,
          )
        ).repository.pullRequest[field]
        if (next.pageInfo.hasNextPage && next.pageInfo.endCursor === cursor)
          throw new Error('stalled pagination')
        pr[field] = { nodes: [...pr[field].nodes, ...next.nodes], pageInfo: next.pageInfo }
      }
    }
    const head = pr.commits?.nodes?.find((n) => n.commit.oid === pr.headRefOid)?.commit
    pr.verifyState =
      head?.status?.contexts?.find((s) => s.context === 'boss/verify')?.state?.toLowerCase() ??
      'absent'
    return { ...pr, repo, mergeHeadSha: pr.headRefOid }
  }
  return { checked, graphql, search, read, run }
}
/** @param {any} options @param {any} deps */
async function context(options, deps) {
  const gh = github(deps)
  const info = JSON.parse(
    await gh.checked([
      'repo',
      'view',
      ...(options.repo ? [options.repo] : []),
      '--json',
      'nameWithOwner,defaultBranchRef',
    ]),
  )
  return { gh, repo: options.repo ?? info.nameWithOwner, base: info.defaultBranchRef.name }
}
/** @param {any} options @param {any} deps */
export async function trace(options = {}, deps = {}) {
  const now = new Date(deps.now?.() ?? new Date())
  const until = new Date(options.until ?? now)
  const since = new Date(options.since ?? until.getTime() - 14 * DAY)
  if (
    !Number.isFinite(+since) ||
    !Number.isFinite(+until) ||
    +since > +until ||
    +until - +since > MAX_WINDOW_DAYS * DAY
  )
    throw new Error('invalid window: maximum 31 days')
  const result = {
    window: { since: since.toISOString(), until: until.toISOString() },
    truncated: false,
    scanned: 0,
    links: [],
    candidates: [],
    written: 0,
    existing: 0,
    pending: [],
    skipped: {},
    failures: [],
    warnings: [],
    unresolved: { 'ticket-unresolved': 0 },
  }
  let operation = 'repository'
  try {
    const { gh, repo, base } = await context(options, deps)
    const cache = new Map()
    const read = async (number) => {
      operation = `read-pr:${number}`
      if (!cache.has(number)) cache.set(number, await gh.read(repo, number))
      return cache.get(number)
    }
    operation = 'search-window'
    const scan = await gh.search(
      `repo:${repo} is:pr is:merged base:${base} merged:${since.toISOString()}..${until.toISOString()}`,
      MAX_FIX_PRS,
    )
    result.truncated = scan.truncated
    let adapter = deps.adapter
    if (adapter === undefined)
      try {
        adapter = resolveTrackerAdapter()
      } catch {
        result.skipped.followups = 'tracker adapter unavailable'
      }
    if (typeof adapter?.readDescription !== 'function')
      result.skipped.followups ??= 'adapter has no readDescription'
    const ticketCache = new Map(),
      prepared = []
    /** @type {Record<string, any[]>} */
    const ticketPrs = {}
    for (const { number } of scan.nodes) {
      const fix = await read(number)
      if (!fix) throw new Error(`missing fix PR ${number}`)
      fix.defaultBranch = base
      result.scanned++
      /** @type {Record<string, string>} */
      const tickets = {}
      if (!result.skipped.followups)
        for (const key of ticketKeys(fix)) {
          if (!ticketCache.has(key))
            try {
              operation = `read-ticket:${key}`
              ticketCache.set(key, (await adapter.readDescription(key)).description)
            } catch (error) {
              if (
                error.code === 'TRACKER_ISSUE_NOT_FOUND' ||
                error.message === 'Linear GraphQL error: Entity not found: Issue' ||
                /^tracker\/linear readDescription: no issue found for id /.test(error.message ?? '')
              ) {
                ticketCache.set(key, null)
                result.unresolved['ticket-unresolved']++
                result.warnings.push(`ticket-unresolved:${key}`)
                continue
              }
              if (error.code === 'TRACKER_CREDENTIALS_MISSING') {
                result.skipped.followups = 'tracker credentials missing'
                break
              }
              throw error
            }
          if (typeof ticketCache.get(key) === 'string') tickets[key] = ticketCache.get(key)
        }
      const references = extractReferences(`${fix.title}\n${fix.body ?? ''}`)
      for (const description of Object.values(tickets))
        for (const sentence of followupReferences(description)) {
          references.push(...sentence.references)
          for (const key of sentence.tickets) {
            if (!Object.hasOwn(ticketPrs, key)) {
              operation = `resolve-ticket:${key}`
              const matches = await gh.search(`repo:${repo} is:pr is:merged in:title "[${key}]"`, 3)
              ticketPrs[key] = matches.truncated ? [] : matches.nodes
              if (ticketPrs[key].length !== 1) result.unresolved['ticket-unresolved']++
            }
            references.push(...ticketPrs[key])
          }
        }
      const originals = []
      for (const n of new Set(
        references.filter((r) => !r.repo || r.repo === repo).map((r) => r.number),
      )) {
        const pr = await read(n)
        if (pr) originals.push(pr)
      }
      const links = linkFix({ fix, originals, tickets, ticketPrs })
      const candidates = weakCandidates({ fix, originals }).filter(
        (c) => !links.some((l) => l.original.number === c.original),
      )
      result.candidates.push(...candidates)
      for (const judgement of options.judgements ?? []) {
        if (judgement.fix !== fix.number) continue
        const candidate = candidates.find((c) => c.original === judgement.original)
        if (!candidate) {
          result.warnings.push(
            `ignored judgement on unlisted pair ${fix.number}/${judgement.original}`,
          )
          continue
        }
        if (
          judgement.verdict === 'escape' &&
          typeof judgement.reason === 'string' &&
          judgement.reason.trim() &&
          !links.some((link) => link.original.number === judgement.original)
        )
          links.push({
            original: originals.find((o) => o.number === judgement.original),
            basis: 'judge',
            evidence: judgement.reason,
          })
      }
      for (const link of links)
        prepared.push(escapeRecord({ repo, fix, link, tracedAt: now.toISOString() }))
    }
    for (const judgement of options.judgements ?? []) {
      const warning = `ignored judgement on unlisted pair ${judgement.fix}/${judgement.original}`
      if (
        !result.candidates.some(
          (c) => c.fix === judgement.fix && c.original === judgement.original,
        ) &&
        !result.warnings.includes(warning)
      )
        result.warnings.push(warning)
    }
    // Finish the history scan before writing; publication conflicts need fresh reads.
    result.links = prepared
    if (options.dryRun) return result
    let labelCreated = false
    operation = 'publish'
    for (const record of prepared) {
      const original = cache.get(record.original.number)
      if (
        original.comments.nodes.some((c) =>
          new RegExp(`<!-- boss-escape v1 fix=${record.fix.number} -->`).test(c.body),
        )
      ) {
        result.existing++
        continue
      }
      operation = `claim:${record.original.number}/${record.fix.number}`
      const claim = await claimPublication(record, gh.run)
      if (!claim.owned) {
        // The first scan can predate the winner's publication. Re-read without
        // the scan cache, but never take over a claim with an absent marker.
        let current
        try {
          current = await gh.read(repo, record.original.number)
        } catch (error) {
          throw Object.assign(error, { ref: claim.ref })
        }
        if (
          current?.comments.nodes.some((c) =>
            c.body.includes(`<!-- boss-escape v1 fix=${record.fix.number} -->`),
          )
        ) {
          result.existing++
          continue
        }
        result.pending.push({
          ref: claim.ref,
          original: record.original.number,
          fix: record.fix.number,
        })
        result.failures.push({ reason: 'publication-pending', operation })
        continue
      }
      // Retain the fence even when a later write fails: a lost response does
      // not prove that GitHub rejected the comment.
      operation = `publish:${record.original.number}/${record.fix.number}`
      try {
        if (!labelCreated) {
          await gh.checked([
            'label',
            'create',
            'boss-escape',
            '--force',
            '--repo',
            repo,
            '--description',
            'Defect repaired after merge',
          ])
          labelCreated = true
        }
        await gh.checked([
          'pr',
          'edit',
          String(record.original.number),
          '--repo',
          repo,
          '--add-label',
          'boss-escape',
        ])
        await gh.checked([
          'pr',
          'comment',
          String(record.original.number),
          '--repo',
          repo,
          '--body',
          renderEscapeComment(record),
        ])
        original.comments.nodes.push({ body: renderEscapeComment(record) })
        result.written++
      } catch (error) {
        result.pending.push({
          ref: claim.ref,
          original: record.original.number,
          fix: record.fix.number,
        })
        result.failures.push({
          reason: 'publication-indeterminate',
          cause: failure(error),
          operation,
        })
      }
    }
  } catch (error) {
    if (error.ref) result.pending.push({ ref: error.ref })
    result.failures.push({ reason: failure(error), operation })
  }
  return result
}
/** @param {any} options @param {any} deps */
export async function list(options, deps = {}) {
  if (!options.since || !Number.isFinite(Date.parse(options.since)))
    throw new Error('list requires --since ISO date')
  const { gh, repo } = await context(options, deps)
  const search = await gh.search(`repo:${repo} is:pr label:boss-escape`, 1000)
  if (search.truncated) throw new Error('escape search truncated')
  const result = { escapes: [], unreadable: 0 }
  const seen = new Set()
  for (const { number } of search.nodes) {
    const pr = await gh.read(repo, number)
    if (!pr) throw new Error(`missing labelled PR ${number}`)
    for (const { body } of pr.comments.nodes) {
      if (!body.includes('<!-- boss-escape')) continue
      const record = parseEscapeComment(body)
      if (!record || record.repo !== repo || record.original.number !== number) {
        result.unreadable++
        continue
      }
      const key = `${number}/${record.fix.number}`
      if (
        Date.parse(record.fix.mergedAt) >= Date.parse(options.since) &&
        (!options.area || record.areas.some((a) => a.area === options.area)) &&
        !seen.has(key)
      ) {
        result.escapes.push(record)
        seen.add(key)
      }
    }
  }
  return result
}
/** @param {string[]} argv @param {any} deps */
export async function main(argv = process.argv.slice(2), deps = {}) {
  const stdout = deps.stdout ?? ((text) => process.stdout.write(text))
  const stderr = deps.stderr ?? ((text) => process.stderr.write(text))
  let parsed
  try {
    parsed = parseArgs({
      args: argv,
      strict: true,
      allowPositionals: true,
      options: {
        since: { type: 'string' },
        until: { type: 'string' },
        repo: { type: 'string' },
        area: { type: 'string' },
        'dry-run': { type: 'boolean' },
        judgements: { type: 'string' },
      },
    })
  } catch (error) {
    stderr(`${error.message}\n`)
    return 64
  }
  const [command, ...extra] = parsed.positionals
  if (extra.length || !['trace', 'list'].includes(command)) {
    stderr('usage: trace [--since ISO] [--until ISO] [--dry-run] | list --since ISO\n')
    return 64
  }
  try {
    const options = {
      ...parsed.values,
      dryRun: parsed.values['dry-run'],
      judgements: parsed.values.judgements
        ? JSON.parse(readFileSync(parsed.values.judgements, 'utf8'))
        : [],
    }
    if (!Array.isArray(options.judgements)) throw new Error('judgements must be an array')
    const result = await (command === 'trace' ? trace(options, deps) : list(options, deps))
    stdout(`${JSON.stringify(result, null, 2)}\n`)
    return result.failures?.length ? 1 : 0
  } catch (error) {
    stderr(`${error.message}\n`)
    return 1
  }
}
if (isMainModule(import.meta.url)) process.exitCode = await main()
