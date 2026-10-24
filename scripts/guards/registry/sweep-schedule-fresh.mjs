export default {
  id: 'sweep-schedule-fresh',
  rule: 'The sweep schedule page is generated from the cron declarations in `.boss-project.json`.',
  command: 'node scripts/guards/sweep-schedule.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.boss-project.json', 'docs/sweeps/schedule.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/sweep-schedule.test.mjs',
    test: '--check fails after adding a cron without regenerating',
  },
}
