import { mkdtempSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

import { withBoundedRetry, classifyTrackerOutcome } from './outcome.mjs'

export const DEPENDENCY_READBACK_RETRY = Object.freeze({
  maxAttempts: 6,
  baseDelayMs: 500,
  maxDelayMs: 4000,
})

class ReadbackLag extends Error {}

// REST API 2026-03-10 exposes summary totals independently from paginated edge lists.
// https://docs.github.com/en/rest/issues/issue-dependencies
// https://docs.github.com/en/rest/issues/sub-issues
export function createGithubRelations(ctx) {
  const api = (path, args = []) =>
    ctx.api(path, ['-H', 'X-GitHub-Api-Version: 2026-03-10', ...args])
  const local = `${ctx.identity.owner}/${ctx.identity.repo}`
  const target = (value, foreign = false) => {
    const text = String(value)
    const match =
      /^https?:\/\/([^/]+)\/(?:api\/v3\/repos\/|repos\/)?([^/]+)\/([^/]+)\/issues\/([1-9]\d*)\/?$/.exec(
        text,
      )
    if (!match) return { repository: local, number: ctx.issueNumber(value) }
    const host = match[1] === 'api.github.com' ? 'github.com' : match[1]
    const repository = `${match[2]}/${match[3]}`
    if (
      host !== ctx.identity.host ||
      (!foreign && repository.toLowerCase() !== local.toLowerCase())
    )
      throw new Error(`github tracker: foreign repository issue ${text}`)
    return { repository, number: match[4] }
  }
  const identifier = (ref) =>
    ref.repository.toLowerCase() === local.toLowerCase()
      ? `GH-${ref.number}`
      : `https://${ctx.identity.host}/${ref.repository}/issues/${ref.number}`
  const path = (ref) => `repos/${ref.repository}/issues/${ref.number}`
  const readRaw = (ref) => {
    const row = api(path(ref))
    if (
      !row ||
      String(row.number) !== String(ref.number) ||
      !['open', 'closed'].includes(row.state)
    )
      throw new Error(`github tracker: unreadable issue or state for ${identifier(ref)}`)
    return row
  }
  const toRaw = (row) => ({
    ...row,
    state: row.state?.toUpperCase(),
    stateReason: row.state_reason?.toUpperCase(),
    createdAt: row.created_at,
    url: row.html_url,
    author: row.user,
  })
  const relation = (row) => {
    const ref = target(row.html_url, true)
    if (!['open', 'closed'].includes(row.state))
      throw new Error(`github tracker: unreadable dependency state for ${identifier(ref)}`)
    const normalized = ctx.normalizeTicket(toRaw(row))
    return {
      identifier: identifier(ref),
      id: identifier(ref),
      number: row.number,
      url: row.html_url,
      repository: ref.repository,
      state: { name: normalized.stateName ?? row.state, type: normalized.stateType ?? 'unstarted' },
    }
  }
  const snapshot = (ref, suffix, total, label) => {
    const pages = api(`${path(ref)}/${suffix}`, ['--paginate', '--slurp'])
    if (!Array.isArray(pages) || pages.some((page) => !Array.isArray(page)))
      throw new Error(`github tracker: unreadable ${label} pagination`)
    const rows = pages.flat()
    return {
      rows,
      total,
      agrees: Number.isSafeInteger(total) && total >= 0 && rows.length === total,
    }
  }
  const summaryError = (ref, label, { rows, total }) =>
    `github tracker: ${label} list for ${identifier(ref)} disagrees with its summary count (${rows.length} listed, ${total} counted); re-run`
  const list = (ref, suffix, total, label) => {
    const result = snapshot(ref, suffix, total, label)
    if (!result.agrees) throw new Error(summaryError(ref, label, result))
    return result.rows
  }
  const edgeSnapshot = (ref, row, direction) => {
    const result = snapshot(
      ref,
      `dependencies/${direction}`,
      row.issue_dependencies_summary?.[`total_${direction}`],
      direction === 'blocked_by' ? 'blocked-by' : 'blocking',
    )
    return { ...result, rows: result.rows.map(relation) }
  }
  const retryReadback = (attempt) =>
    withBoundedRetry(attempt, {
      ...DEPENDENCY_READBACK_RETRY,
      sleep: ctx.sleep,
      classify: (error) =>
        error instanceof ReadbackLag
          ? { retry: true }
          : classifyTrackerOutcome(error, { operation: 'read' }),
    })
  const readEdges = (ref, row, direction) =>
    list(
      ref,
      `dependencies/${direction}`,
      row.issue_dependencies_summary?.[`total_${direction}`],
      direction === 'blocked_by' ? 'blocked-by' : 'blocking',
    ).map(relation)
  const hydrate = (value) => {
    const ref = target(value, true),
      row = readRaw(ref)
    const blockedBy = readEdges(ref, row, 'blocked_by'),
      blocks = readEdges(ref, row, 'blocking')
    let parentId = null
    if (row.parent_issue_url) parentId = identifier(target(row.parent_issue_url, true))
    else {
      try {
        parentId = relation(api(`${path(ref)}/parent`)).identifier
      } catch (error) {
        if (!/HTTP 404\b/.test(error.message)) throw error
      }
    }
    return {
      ...ctx.normalizeTicket({ ...toRaw(row), relations: { blockedBy, blocks }, parentId }),
      id: identifier(ref),
      identifier: identifier(ref),
      relations: { blockedBy, blocks },
      parentId,
    }
  }
  const listChildren = (parentId) => {
    const ref = target(parentId),
      raw = readRaw(ref)
    const rows = list(ref, 'sub_issues', raw.sub_issues_summary?.total, 'sub-issue')
    for (const row of rows) target(row.html_url)
    const parent = hydrate(parentId),
      children = rows.map((row) => hydrate(row.html_url))
    const byId = new Map(children.map((row) => [row.identifier, row]))
    const relationWarnings = []
    for (const child of children) {
      for (const [key, inverse] of [
        ['blockedBy', 'blocks'],
        ['blocks', 'blockedBy'],
      ]) {
        for (const edge of child.relations[key]) {
          const peer = byId.get(edge.identifier)
          if (!peer || peer.relations[inverse].some((row) => row.identifier === child.identifier))
            continue
          peer.relations[inverse].push({
            identifier: child.identifier,
            id: child.id,
            number: Number(ctx.issueNumber(child.id)),
            url: child.url,
            repository: local,
            state: { name: child.stateName, type: child.stateType },
          })
          const blocker = key === 'blocks' ? child.identifier : peer.identifier
          const dependent = key === 'blocks' ? peer.identifier : child.identifier
          relationWarnings.push(
            `${blocker} blocks ${dependent} was read only from ${child.identifier}`,
          )
        }
      }
    }
    for (const child of children)
      child.blockedBy = child.relations.blockedBy.map((row) => row.identifier)
    return { parent, children, relationWarnings }
  }
  const appendDependency = async ({ id, blockedBy }) => {
    if (!Array.isArray(blockedBy)) throw new Error('github tracker: blockedBy must be an array')
    const ref = target(id)
    const sources = blockedBy.map((value) => target(value))
    if (sources.some((source) => String(source.number) === String(ref.number)))
      throw new Error('github tracker: self dependency is not allowed')
    const row = readRaw(ref)
    const sourceRows = sources.map((source) => ({ source, row: readRaw(source) }))
    for (const { row } of sourceRows)
      if (!Number.isSafeInteger(row.id) || row.id <= 0)
        throw new Error('github tracker: unreadable dependency REST id')
    let current = await retryReadback((attempt) => {
      const result = edgeSnapshot(ref, attempt === 1 ? row : readRaw(ref), 'blocked_by')
      if (!result.agrees) throw new ReadbackLag(summaryError(ref, 'blocked-by', result))
      return result.rows
    })
    const posted = []
    for (const { source, row } of sourceRows) {
      if (
        current.some((edge) => edge.identifier === identifier(source)) ||
        posted.some((edge) => identifier(edge) === identifier(source))
      )
        continue
      const temp = mkdtempSync(join(tmpdir(), 'github-dependency-'))
      try {
        // POST takes the global REST issue id, not the repository issue number.
        const file = join(temp, 'dependency.json')
        writeFileSync(file, JSON.stringify({ issue_id: row.id }), { mode: 0o600 })
        api(`${path(ref)}/dependencies/blocked_by`, ['--method', 'POST', '--input', file])
        posted.push(source)
      } catch (error) {
        throw new Error(
          `github tracker: add ${identifier(source)} blocks ${identifier(ref)}: write may have applied; posted before failure: [${posted.map(identifier).join(', ')}]; ${error.message}`,
        )
      } finally {
        rmSync(temp, { recursive: true, force: true, maxRetries: 3 })
      }
    }
    let warnings = []
    if (posted.length) {
      let missing = [],
        reads = 0
      try {
        await retryReadback((attempt) => {
          reads = attempt
          const forward = edgeSnapshot(ref, readRaw(ref), 'blocked_by')
          current = forward.rows
          missing = posted.filter(
            (source) => !current.some((edge) => edge.identifier === identifier(source)),
          )
          warnings = []
          const summaryWarning = (source, side, result) => {
            if (!result.agrees)
              warnings.push(
                `${side} summary for ${identifier(source)} counts ${result.total} but lists ${result.rows.length}`,
              )
          }
          summaryWarning(ref, 'blocked-by', forward)
          for (const source of posted) {
            const mirror = edgeSnapshot(source, readRaw(source), 'blocking')
            if (!mirror.rows.some((edge) => edge.identifier === identifier(ref)))
              warnings.push(
                `${identifier(source)} blocks ${identifier(ref)} not yet listed on ${identifier(source)}'s blocking side after ${attempt} reads`,
              )
            summaryWarning(source, 'blocking', mirror)
          }
          if (missing.length || warnings.length)
            throw new ReadbackLag('dependency read-back still converging')
        })
      } catch (error) {
        const context = `github tracker: read back ${posted.map(identifier).join(', ')} blocks ${identifier(ref)}: write may have applied; `
        if (!(error instanceof ReadbackLag)) throw new Error(context + error.message)
        if (missing.length)
          throw new Error(
            context +
              `forward blocked-by list for ${identifier(ref)} still lacks ${missing.map(identifier).join(', ')} after ${reads} reads`,
          )
      }
    }
    return {
      id: identifier(ref),
      blockedBy: current.map((edge) => edge.identifier),
      ...(warnings.length ? { warnings } : {}),
    }
  }
  const readBlockers = (value) => {
    const ref = target(value, true)
    return readEdges(ref, readRaw(ref), 'blocked_by')
  }
  return {
    hydrate,
    readBlockers,
    listChildren,
    appendDependency,
    readPath: (value) => path(target(value, true)),
    localNumber: (value) => target(value).number,
  }
}

export function readDependencies(issue) {
  if (!Array.isArray(issue?.relations?.blockedBy))
    throw new Error('github tracker: dependency relations are absent; hydrate the issue first')
  return issue.relations.blockedBy
}
export function isUnblocked(issue) {
  return readDependencies(issue).every((edge) =>
    ['completed', 'canceled'].includes(edge?.state?.type),
  )
}
