export default {
  id: 'glossary-fresh',
  rule: 'The entity glossary is generated from the canonical entity vocabulary.',
  command: 'node scripts/guards/glossary.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['docs/structure/entities.yaml', 'docs/structure/glossary.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/glossary.test.mjs',
    test: '--check fails on a hand-edited glossary.md',
  },
}
