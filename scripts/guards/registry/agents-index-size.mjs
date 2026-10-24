export default {
  id: 'agents-index-size',
  rule: 'AGENTS.md stays within its explicit byte budget.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['AGENTS.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/file-size.test.mjs',
    test: 'agents-index-size fails seeded byte growth and accepts multibyte content at budget',
  },
}
