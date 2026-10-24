export default {
  id: 'boundaries-doc-fresh',
  rule: 'The boundary reference is generated from boundary guards and their current baseline counts.',
  command: 'node scripts/guards/boundaries-doc.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'scripts/guards/registry/*.mjs',
    'scripts/guards/*baseline.json',
    'docs/structure/boundaries.md',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/boundaries-doc.test.mjs',
    test: '--check fails on a hand-edited boundaries.md',
  },
}
