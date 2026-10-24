export default {
  id: 'rules-table-fresh',
  rule: 'The rules tables in AGENTS.md and docs/philosophy/rules.md are generated from the registry and are never edited by hand.',
  command: 'node scripts/guards/rules-table.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['AGENTS.md', 'docs/philosophy/rules.md', 'scripts/guards/registry/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/rules-table.test.mjs',
    test: '--check fails on a hand-edited row in the region',
  },
}
