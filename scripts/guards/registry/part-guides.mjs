export default {
  id: 'part-guides',
  rule: 'Every part guide is a regular AGENTS.md with a tracked CLAUDE.md symlink and a byte budget.',
  command: 'node scripts/guards/check-part-guides.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/AGENTS.md', '**/CLAUDE.md', 'scripts/guards/budgets.json'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-part-guides.test.mjs',
    test: 'part-guides rejects seeded byte growth and passes at budget',
  },
}
