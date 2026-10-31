import { blockerVerdict, classifyBlocker, prUrlsOf } from './blocker-clearance.mjs'
import { MAX_GATE_PR_LOOKUPS } from '../linear-deps-lib.mjs'
import { readFileSync } from 'node:fs'
import { parseArgs } from 'node:util'
import { loadSkillConfig, withTrackerDefaults } from '../skill-config.mjs'
import { matchIssue, validateSelectionBlock } from '../selection.mjs'
import { isMainModule } from '../main-module.mjs'
import { githubStates, GITHUB_CLOSING_ROLES, parseRepository } from './github-config.mjs'
import { formatClaimComment, resolveClaimVerdict } from '../linear-claim.mjs'
import { runGh } from './github-gh.mjs'
import { runVerb } from './github-verbs.mjs'
import { githubSelectMarked } from './github-marked.mjs'
import { githubApplyIssueWrites } from './github-issue-writes.mjs'
import { githubApplyIssueCreate } from './github-issue-create.mjs'
import { createGithubRelations, readDependencies, isUnblocked } from './github-relations.mjs'

const FIELDS = 'number,title,body,state,stateReason,labels,assignees,author,createdAt,url'
const QUERY_KEYS = new Set([
  'state',
  'selection',
  'requireLabels',
  'excludeLabels',
  'limit',
  'team',
])

/** @param {unknown} value */
export function issueNumber(value) {
  const match =
    /^(?:GH-|#)?([1-9]\d*)$/.exec(String(value)) ??
    /^https?:\/\/[^/]+\/[^/]+\/[^/]+\/issues\/([1-9]\d*)\/?$/.exec(String(value))
  if (!match) throw new Error('github tracker: invalid issue identifier')
  return match[1]
}

/** @param {{config?: object, env?: object, spawn?: Function, sleep?: (ms: number) => Promise<void>}} [options] */
export function createGithubAdapter({
  config = loadSkillConfig(),
  env = process.env,
  spawn,
  sleep,
} = {}) {
  const block = withTrackerDefaults(config).trackerConfig?.github
  const identity = parseRepository(block?.repository)
  const repository = `${identity.host}/${identity.owner}/${identity.repo}`
  const states = githubStates(block?.states)
  const openRoles = Object.keys(states).filter((role) => !Object.hasOwn(GITHUB_CLOSING_ROLES, role))
  const gh = (args, options = {}) => runGh(args, { env, spawn, ...options })
  const api = (path, args = [], options = {}) =>
    gh(['api', path, '--hostname', identity.host, ...args], options)
  const issue = (verb, args = [], options = {}) =>
    gh(['issue', verb, ...args, '--repo', repository], options)
  let labels
  const repositoryLabels = () => {
    if (!labels) {
      labels = gh(['label', 'list', '--repo', repository, '--json', 'name', '--limit', '1000'])
      if (!Array.isArray(labels) || labels.length >= 1000)
        throw new Error('github tracker: label snapshot incomplete')
      labels = labels.map((label) => label.name)
    }
    return labels
  }
  const resolveLabel = (name) => {
    const result = repositoryLabels().find(
      (label) => label.toLowerCase() === String(name).toLowerCase(),
    )
    if (!result)
      throw new Error(
        `github tracker: missing label ${name}; create it (gh label create <name>) or run boss init --merge --create-labels`,
      )
    return result
  }
  const normalizeTicket = (raw) => {
    const labels = (raw.labels ?? []).map((label) =>
      typeof label === 'string' ? label : label.name,
    )
    const conflicts = openRoles
      .map((role) => states[role])
      .filter((name) => labels.some((label) => label.toLowerCase() === name.toLowerCase()))
    const closed = String(raw.state).toUpperCase() === 'CLOSED'
    const role = closed
      ? ({ NOT_PLANNED: 'canceled', DUPLICATE: 'duplicate' }[raw.stateReason] ?? 'done')
      : conflicts.length === 1
        ? openRoles.find((role) => states[role] === conflicts[0])
        : null
    const priority = labels.find((label) => /^boss:priority-[1-4]$/.test(label))
    const estimate = labels.find((label) => /^boss:estimate-\d+$/.test(label))
    const assignees = raw.assignees ?? []
    return {
      id: `GH-${raw.number}`,
      identifier: `GH-${raw.number}`,
      title: raw.title,
      description: raw.body ?? '',
      priority: priority ? Number(priority.split('-').at(-1)) : 0,
      estimate: estimate ? Number(estimate.split('-').at(-1)) : null,
      createdAt: raw.createdAt,
      stateName: role ? states[role] : null,
      stateType:
        role === 'done'
          ? 'completed'
          : closed
            ? 'canceled'
            : ['unplanned', 'planned'].includes(role)
              ? 'unstarted'
              : role
                ? 'started'
                : null,
      status: role ? states[role] : null,
      statusType:
        role === 'done'
          ? 'completed'
          : closed
            ? 'canceled'
            : ['unplanned', 'planned'].includes(role)
              ? 'unstarted'
              : role
                ? 'started'
                : null,
      stateConflict: !closed && conflicts.length > 1 ? conflicts : [],
      labels,
      blockedBy: (raw.relations?.blockedBy ?? []).map((edge) => edge.identifier ?? edge.id),
      parentId: raw.parentId ?? null,
      planUrl: null,
      planAttachment:
        raw.attachments?.find(
          (attachment) => attachment.title === `Implementation plan (GH-${raw.number})`,
        ) ?? null,
      attachments: raw.attachments ?? [],
      url: raw.url,
      assigneeId:
        assignees.length === 1 ? assignees[0].login : assignees.length === 0 ? null : undefined,
      creatorId: raw.author?.login,
    }
  }
  const resolveSelection = async (query) => {
    const selection = query.selection ?? {}
    const errors = validateSelectionBlock(selection)
    if (errors?.length) throw new Error(`github tracker: invalid selection: ${errors.join(', ')}`)
    const result = {}
    let viewer
    for (const dimension of ['labels', 'assignees', 'creators', 'projects']) {
      result[dimension] = { include: [], exclude: [] }
      for (const polarity of ['include', 'exclude'])
        for (const ref of selection[dimension]?.[polarity] ?? []) {
          if (dimension === 'projects')
            throw new Error('project selection is not supported by the github tracker')
          if (dimension === 'labels') result[dimension][polarity].push(resolveLabel(ref))
          else {
            if (ref !== 'me')
              throw new Error('GitHub cannot resolve an email or id selector; use me')
            viewer ??= api('user').login
            if (typeof viewer !== 'string' || !viewer)
              throw new Error('github tracker: viewer identity unavailable')
            result[dimension][polarity].push(viewer)
          }
        }
    }
    return result
  }
  const selectPlanned = async (query) => {
    for (const key of Object.keys(query ?? {}))
      if (!QUERY_KEYS.has(key)) throw new Error(`github tracker: unsupported query key ${key}`)
    if (query?.team !== undefined) {
      const team = String(query.team).toLowerCase()
      const configured = repository.toLowerCase()
      const local = `${identity.owner}/${identity.repo}`.toLowerCase()
      if (team !== configured && !(identity.host.toLowerCase() === 'github.com' && team === local))
        throw new Error(
          `github tracker: --team ${query.team} does not name the configured repository ${block.repository}`,
        )
    }
    if (!query?.state || !openRoles.some((role) => states[role] === query.state))
      throw new Error('github tracker: selection requires an open workflow state')
    const state = resolveLabel(query.state)
    const requireLabels = (query.requireLabels ?? []).map(resolveLabel)
    const excludeLabels = [
      ...new Set([block.labels.needsHuman ?? 'needs-human', ...(query.excludeLabels ?? [])]),
    ].map(
      (name) =>
        repositoryLabels().find((label) => label.toLowerCase() === String(name).toLowerCase()) ??
        name,
    )
    const selection = await resolveSelection(query)
    const rows = issue('list', [
      '--state',
      'open',
      '--label',
      state,
      ...requireLabels.flatMap((label) => ['--label', label]),
      '--json',
      FIELDS,
      '--limit',
      '1000',
    ])
    if (!Array.isArray(rows) || rows.length >= 1000)
      throw new Error('github tracker: issue snapshot incomplete (1000 cap)')
    return rows
      .map(normalizeTicket)
      .filter(
        (row) =>
          row.stateName === query.state &&
          matchIssue(row, selection, { requireLabels, excludeLabels }).matches,
      )
      .sort(
        (a, b) =>
          (a.priority || 5) - (b.priority || 5) ||
          String(a.createdAt).localeCompare(String(b.createdAt)),
      )
      .slice(0, Math.min(250, Math.max(1, Math.floor(query.limit ?? 250))))
  }
  const summaries = {
    selectPlanned: ['select-planned', '{state, labels, limit} -> normalized candidate rows'],
    getIssue: ['get-issue', '{id} -> issue with comments'],
    createIssue: [
      'create-issue',
      '{title, description, labels, state, parentId, priority, estimate} -> verified native child; parentId, priority and estimate are optional',
    ],
    writeSourceNotes: [
      'write-source-notes',
      '{issue, file, title} -> write-once verified Source notes record',
    ],
    writeEpicSpec: ['write-epic-spec', '{issue, file, title} -> append-only verified epic spec'],
    writePlan: ['write-plan', '{issue, file, title} -> verified in-place plan record'],
    readPlanAttachment: [
      'read-plan',
      "{id: plan record id, outFile} -> plan bytes written atomically to disk; use writePlan's result id or getIssue's planAttachment.id (the matching attachments[].id for an Epic spec record); an issue id is refused",
    ],
    readComments: ['read-comments', '{issueId} -> comments with id, body and createdAt'],
    writeComment: ['write-comment', '{issueId, body} -> posts a comment'],
    deleteComment: ['delete-comment', '{id} -> deletes the identified comment'],
    updateComment: ['update-comment', '{id, body} -> updates an existing comment'],
    readLabels: ['read-labels', '{id} -> current label names'],
    moveState: [
      'move-state',
      '{id, state} -> transition state; optional labels is the whole desired label set',
    ],
    setPriorityEstimate: [
      'set-priority-estimate',
      '{id, priority(1-4), estimate(fib)} -> set ranking; optional labels is the desired label set',
    ],
    appendDependency: [
      'append-dependency',
      '{id, blockedBy: [ids]} -> {id, blockedBy: [ids]}; cycle-checked by caller',
    ],
    listChildren: ['list-children', '{parentId} -> {parent, children} with hydrated relations'],
    writeDescription: ['write-description', '{id, description} -> replace and verify issue body'],
    preflight: ['preflight', '{} -> self-check for command-only tracker'],
  }
  const context = {
    identity,
    sleep,
    relations: null,
    block,
    states,
    openRoles,
    gh,
    api,
    issue,
    repositoryLabels,
    resolveLabel,
    normalizeTicket,
    selectPlanned,
    issueNumber,
    fields: FIELDS,
  }
  const relations = createGithubRelations(context)
  context.relations = relations
  return {
    tracker: 'github',
    applyIssueWrites: (writes = {}) => githubApplyIssueWrites(writes, context),
    applyIssueCreate: (input) =>
      githubApplyIssueCreate(input, {
        ...context,
        moveState: (args) => runVerb('move-state', args, context),
      }),
    states: () => ({ ...states }),
    normalizeTicket,
    resolveSelection,
    selectPlanned,
    selectMarked: (query = {}) => githubSelectMarked(query, { api, identity }),
    hasWork: async (query) => {
      if (
        !['selection', 'requireLabels', 'excludeLabels'].some((key) =>
          Object.hasOwn(query ?? {}, key),
        )
      )
        throw new Error('github tracker: legacy gate query is not supported')
      return (await selectPlanned({ ...query, limit: 1 })).length > 0
    },
    /** @param {object} query
     * @param {{prLookup?: Function, inReviewState?: string}} options */
    hasUnblockedWork: async (query, { prLookup, inReviewState = states.inReview } = {}) => {
      if (
        !['selection', 'requireLabels', 'excludeLabels'].some((key) =>
          Object.hasOwn(query ?? {}, key),
        )
      )
        throw new Error('github tracker: legacy gate query is not supported')
      const candidates = []
      for (const candidate of await selectPlanned({ ...query, limit: 250 })) {
        const blockers = relations.readBlockers(candidate.id)
        if (blockerVerdict(blockers) === 'unblocked') return true
        candidates.push(blockers)
      }
      if (!prLookup) return false
      const prFacts = new Map()
      for (const blockers of candidates) {
        for (const blocker of blockers) {
          if (classifyBlocker(blocker).clearance !== 'needs-pr-facts') continue
          for (const url of prUrlsOf(blocker)) {
            if (prFacts.has(url) || prFacts.size >= MAX_GATE_PR_LOOKUPS) continue
            try {
              prFacts.set(url, await prLookup(url))
            } catch {
              prFacts.set(url, { error: 'lookup failed' })
            }
          }
        }
        if (blockerVerdict(blockers, { prFacts, mode: 'gate', inReviewState }) === 'unblocked')
          return true
      }
      return false
    },
    readBlockers: async (ids, { mode = 'strict' } = {}) =>
      ids.map((id) => ({
        id,
        blockers: relations.readBlockers(id),
        ...(mode === 'selection' ? { description: relations.readDescription(id) } : {}),
      })),
    readDependencies,
    isUnblocked,
    claimScopes: ['build', 'plan'],
    formatClaimComment: (token, sessionId, options) =>
      formatClaimComment(token, sessionId, options),
    resolveClaim: (comments, myToken, options = null) =>
      resolveClaimVerdict(comments, myToken, options, { orderBy: 'commentId' }),
    operationMap: Object.fromEntries(
      Object.entries(summaries).map(([key, [verb, summary]]) => [
        key,
        { command: { helper: 'tracker/github.mjs', verb }, summary },
      ]),
    ),
    // Helper entry point shares the adapter's identity and normalization, avoiding divergent reads.
    runOperation: (verb, args) => runVerb(verb, args, context),
  }
}

/** @param {string[]} argv @param {{config?: object, env?: object, spawn?: Function, sleep?: (ms: number) => Promise<void>, stdout?: Function, stderr?: Function}} [deps] */
export async function main(argv, deps = {}) {
  try {
    const { values, positionals } = parseArgs({
      args: argv,
      options: { 'args-file': { type: 'string' } },
      allowPositionals: true,
      strict: true,
    })
    if (positionals.length !== 1 || !values['args-file'])
      throw new Error('usage: github.mjs <verb> --args-file <json-file>')
    const args = JSON.parse(readFileSync(values['args-file'], 'utf8'))
    if (!args || typeof args !== 'object' || Array.isArray(args))
      throw new Error('github tracker: arguments must be an object')
    const result = await createGithubAdapter(deps).runOperation(positionals[0], args)
    ;(deps.stdout ?? ((text) => process.stdout.write(text)))(JSON.stringify(result) + '\n')
    return 0
  } catch (error) {
    ;(deps.stderr ?? ((text) => process.stderr.write(text)))(error.message + '\n')
    return 2
  }
}
if (isMainModule(import.meta.url)) process.exitCode = await main(process.argv.slice(2))
