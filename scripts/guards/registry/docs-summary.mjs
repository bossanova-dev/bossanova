export default {
  id: 'docs-summary',
  rule: 'Docs carry a non-empty single-line summary frontmatter scalar.',
  command: 'node scripts/guards/docs-summary.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/**/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/docs-summary.test.mjs',
    test: 'docs-summary rejects missing and block scalar summaries with the registered command',
  },
}
