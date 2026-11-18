export default {
  id: 'go-work-sum-churn',
  rule: 'go.work.sum changes only with a module file.',
  command: 'node scripts/guards/check-go-work-sum-churn.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: { workflow: 'test-scripts.yml', job: 'scripts' },
  },
  scans: ['go.work.sum'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-go-work-sum-churn.test.mjs',
    test: 'go-work-sum-churn rejects a branch carrying go.work.sum beside unrelated work (the b9e1b5e31d shape)',
  },
}
