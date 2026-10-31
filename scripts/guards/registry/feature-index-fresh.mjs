export default {
  id: 'feature-index-fresh',
  rule: 'The feature index is generated from feature frontmatter.',
  command: 'node scripts/guards/feature-map.mjs index --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/features/**/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/feature-map.test.mjs',
    test: 'feature index rejects a hand edit',
  },
}
