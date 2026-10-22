// BOS-1783: replay the live resume-ledger rename; regenerate sqlc separately.
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { parseArgs } from 'node:util'

parseArgs({ strict: true, allowPositionals: false })
const names = {
  run_key: 'budget_key',
  RunKey: 'BudgetKey',
  runKey: 'budgetKey',
  ListForRun: 'ListForBudget',
  DeleteForRun: 'DeleteForBudget',
  RunResumeKey: 'ResumeBudgetKey',
}
const paths = execFileSync('git', [
  'ls-files',
  '-z',
  'services/bossd/internal',
  'lib/bossalib/models',
])
  .toString()
  .split('\0')
  .filter(
    (path) =>
      path.endsWith('.go') &&
      !path.includes('/sqlcgen/') &&
      !path.endsWith('/fix_row_state_migration_test.go') &&
      !path.endsWith('/resume_budget_key_migration_test.go'),
  )
for (const path of paths) {
  const before = readFileSync(path, 'utf8')
  const after = before
    .replace(
      /\b(run_key|RunKey|runKey|ListForRun|DeleteForRun|RunResumeKey)\b/g,
      (name) => names[name],
    )
    .replace(/KeepsOriginalRunKey/g, 'KeepsOriginalBudgetKey')
  if (after !== before) writeFileSync(path, after)
}
