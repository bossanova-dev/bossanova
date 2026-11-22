import { isRateLimitRefusal, quotaOf } from './gh-quota-read.mjs'

function graphqlValue(payload) {
  if (!payload || payload.errors?.length || !payload.data?.repository)
    throw new Error('unreadable GraphQL response')
  return payload.data.repository
}

/** Read through the caller's seam, preserving its primary call (and cache key). */
export function readAcrossQuotas(read, primary, alternate) {
  const attempt = (request) => {
    const result = read(...request.call)
    const errors = result.payload?.errors
    if (!result.ok || errors?.length)
      return {
        ok: false,
        refused:
          (!result.error || typeof result.exit === 'number') &&
          (isRateLimitRefusal(String(result.stderr ?? '')) ||
            errors?.some((error) => error.type === 'RATE_LIMITED') === true),
      }
    try {
      return { ok: true, value: request.normalize(result.payload) }
    } catch {
      return { ok: false, refused: false }
    }
  }
  const first = attempt(primary)
  if (first.ok) return { ok: true, value: first.value, degraded: null }
  if (!first.refused) return { ok: false, code: 'UNREADABLE' }
  const second = attempt(alternate)
  if (second.ok)
    return {
      ok: true,
      value: second.value,
      degraded: {
        transport: quotaOf(alternate.call[1]),
        reason: `${quotaOf(primary.call[1])}-rate-limited`,
      },
    }
  return { ok: false, code: second.refused ? 'RATE_LIMITED' : 'UNREADABLE' }
}

/** Record only metadata, never GitHub payloads or credentials. */
export function recordDegraded(result, read, degraded) {
  if (!result.degraded) return
  const entry = { read, ...result.degraded }
  degraded.push(entry)
  process.stderr.write(
    `DEGRADED_READ read=${read} transport=${entry.transport} reason=${entry.reason}\n`,
  )
}

const rest = (endpoint, normalize, paginated = false) => ({
  call: ['gh', ['api', endpoint, ...(paginated ? ['--paginate', '--slurp'] : [])], { json: true }],
  normalize,
})
function graphql(repo, query, variables, normalize, paginated = false) {
  const [owner, name] = repo ? repo.split('/') : ['{owner}', '{repo}']
  return {
    call: [
      'gh',
      [
        'api',
        'graphql',
        '-f',
        `query=${query}`,
        repo ? '-f' : '-F',
        `owner=${owner}`,
        repo ? '-f' : '-F',
        `name=${name}`,
        ...Object.entries(variables).flatMap(([key, value]) => [
          typeof value === 'number' ? '-F' : '-f',
          `${key}=${value}`,
        ]),
        ...(paginated ? ['--paginate', '--slurp'] : []),
      ],
      { json: true },
    ],
    normalize,
  }
}

export function repoNameRest() {
  return rest('repos/{owner}/{repo}', (payload) => {
    if (typeof payload?.full_name !== 'string' || !payload.full_name)
      throw new Error('missing repo')
    return payload.full_name
  })
}

export function pullCoreRest(repo, pr) {
  return rest(`repos/${repo}/pulls/${pr}`, (p) => {
    if (
      !['open', 'closed'].includes(p?.state) ||
      typeof p.draft !== 'boolean' ||
      typeof p.head?.sha !== 'string'
    )
      throw new Error('missing pull request')
    return {
      state: p.merged === true ? 'MERGED' : p.state.toUpperCase(),
      isDraft: p.draft,
      headRefOid: p.head.sha,
    }
  })
}

export function draftTransitionsRest(repo, pr) {
  return rest(
    `repos/${repo}/issues/${pr}/timeline?per_page=100`,
    (pages) => {
      if (!Array.isArray(pages) || pages.some((page) => !Array.isArray(page)))
        throw new Error('missing timeline')
      return pages
        .flat()
        .filter((event) => ['ready_for_review', 'convert_to_draft'].includes(event?.event))
        .slice(-2)
        .map((event) => ({
          __typename:
            event.event === 'ready_for_review' ? 'ReadyForReviewEvent' : 'ConvertToDraftEvent',
          createdAt: event.created_at,
        }))
    },
    true,
  )
}

const COMMENTS_QUERY = `query($owner:String!,$name:String!,$number:Int!,$endCursor:String){
  repository(owner:$owner,name:$name){pullRequest(number:$number){comments(first:100,after:$endCursor){
    nodes{databaseId body createdAt updatedAt authorAssociation author{__typename login}} pageInfo{hasNextPage endCursor}
  }}}
}`
export function issueCommentsGraphql(repo, pr) {
  return graphql(
    repo,
    COMMENTS_QUERY,
    { number: pr },
    (pages) => {
      if (!Array.isArray(pages) || pages.length === 0) throw new Error('missing comment pages')
      return pages.map((page) => {
        const nodes = graphqlValue(page).pullRequest?.comments?.nodes
        if (!Array.isArray(nodes)) throw new Error('missing comments')
        return nodes.map((node) => {
          const author = node.author
          if (author !== null && (typeof author?.login !== 'string' || !author.login))
            throw new Error('missing author')
          const login =
            author === null
              ? 'ghost'
              : author.__typename === 'Bot' && !author.login.endsWith('[bot]')
                ? `${author.login}[bot]`
                : author.login
          return {
            id: node.databaseId,
            body: node.body,
            created_at: node.createdAt,
            updated_at: node.updatedAt,
            author_association: node.authorAssociation,
            user: { login },
          }
        })
      })
    },
    true,
  )
}

const PERMISSION_QUERY = `query($owner:String!,$name:String!,$login:String!,$endCursor:String){
  repository(owner:$owner,name:$name){collaborators(first:100,query:$login,after:$endCursor){
    edges{permission node{login}} pageInfo{hasNextPage endCursor}
  }}
}`
export function collaboratorPermissionGraphql(repo, login) {
  return graphql(
    repo,
    PERMISSION_QUERY,
    { login },
    (payload) => {
      const pages = Array.isArray(payload) ? payload : [payload]
      if (pages.length === 0) throw new Error('missing permission pages')
      const edges = pages.flatMap((page) => {
        const list = graphqlValue(page).collaborators?.edges
        if (
          !Array.isArray(list) ||
          list.some(
            (edge) => typeof edge?.node?.login !== 'string' || typeof edge.permission !== 'string',
          )
        )
          throw new Error('missing collaborators')
        return list
      })
      const edge = edges.find(
        (candidate) => candidate.node.login.toLowerCase() === login.toLowerCase(),
      )
      return Boolean(edge && ['ADMIN', 'MAINTAIN', 'WRITE'].includes(edge.permission))
    },
    true,
  )
}

const STATUSES_QUERY = `query($owner:String!,$name:String!,$sha:GitObjectID!){
  repository(owner:$owner,name:$name){object(oid:$sha){... on Commit{status{contexts{context state description targetUrl createdAt}}}}}
}`
export function commitStatusesGraphql(repo, sha) {
  return graphql(repo, STATUSES_QUERY, { sha }, (payload) => {
    const object = graphqlValue(payload).object
    if (!object || !Object.hasOwn(object, 'status')) throw new Error('missing commit')
    if (object.status === null) return []
    const contexts = object.status?.contexts
    if (
      !Array.isArray(contexts) ||
      contexts.some(
        (c) =>
          typeof c?.context !== 'string' ||
          !['SUCCESS', 'FAILURE', 'ERROR', 'PENDING', 'EXPECTED'].includes(c.state),
      )
    )
      throw new Error('missing statuses')
    return contexts
      .filter((c) => c.state !== 'EXPECTED')
      .map((c) => ({
        context: c.context,
        state: c.state.toLowerCase(),
        description: c.description,
        target_url: c.targetUrl,
        updated_at: c.createdAt,
      }))
  })
}

const text = (value) => typeof value === 'string' && value.trim() !== ''
const apiRepo = (repo) => (text(repo) ? `repos/${repo}` : 'repos/{owner}/{repo}')
const isGhNotFound = (r) =>
  !r.ok && (r.payload?.status === '404' || r.payload?.message === 'Not Found')

export function repoFor(read, repo, degraded = []) {
  if (text(repo)) return repo
  const view = readAcrossQuotas(
    read,
    {
      call: ['gh', ['repo', 'view', '--json', 'nameWithOwner'], { json: true }],
      normalize: (p) => p?.nameWithOwner,
    },
    repoNameRest(),
  )
  recordDegraded(view, 'repo-name', degraded)
  return view.ok && text(view.value) ? view.value : ''
}

// The repository permissions whose holder's markers count: a decision on a park is as strong as
// a merge, so read-only collaborators and org members without access to this repo do not count.
const OPERATOR_PERMISSIONS = new Set(['admin', 'write'])

// Can `login` write to the repository? true or false, or null when the read failed. A 404 means
// the account is not a collaborator at all.
export function canWrite(read, repo, login, degraded = []) {
  const call = [
    'gh',
    ['api', `${apiRepo(repo)}/collaborators/${encodeURIComponent(login)}/permission`],
    { json: true },
  ]
  const result = readAcrossQuotas(
    (...args) => {
      const r = read(...args)
      return isGhNotFound(r) ? { ok: true, payload: { permission: 'none' } } : r
    },
    {
      call,
      normalize: (p) => {
        if (typeof p?.permission !== 'string') throw new Error('missing permission')
        return OPERATOR_PERMISSIONS.has(p.permission)
      },
    },
    collaboratorPermissionGraphql(repo, login),
  )
  recordDegraded(result, 'collaborator-permission', degraded)
  return result.ok ? result.value : null
}
