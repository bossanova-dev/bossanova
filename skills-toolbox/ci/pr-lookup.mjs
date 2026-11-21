import { repository } from './gh-repository.mjs'
import { FALLBACK_LABELS, readWithQuotaFallback } from './gh-quota-read.mjs'

const HEAD_PR_FIELDS = 'number,title,body,headRefName,state,url'
const fail = (message) => ({ exit: 1, out: { error: { code: 'UNREADABLE', message } } })

function restPullArgs(repo, suffix) {
  const parts = repo.split('/')
  const host = parts.length === 3 ? parts.shift() : null
  return ['api', `repos/${parts.join('/')}/pulls${suffix}`, ...(host ? ['--hostname', host] : [])]
}

function lookup(options, primary, alternate) {
  const result = readWithQuotaFallback({
    ...options,
    primary,
    alternate,
    label: FALLBACK_LABELS.PR_STATE,
  })
  return result.ok === true
    ? {
        exit: 0,
        out: result.value,
        metadata: { transport: result.transport, degraded: result.degraded },
      }
    : { exit: 1, out: { error: result.error } }
}

function normalizedLanded(value, rest) {
  const merged = rest ? value.merged : value.state === 'MERGED'
  const state = rest ? (merged ? 'MERGED' : value.state?.toUpperCase()) : value.state
  const url = rest ? value.html_url : value.url
  const mergeCommit = rest ? value.merge_commit_sha : value.mergeCommit?.oid
  if (
    !Number.isInteger(value.number) ||
    typeof url !== 'string' ||
    !['OPEN', 'CLOSED', 'MERGED'].includes(state) ||
    typeof merged !== 'boolean' ||
    (merged && typeof mergeCommit !== 'string')
  )
    throw new Error('malformed pull response')
  return { number: value.number, url, state, merged, mergeCommit: merged ? mergeCommit : null }
}

export function landedLookup({ pr, repo, timeoutMs, deps }) {
  let resolved
  try {
    resolved = repository(repo, deps, timeoutMs)
  } catch (error) {
    return fail(error.message)
  }
  const result = lookup(
    { timeoutMs, deps },
    {
      args: restPullArgs(resolved, `/${pr}`),
      normalize: (value) => normalizedLanded(value, true),
    },
    {
      args: [
        'pr',
        'view',
        String(pr),
        '--repo',
        resolved,
        '--json',
        'number,state,mergeCommit,url',
      ],
      normalize: (value) => normalizedLanded(value, false),
    },
  )
  return result.exit === 0 ? { exit: 0, out: { ...result.out, ...result.metadata } } : result
}

export function headPRLookup({ branch, repo = '', timeoutMs, deps }) {
  let resolved
  try {
    resolved = repository(repo, deps, timeoutMs)
  } catch (error) {
    return fail(error.message)
  }
  const normalize = (values, rest) => {
    if (!Array.isArray(values)) throw new Error('malformed pull list')
    return values.map((value) => {
      const item = rest
        ? {
            number: value.number,
            title: value.title ?? '',
            body: value.body ?? '',
            headRefName: value.head?.ref,
            state: value.state?.toUpperCase(),
            url: value.html_url,
          }
        : value
      if (!Number.isInteger(item.number) || typeof item.url !== 'string' || item.state !== 'OPEN')
        throw new Error('malformed pull list item')
      return item
    })
  }
  const primary = {
    args: [
      'pr',
      'list',
      '--head',
      branch,
      '--state',
      'open',
      '--json',
      HEAD_PR_FIELDS,
      '--repo',
      resolved,
    ],
    normalize: (value) => normalize(value, false),
  }
  const alternate = {
    args: [...restPullArgs(resolved, '?state=open&per_page=100'), '--paginate', '--slurp'],
    normalize: (pages) => {
      if (!Array.isArray(pages) || !pages.every(Array.isArray))
        throw new Error('malformed pull pages')
      // Match gh pr list --head by branch name, including PRs from contributor forks.
      return normalize(pages.flat(), true).filter((item) => item.headRefName === branch)
    },
  }
  try {
    return lookup({ timeoutMs, deps }, primary, alternate)
  } catch (error) {
    return fail(error.message)
  }
}
