// The review request a standing park carries: GitHub's own "needs you" signal beside a green
// `needs human:` status. Cosmetic, like the park note: it never changes the verdict.

const LOGIN = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?$/
const TEAM = /^[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?\/[A-Za-z0-9._-]+$/

const apiRepo = (repo) => (repo ? `repos/${repo}` : 'repos/{owner}/{repo}')
const lower = (value) => String(value ?? '').toLowerCase()

/**
 * The reviewer list `post --reviewers` takes: comma-separated logins or `org/team` slugs. Null when
 * an entry is neither, so a typo is refused rather than silently asking nobody.
 * @param {string} value
 * @returns {string[] | null}
 */
export function parseReviewerList(value) {
  const entries = String(value ?? '')
    .split(',')
    .map((entry) => entry.trim())
    .filter(Boolean)
  return entries.every((entry) => LOGIN.test(entry) || TEAM.test(entry)) ? entries : null
}

/** The `--reviewers <csv>` argv for a judge output's `reviewers`, or none. */
export const reviewerArgs = (reviewers) =>
  Array.isArray(reviewers) && reviewers.length > 0 ? ['--reviewers', reviewers.join(',')] : []

// Who already stands asked: requested users and teams (by login, name or slug) and assignees.
function alreadyAsked(p) {
  const asked = new Set()
  for (const r of Array.isArray(p?.reviewRequests) ? p.reviewRequests : [])
    for (const key of [r?.login, r?.name, r?.slug])
      if (key) asked.add(lower(String(key).split('/').at(-1)))
  const assigned = new Set(
    (Array.isArray(p?.assignees) ? p.assignees : []).map((a) => lower(a?.login)),
  )
  return { asked, assigned }
}

/**
 * Asks for review of a standing park: `reviewers` when configured, else the repo owner when the
 * owner is a user. The PR author is dropped (GitHub refuses that request), as is anyone already
 * asked; an author-only candidate gets the PR assigned instead. One of `requested`,
 * `already-requested`, `assigned-author`, `already-assigned`, `skipped:<why>`, `failed` or
 * `planned` (dry run).
 * @param {(cmd: string, args: string[], opts?: {json?: boolean}) => {ok: boolean, payload?: any}} read
 * @param {{repo: string, pr: number, p: any, reviewers?: string[] | null, dryRun?: boolean, writes?: object[]}} args
 * @returns {string}
 */
export function requestParkReview(
  read,
  { repo, pr, p, reviewers = null, dryRun = false, writes = [] },
) {
  let candidates = Array.isArray(reviewers) ? reviewers : []
  if (candidates.length === 0) {
    const owner = read('gh', ['api', apiRepo(repo)], { json: true })
    if (!owner.ok || typeof owner.payload?.owner?.login !== 'string') return 'failed'
    if (owner.payload.owner.type !== 'User') return 'skipped:no-reviewer'
    candidates = [owner.payload.owner.login]
  }
  const author = lower(p?.author?.login)
  const { asked, assigned } = alreadyAsked(p)
  const users = candidates.filter((c) => !c.includes('/'))
  const teams = candidates.filter((c) => c.includes('/')).map((c) => c.split('/')[1])
  const others = users.filter((u) => lower(u) !== author)
  if (others.length === 0 && teams.length === 0) {
    if (!author) return 'skipped:no-reviewer'
    if (assigned.has(author)) return 'already-assigned'
    writes.push({ kind: 'assignee', assignees: [p.author.login] })
    if (dryRun) return 'planned'
    const r = read('gh', [
      'api',
      '--method',
      'POST',
      `${apiRepo(repo)}/issues/${pr}/assignees`,
      '-f',
      `assignees[]=${p.author.login}`,
    ])
    return r.ok ? 'assigned-author' : 'failed'
  }
  const wantUsers = others.filter((u) => !asked.has(lower(u)))
  const wantTeams = teams.filter((t) => !asked.has(lower(t)))
  if (wantUsers.length === 0 && wantTeams.length === 0) return 'already-requested'
  writes.push({ kind: 'review-request', reviewers: wantUsers, teamReviewers: wantTeams })
  if (dryRun) return 'planned'
  const fields = [
    ...wantUsers.flatMap((u) => ['-f', `reviewers[]=${u}`]),
    ...wantTeams.flatMap((t) => ['-f', `team_reviewers[]=${t}`]),
  ]
  const r = read('gh', [
    'api',
    '--method',
    'POST',
    `${apiRepo(repo)}/pulls/${pr}/requested_reviewers`,
    ...fields,
  ])
  return r.ok ? 'requested' : 'failed'
}
