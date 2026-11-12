// Where a PR merges, for the proof page's notice and the proof-author bundle: into a branch that is
// not the repo's default, or stacked on other open PRs. A commit's identity is its
// `git patch-id --stable`, which survives the rebases that change its SHA. A branch's own commits
// are those whose change the default branch lacks (`--cherry-pick`), so a copy of a parent merged
// under new SHAs drops out. Each one belongs to the oldest open PR that carries it, and the PR is stacked on the owners
// of every commit that is not its own. A PR whose base is another open PR's head branch is stacked
// on that PR too. Everything is read from the forge and git at call time: a stacked PR is often
// retargeted once its parent merges.
import { MAX_STACK } from './verify-proof-page.mjs'
import { MAX_TEXT_CHARS, capText } from './verify-proof.mjs'

/**
 * @typedef {import('./verify-proof-page.mjs').BranchContext} BranchContext
 * @typedef {import('./verify-proof-page.mjs').StackParent} StackParent
 * @typedef {{number: number, title: string, url: string, headRef: string, headSha: string,
 *   baseRef: string}} OpenPr
 * @typedef {(command: string, args: string[], opts?: {input?: string, json?: boolean,
 *   raw?: boolean}) => {ok: boolean, output?: string, payload?: any}} Read
 */

// The open PRs one read returns, newest first; an older parent than these goes unseen.
export const MAX_OPEN_PRS = 100
const TITLE_TAGS = /^(\s*\[[^\]]*\]\s*)+/

/**
 * The branch context from patch ids already read: `mine` for this PR, `carriers` for each older
 * open PR in ascending number order. Null for a PR into the default branch that carries no other
 * PR's commits.
 * @param {{me: OpenPr, defaultBranch: string, mine: Set<string>,
 *   carriers: {pr: OpenPr, ids: Set<string>}[]}} input
 * @returns {BranchContext | null}
 */
function branchContext({ me, defaultBranch, mine, carriers }) {
  /** @type {Map<number, number>} */
  const owned = new Map()
  for (const id of mine) {
    const owner = carriers.find(({ ids }) => ids.has(id))
    if (owner) owned.set(owner.pr.number, (owned.get(owner.pr.number) ?? 0) + 1)
  }
  const byBase = carriers.find(({ pr }) => pr.headRef === me.baseRef)?.pr
  if (byBase && !owned.has(byBase.number)) owned.set(byBase.number, 0)
  /** @type {StackParent[]} */
  const stackedOn = carriers
    .filter(({ pr }) => owned.has(pr.number))
    .slice(0, MAX_STACK)
    .map(({ pr, ids }) => ({
      pr: pr.number,
      title: capText(pr.title.replace(TITLE_TAGS, ''), MAX_TEXT_CHARS) || `#${pr.number}`,
      url: pr.url,
      branch: capText(pr.headRef, MAX_TEXT_CHARS),
      shared: owned.get(pr.number) ?? 0,
      total: ids.size,
      how: pr === byBase ? 'base' : 'content',
    }))
  if (!stackedOn.length && me.baseRef === defaultBranch) return null
  return { base: me.baseRef, default: defaultBranch, stackedOn }
}

/** A REST pull row as an OpenPr, or null when it lacks a field. @param {any} row @returns {OpenPr | null} */
function openPr(row) {
  const pr = {
    number: row?.number,
    title: row?.title,
    url: row?.html_url,
    headRef: row?.head?.ref,
    headSha: row?.head?.sha,
    baseRef: row?.base?.ref,
  }
  if (!Number.isSafeInteger(pr.number) || !/^[0-9a-f]{40}$/i.test(String(pr.headSha))) return null
  for (const key of /** @type {const} */ (['title', 'url', 'headRef', 'baseRef']))
    if (typeof pr[key] !== 'string' || !pr[key]) return null
  return /** @type {OpenPr} */ (pr)
}

/**
 * PR `pr`'s branch context in `repo`, read through `read` from the forge and the checkout it runs
 * in: the default branch, the newest MAX_OPEN_PRS open PRs, and the patch ids of each older one's
 * commits, fetching any head the checkout lacks. Undefined when the default branch, the PR or its
 * own commits cannot be read; an older PR whose commits cannot be read is left out. Never throws.
 * @param {{read: Read, repo: string, pr: number}} input
 * @returns {BranchContext | null | undefined}
 */
export function readBranchContext({ read, repo, pr }) {
  try {
    const defaultBranch = read('gh', ['api', `repos/${repo}`], { json: true }).payload
      ?.default_branch
    if (typeof defaultBranch !== 'string' || !defaultBranch) return undefined
    const list = read('gh', ['api', `repos/${repo}/pulls?state=open&per_page=${MAX_OPEN_PRS}`], {
      json: true,
    })
    if (!list.ok || !Array.isArray(list.payload)) return undefined
    const open = list.payload.map(openPr).filter((row) => row !== null)
    const me =
      open.find((row) => row.number === pr) ??
      openPr(read('gh', ['api', `repos/${repo}/pulls/${pr}`], { json: true }).payload)
    if (!me) return undefined
    const tip = /^[0-9a-f]{40}/i.exec(
      read('git', ['ls-remote', 'origin', `refs/heads/${defaultBranch}`]).output ?? '',
    )?.[0]
    if (!tip) return undefined
    const older = open.filter((row) => row.number < pr).sort((a, b) => a.number - b.number)
    const present = (/** @type {string} */ sha) =>
      read('git', ['cat-file', '-e', `${sha}^{commit}`]).ok
    const missing = [tip, me.headSha, ...older.map((row) => row.headSha)].filter(
      (sha) => !present(sha),
    )
    // One fetch for every missing head; a refused batch falls back to one fetch per head.
    if (missing.length && !read('git', ['fetch', '--no-tags', '--quiet', 'origin', ...missing]).ok)
      for (const sha of missing) read('git', ['fetch', '--no-tags', '--quiet', 'origin', sha])
    /** @param {string} sha @returns {Set<string> | null} */
    const patchIds = (sha) => {
      const log = read(
        'git',
        [
          'log',
          '-p',
          '--no-color',
          '--no-ext-diff',
          '--no-merges',
          '--cherry-pick',
          '--right-only',
          `${tip}...${sha}`,
        ],
        { raw: true },
      )
      if (!log.ok) return null
      const ids = read('git', ['patch-id', '--stable'], { input: log.output ?? '', raw: true })
      if (!ids.ok) return null
      return new Set(
        String(ids.output ?? '')
          .split('\n')
          .filter(Boolean)
          .map((line) => line.split(' ')[0]),
      )
    }
    const mine = patchIds(me.headSha)
    if (!mine) return undefined
    const carriers = older.flatMap((row) => {
      const ids = patchIds(row.headSha)
      return ids ? [{ pr: row, ids }] : []
    })
    return branchContext({ me, defaultBranch, mine, carriers })
  } catch {
    return undefined
  }
}
