import { VERIFY_CONTEXT, VERIFY_DESCRIPTIONS } from './verify-vocabulary.mjs'

const isSha = (value) => typeof value === 'string' && /^[0-9a-f]{40}$/i.test(value)
const text = (value) => typeof value === 'string' && value.trim() !== ''
const apiRepo = (repo) => (text(repo) ? `repos/${repo}` : 'repos/{owner}/{repo}')
const plainObject = (value) => value !== null && typeof value === 'object' && !Array.isArray(value)

// The pulls files API lists at most this many files; a list at the cap may be truncated.
export const PR_FILES_CAP = 3000

/**
 * The PR's full `base..head` changed paths, each renamed file under BOTH names: a rename moves a
 * path out of whatever matched it, so its source is as changed as its destination. `gh pr diff
 * --name-only` names only the destination, which let a rename out of a guarded path pass unseen.
 * Returns a deduped list in API order, or null when the read fails, is malformed, or hits the
 * file cap (a truncated list cannot prove a path unchanged).
 */
export function readChangedPaths(read, pr, repo) {
  const r = read(
    'gh',
    ['api', `${apiRepo(repo)}/pulls/${pr}/files?per_page=100`, '--paginate', '--slurp'],
    { json: true },
  )
  if (!r.ok || !Array.isArray(r.payload) || !r.payload.every(Array.isArray)) return null
  const files = r.payload.flat()
  if (files.length >= PR_FILES_CAP) return null
  const paths = new Set()
  for (const file of files) {
    if (!plainObject(file) || !text(file.filename)) return null
    paths.add(file.filename)
    if (text(file.previous_filename)) paths.add(file.previous_filename)
  }
  return [...paths]
}

// ---------------------------------------------------------------------------
// The approval anchor: the head a person last approved. Read only when something consumes a range.

const VERIFY_CONTEXT_FIELDS = `context(name: ${JSON.stringify(VERIFY_CONTEXT)}) { state description createdAt }`

// One read: the PR's reviews (every state, so a dismissed review is seen and ignored) and its last
// commits and force-pushes, each with its latest `boss/verify` status, so a head that was
// force-pushed away still anchors.
export const APPROVAL_ANCHOR_QUERY = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(last: 100) { nodes { state submittedAt authorAssociation commit { oid } } }
      timelineItems(last: 100, itemTypes: [PULL_REQUEST_COMMIT, HEAD_REF_FORCE_PUSHED_EVENT]) {
        nodes {
          ... on PullRequestCommit { commit { oid status { ${VERIFY_CONTEXT_FIELDS} } } }
          ... on HeadRefForcePushedEvent { beforeCommit { oid status { ${VERIFY_CONTEXT_FIELDS} } } }
        }
      }
    }
  }
}`

// Only a review from someone with write access can anchor: an outsider's approval must not narrow
// the judged range, and an outsider's approval on the head must not empty it.
export const ANCHOR_REVIEW_ASSOCIATIONS = new Set(['OWNER', 'MEMBER', 'COLLABORATOR'])

/**
 * Classifies the anchor read. Candidates are a commit whose latest `boss/verify` status is a success
 * described exactly `verified (approved)`, and the commit of an `APPROVED` review by an owner,
 * member or collaborator; the latest by
 * timestamp wins. Returns `{sha, via: 'verified-approved'|'review-approved', at}`, or
 * `{sha: null, why: 'anchor-unreadable'|'no-anchor'}`: a malformed payload is unknown, never empty.
 *
 * @param {{errors?: unknown, data?: {repository?: {pullRequest?: any}}}} payload The GraphQL
 *   response, checked structurally here.
 * @param {{revoked?: (sha: string) => boolean}} [options]
 */
export function resolveApprovalAnchor(payload, { revoked = () => false } = {}) {
  const unreadable = { sha: null, why: 'anchor-unreadable' }
  if (!plainObject(payload) || payload.errors !== undefined) return unreadable
  const pull = payload.data?.repository?.pullRequest
  if (!plainObject(pull)) return unreadable
  const reviews = pull.reviews?.nodes
  const timeline = pull.timelineItems?.nodes
  if (!Array.isArray(reviews) || !Array.isArray(timeline)) return unreadable
  const candidates = []
  for (const node of timeline) {
    const commit = plainObject(node) ? (node.commit ?? node.beforeCommit) : null
    const ctx = commit?.status?.context
    if (
      isSha(commit?.oid) &&
      plainObject(ctx) &&
      String(ctx.state).toUpperCase() === 'SUCCESS' &&
      ctx.description === VERIFY_DESCRIPTIONS.verifiedApproved
    )
      candidates.push({ sha: commit.oid, via: 'verified-approved', at: ctx.createdAt })
  }
  for (const review of reviews)
    if (
      plainObject(review) &&
      review.state === 'APPROVED' &&
      ANCHOR_REVIEW_ASSOCIATIONS.has(review.authorAssociation) &&
      isSha(review.commit?.oid)
    )
      candidates.push({ sha: review.commit.oid, via: 'review-approved', at: review.submittedAt })
  // A candidate with no readable timestamp cannot be ordered, so it is dropped: fewer anchors only
  // ever widen the judged range. So is a head the operator sent back: its approval is revoked.
  let best = null
  for (const c of candidates) {
    if (revoked(c.sha.toLowerCase())) continue
    const ms = Date.parse(c.at ?? '')
    if (Number.isFinite(ms) && (best === null || ms > best.ms)) best = { ...c, ms }
  }
  if (!best) return { sha: null, why: 'no-anchor' }
  return { sha: best.sha.toLowerCase(), via: best.via, at: best.at }
}

export function readApprovalAnchor(read, repo, pr, readRevocations) {
  if (typeof readRevocations !== 'function') throw new TypeError('readRevocations is required')
  const [owner, name] = text(repo) ? repo.split('/') : []
  const r = read(
    'gh',
    [
      'api',
      'graphql',
      '-f',
      `query=${APPROVAL_ANCHOR_QUERY}`,
      ...(owner && name
        ? ['-f', `owner=${owner}`, '-f', `name=${name}`]
        : ['-F', 'owner={owner}', '-F', 'name={repo}']),
      '-F',
      `number=${pr}`,
    ],
    { json: true },
  )
  if (!r.ok) return { sha: null, why: 'anchor-unreadable' }
  // A legacy send-back marker revokes every approval of its head, so a sent-back head never
  // anchors; when the markers cannot be read, no head does (the full range is judged).
  const revocations = readRevocations()
  if (!revocations.readable) return { sha: null, why: 'comments-unreadable' }
  return resolveApprovalAnchor(r.payload, { revoked: revocations.revoked })
}

// ---------------------------------------------------------------------------
// The PR-level delta: what the PR's own changes gained since the anchor, each side diffed against
// its own merge-base with the base tip, so a rebase never imports base commits.

// The compare API lists at most this many files; a side at the cap may be truncated.
export const COMPARE_FILE_CAP = 300

// A patch with its hunk-header line numbers removed: a rebase that only shifts a hunk is no change.
const normalizePatch = (patch) => patch.replace(/^@@ -\d+(?:,\d+)? \+\d+(?:,\d+)? @@/gm, '@@ @@')

const fileChanged = (a, b) => {
  if (a.status !== b.status || a.previous_filename !== b.previous_filename) return true
  if (typeof a.patch === 'string' && typeof b.patch === 'string')
    return normalizePatch(a.patch) !== normalizePatch(b.patch)
  return !text(a.sha) || !text(b.sha) || a.sha !== b.sha
}

/**
 * The paths whose PR-level change differs between two compare `files` lists (`base...anchor` and
 * `base...head`): on one side only, a different status or rename source, a different patch once
 * hunk headers are normalized, or, without a patch on both sides, a different blob. A renamed entry
 * in the delta also names its old path. Returns a sorted, deduped path list.
 */
export function prLevelDelta(filesAtAnchor, filesAtHead) {
  const byName = (files) =>
    new Map(
      (Array.isArray(files) ? files : [])
        .filter((f) => plainObject(f) && text(f.filename))
        .map((f) => [f.filename, f]),
    )
  const atAnchor = byName(filesAtAnchor)
  const atHead = byName(filesAtHead)
  const delta = new Set()
  const add = (f) => {
    delta.add(f.filename)
    if (text(f.previous_filename)) delta.add(f.previous_filename)
  }
  for (const [name, head] of atHead) {
    const anchor = atAnchor.get(name)
    if (!anchor || fileChanged(anchor, head)) {
      add(head)
      if (anchor) add(anchor)
    }
  }
  for (const [name, anchor] of atAnchor) if (!atHead.has(name)) add(anchor)
  return [...delta].sort()
}

// `{files}` for `base...sha`, or `{files: null, why}` when the read fails, is malformed, or hits the
// file cap (a truncated list cannot prove a path unchanged).
export function readCompareFiles(read, repo, baseSha, sha) {
  const r = read('gh', ['api', `${apiRepo(repo)}/compare/${baseSha}...${sha}`], { json: true })
  if (!r.ok || !Array.isArray(r.payload?.files)) return { files: null, why: 'compare-unreadable' }
  if (r.payload.files.length >= COMPARE_FILE_CAP) return { files: null, why: 'compare-file-cap' }
  return { files: r.payload.files }
}

/**
 * Chooses the range the path codes and the extension envelope are judged on. With an anchor A and
 * both compares readable it is the delta since A (empty when A is the head); otherwise the full
 * `base..head` diff with `why` naming the fallback. `{paths, range: {kind, anchorSha, why?}}`, where
 * `paths` is null when even the full diff is unreadable.
 */
export function selectChangedRange(read, { repo, pr, baseSha, headSha, readRevocations }) {
  const full = (anchorSha, why) => ({
    paths: readChangedPaths(read, pr, repo),
    range: { kind: 'full', anchorSha, why },
  })
  if (!isSha(baseSha)) return full(null, 'no-base-sha')
  const anchor = readApprovalAnchor(read, repo, pr, readRevocations)
  if (!anchor.sha) return full(null, anchor.why)
  const delta = { kind: 'delta', anchorSha: anchor.sha }
  if (anchor.sha === String(headSha).toLowerCase()) return { paths: [], range: delta }
  const atAnchor = readCompareFiles(read, repo, baseSha, anchor.sha)
  if (!atAnchor.files) return full(anchor.sha, atAnchor.why)
  const atHead = readCompareFiles(read, repo, baseSha, headSha)
  if (!atHead.files) return full(anchor.sha, atHead.why)
  return { paths: prLevelDelta(atAnchor.files, atHead.files), range: delta }
}
