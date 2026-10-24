export default {
  id: 'docs-index-fresh',
  rule: 'The docs index in AGENTS.md is generated from tracked docs with single-line summaries.',
  command: 'node scripts/guards/docs-index.mjs --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['AGENTS.md', 'docs/**/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/docs-index.test.mjs',
    test: 'docs-index check fails after a hand edit in the generated region',
  },
}
