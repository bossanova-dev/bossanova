export default {
  id: 'job-timeouts',
  rule: 'Every workflow job declares `timeout-minutes`, and its step caps add up to less than the job cap.',
  command: 'node --test scripts/check-job-timeouts.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.github/workflows/**'],
  baseline: null,
  proof: {
    file: 'scripts/check-job-timeouts.test.mjs',
    test: 'a job without timeout-minutes is reported with its line',
  },
}
