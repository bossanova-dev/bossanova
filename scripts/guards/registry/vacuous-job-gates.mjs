export default {
  id: 'vacuous-job-gates',
  rule: "A CI job output publishes a value a step computed, never a step's completion status.",
  command: 'node scripts/check-vacuous-job-gates.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.github/workflows/**'],
  baseline: null,
  proof: {
    file: 'scripts/check-vacuous-job-gates.test.mjs',
    test: 'reports a job output published from a step completion status',
  },
}
