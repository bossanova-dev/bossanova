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
export function readChangedFiles(read, pr, repo) {
  const r = read(
    'gh',
    ['api', `${apiRepo(repo)}/pulls/${pr}/files?per_page=100`, '--paginate', '--slurp'],
    { json: true },
  )
  if (!r.ok || !Array.isArray(r.payload) || !r.payload.every(Array.isArray)) return null
  const files = r.payload.flat()
  if (files.length >= PR_FILES_CAP) return null
  for (const file of files) {
    if (!plainObject(file) || !text(file.filename)) return null
  }
  return files
}

function changedFilePaths(files) {
  const paths = new Set()
  for (const file of files) {
    paths.add(file.filename)
    if (text(file.previous_filename)) paths.add(file.previous_filename)
  }
  return [...paths]
}

export function readChangedPaths(read, pr, repo) {
  const files = readChangedFiles(read, pr, repo)
  return files === null ? null : changedFilePaths(files)
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
 * described exactly `verified (approved)`, a trusted review, or a trusted operator approval record
 * waiving codes on an earlier head. The latest timestamp wins. Returns `{sha, via, at}`, or
 * `{sha: null, why: 'anchor-unreadable'|'no-anchor'}`: a malformed payload is unknown, never empty.
 *
 * @param {{errors?: unknown, data?: {repository?: {pullRequest?: any}}}} payload The GraphQL
 *   response, checked structurally here.
 * @param {{revoked?: (sha: string, at: string) => boolean, approvals?: Array<{head: string, at: number, waives: string}>, headSha?: string}} [options]
 */
export function resolveApprovalAnchor(
  payload,
  { revoked = () => false, approvals = [], headSha } = {},
) {
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
  for (const approval of approvals) {
    if (
      !isSha(approval?.head) ||
      !text(approval.waives) ||
      approval.head.toLowerCase() === String(headSha).toLowerCase()
    )
      continue
    const ms = typeof approval.at === 'number' ? approval.at : Date.parse(approval.at)
    if (Number.isFinite(ms))
      candidates.push({
        sha: approval.head,
        via: 'operator-approved',
        at: new Date(ms).toISOString(),
      })
  }
  // A candidate with no readable timestamp cannot be ordered, so it is dropped: fewer anchors only
  // ever widen the judged range. So is a head the operator sent back: its approval is revoked.
  let best = null
  for (const c of candidates) {
    if (revoked(c.sha.toLowerCase(), c.at)) continue
    const ms = Date.parse(c.at ?? '')
    if (Number.isFinite(ms) && (best === null || ms > best.ms)) best = { ...c, ms }
  }
  if (!best) return { sha: null, why: 'no-anchor' }
  return { sha: best.sha.toLowerCase(), via: best.via, at: best.at }
}

export function readApprovalAnchor(read, repo, pr, readRevocations, headSha) {
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
  // Unreadable trusted markers cannot establish whether an approval was revoked.
  const revocations = readRevocations()
  if (!revocations.readable) return { sha: null, why: 'comments-unreadable' }
  return resolveApprovalAnchor(r.payload, { ...revocations, headSha })
}

// ---------------------------------------------------------------------------
// The PR-level delta: what the PR's own changes gained since the anchor, each side diffed against
// its own merge-base with the base tip, so a rebase never imports base commits.

// The compare API lists at most this many files; a side at the cap may be truncated.
export const COMPARE_FILE_CAP = 300

// Trust and marker parsing belong to the gate; this adapter consumes its one filtered marker read.
export function approvalRangeMarkers(result, decisionFor) {
  if (!result.readable) return { readable: false }
  const markers = result.markers
  const approvals = new Map()
  for (const marker of markers) {
    if (marker.kind !== 'approve' || decisionFor(markers, marker.head).decision !== 'approved')
      continue
    const old = approvals.get(marker.head)
    if (!old || marker.at > old.at || (marker.at === old.at && marker.id > old.id))
      approvals.set(marker.head, marker)
  }
  return {
    readable: true,
    approvals: [...approvals.values()].map(({ head, at, waives }) => ({ head, at, waives })),
    revoked: (sha, at) =>
      decisionFor(markers, sha).decision === 'sent-back' ||
      markers.some(
        (m) =>
          m.head === sha &&
          m.kind === 'blockers' &&
          m.at > Date.parse(at) &&
          m.blockerFile.blockers.some((b) => b.kind === 'operator-note'),
      ),
  }
}

/** Is a commit local, fetching from origin once when needed? */
export function ensureCommit(git, sha) {
  const commit = `${sha}^{commit}`
  if (git('git', ['cat-file', '-e', commit]).ok) return true
  git('git', ['fetch', '--no-tags', '--quiet', 'origin', sha])
  return git('git', ['cat-file', '-e', commit]).ok
}

// Context and hunk positions are base content. Preserve only the PR's lines and their EOF markers.
function ownLines(patch) {
  const out = []
  let changed = false
  for (const line of patch.split('\n')) {
    const own = line.startsWith('+') || line.startsWith('-')
    if (own || (changed && line === '\\ No newline at end of file')) out.push(line)
    changed = own
  }
  return out.join('\n')
}

const fileChanged = (a, b, ownChange) => {
  if (a.status !== b.status || a.previous_filename !== b.previous_filename) return true
  if (typeof a.patch === 'string' && typeof b.patch === 'string')
    return ownLines(a.patch) !== ownLines(b.patch)
  if (a.patch === b.patch && text(a.sha) && a.sha === b.sha) return false
  if (typeof ownChange !== 'function') return true
  try {
    const before = ownChange(a, 'anchor')
    const after = ownChange(b, 'head')
    return typeof before !== 'string' || typeof after !== 'string' || before !== after
  } catch {
    return true
  }
}

// Mirrors base...side from the compare API, including on a force-pushed rebase.
function localOwnChange(read, baseSha, sides) {
  const mergeBases = new Map()
  return (file, side) => {
    const sha = sides[side]
    if (!mergeBases.has(sha)) {
      if (!ensureCommit(read, baseSha) || !ensureCommit(read, sha)) {
        mergeBases.set(sha, null)
      } else {
        const r = read('git', ['merge-base', baseSha, sha])
        mergeBases.set(sha, r.ok && isSha(r.output?.trim()) ? r.output.trim() : null)
      }
    }
    const base = mergeBases.get(sha)
    if (!base) return null
    const paths = [...new Set([file.previous_filename, file.filename].filter(text))]
    const r = read('git', [
      'diff',
      '-U0',
      '--no-color',
      '--no-ext-diff',
      '--no-textconv',
      '--no-renames',
      base,
      sha,
      '--',
      ...paths.map((name) => `:(literal)${name}`),
    ])
    if (!r.ok || typeof r.output !== 'string' || /^Binary files |^GIT binary patch/m.test(r.output))
      return null
    // File headers can themselves start with +/-; project only lines inside actual hunks.
    const hunks = []
    let inHunk = false
    for (const line of r.output.split('\n')) {
      if (line.startsWith('diff --git ')) inHunk = false
      if (line.startsWith('@@ ')) inHunk = true
      if (inHunk) hunks.push(line)
    }
    return ownLines(hunks.join('\n'))
  }
}

/**
 * The paths whose PR-level change differs between two compare `files` lists (`base...anchor` and
 * `base...head`): on one side only, a different status or rename source, or different own lines
 * (locally resolved when a patch is missing). A renamed entry in the delta also names its old
 * path. Returns a sorted, deduped path list.
 * @param {object[]} filesAtAnchor
 * @param {object[]} filesAtHead
 * @param {{ownChange?: (file: object, side: 'anchor'|'head') => string|null}} [options]
 */
export function prLevelDelta(filesAtAnchor, filesAtHead, { ownChange } = {}) {
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
    if (!anchor || fileChanged(anchor, head, ownChange)) {
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
  if (!r.payload.files.every((file) => plainObject(file) && text(file.filename)))
    return { files: null, why: 'compare-unreadable' }
  return { files: r.payload.files }
}

/**
 * Chooses the range the path codes and the extension envelope are judged on. With an anchor A and
 * both compares readable it is the delta since A (empty when A is the head); otherwise the full
 * `base..head` diff with `why` naming the fallback. `{paths, range: {kind, anchorSha, why?}}`, where
 * `paths` is null when even the full diff is unreadable.
 */
export function selectChangedRange(read, { repo, pr, baseSha, headSha, readRevocations }) {
  const full = (anchorSha, why) => {
    const files = readChangedFiles(read, pr, repo)
    return {
      files,
      paths: files === null ? null : changedFilePaths(files),
      range: { kind: 'full', anchorSha, why },
    }
  }
  if (!isSha(baseSha)) return full(null, 'no-base-sha')
  const anchor = readApprovalAnchor(read, repo, pr, readRevocations, headSha)
  if (!anchor.sha) return full(null, anchor.why)
  const delta = { kind: 'delta', anchorSha: anchor.sha }
  if (anchor.sha === String(headSha).toLowerCase()) return { paths: [], files: [], range: delta }
  const atAnchor = readCompareFiles(read, repo, baseSha, anchor.sha)
  if (!atAnchor.files) return full(anchor.sha, atAnchor.why)
  const atHead = readCompareFiles(read, repo, baseSha, headSha)
  if (!atHead.files) return full(anchor.sha, atHead.why)
  const paths = prLevelDelta(atAnchor.files, atHead.files, {
    ownChange: localOwnChange(read, baseSha, { anchor: anchor.sha, head: headSha }),
  })
  if (paths.length === 0) return { paths, files: [], range: delta }
  const sinceAnchor = readCompareFiles(read, repo, anchor.sha, headSha)
  if (!sinceAnchor.files) return full(anchor.sha, sinceAnchor.why)
  const deltaPaths = new Set(paths)
  // Use unapproved patches, but only for the PR-level paths: the anchor compare can also
  // contain base-branch edits imported by a rebase.
  const files = sinceAnchor.files.filter(
    (file) => deltaPaths.has(file.filename) || deltaPaths.has(file.previous_filename),
  )
  // A rewritten head can omit reverted paths from the anchor compare. Retain their earlier
  // metadata so an incomplete compare cannot silently remove a review area.
  const represented = new Set(changedFilePaths(files))
  for (const file of [...atHead.files, ...atAnchor.files]) {
    const names = changedFilePaths([file])
    if (names.some((name) => deltaPaths.has(name) && !represented.has(name))) {
      files.push(file)
      for (const name of names) represented.add(name)
    }
  }
  return { paths, files, range: delta }
}

// Resolve the last observed CI context set from the PR timeline.
export function resolvePriorHead(payload, headSha) {
  const unknown = { known: false }
  if (!plainObject(payload) || payload.errors !== undefined) return unknown
  const nodes = payload.data?.repository?.pullRequest?.timelineItems?.nodes
  if (!Array.isArray(nodes)) return unknown
  for (const node of [...nodes].reverse()) {
    const commit = node?.commit ?? node?.beforeCommit
    if (!isSha(commit?.oid)) return unknown
    if (commit.oid.toLowerCase() === headSha.toLowerCase()) continue
    if (commit.statusCheckRollup === null) continue
    const connection = commit.statusCheckRollup?.contexts
    if (
      !plainObject(connection) ||
      connection.pageInfo?.hasNextPage !== false ||
      !Array.isArray(connection.nodes)
    )
      return unknown
    const contexts = []
    for (const context of connection.nodes) {
      const name = context?.name ?? context?.context
      if (typeof name !== 'string' || !name.trim()) return unknown
      if (name.startsWith('boss/')) continue
      const workflow = context.checkSuite?.workflowRun?.workflow
      contexts.push({
        ...context,
        ...(workflow?.name ? { workflowName: workflow.name } : {}),
        ...(Number.isInteger(workflow?.databaseId) ? { workflowId: workflow.databaseId } : {}),
      })
    }
    if (contexts.length) return { known: true, sha: commit.oid.toLowerCase(), contexts }
  }
  return { known: true, sha: null }
}
