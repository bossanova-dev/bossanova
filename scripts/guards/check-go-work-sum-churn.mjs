// guard-id: go-work-sum-churn
import { spawnSync } from 'node:child_process'
import path from 'node:path'
import { parseArgs } from 'node:util'

import { isMainModule } from '../../skills-toolbox/main-module.mjs'

/** @param {string[]} changedPaths @returns {string[]} */
export function incidentalWorkSumPaths(changedPaths) {
  const sums = changedPaths.filter((file) => path.posix.basename(file) === 'go.work.sum')
  const hasModule = changedPaths.some((file) =>
    ['go.mod', 'go.sum', 'go.work'].includes(path.posix.basename(file)),
  )
  return !hasModule && sums.length < changedPaths.length ? sums : []
}

function main() {
  const { values } = parseArgs({
    options: {
      base: { type: 'string' },
      head: { type: 'string', default: 'HEAD' },
    },
    strict: true,
    allowPositionals: false,
  })
  const branch = process.env.GO_WORK_SUM_BASE_BRANCH || process.env.GITHUB_BASE_REF || 'main'
  const base = values.base || `origin/${branch}`
  const baseBranch = base.replace(/^refs\/(?:remotes\/origin|heads)\//, '').replace(/^origin\//, '')
  if (['staging', 'production'].includes(baseBranch)) {
    console.log(`go-work-sum-churn: skipped release base ${baseBranch}`)
    return
  }
  const resolved = spawnSync(
    'git',
    ['rev-parse', '--verify', '--end-of-options', `${base}^{commit}`],
    {
      encoding: 'utf8',
    },
  )
  if (resolved.status !== 0) {
    console.error(`go-work-sum-churn: cannot resolve base ${base}; use --base <commit-ref>`)
    process.exitCode = 2
    return
  }
  const diff = spawnSync('git', ['diff', '--name-only', '-z', `${base}...${values.head}`, '--'], {
    encoding: 'utf8',
  })
  if (diff.status !== 0) {
    console.error(`go-work-sum-churn: cannot read ${base}...${values.head}: ${diff.stderr.trim()}`)
    process.exitCode = 2
    return
  }
  const violations = incidentalWorkSumPaths(diff.stdout.split('\0').filter(Boolean))
  for (const file of violations) {
    console.error(
      `${file}: restore with git restore --source=${base}...${values.head} --staged --worktree -- ${file}, then amend or add a commit`,
    )
  }
  process.exitCode = violations.length > 0 ? 1 : 0
}

if (isMainModule(import.meta.url)) main()
