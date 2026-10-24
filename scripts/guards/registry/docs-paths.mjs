export default {
  id: 'docs-paths',
  rule: 'Repository paths cited in markdown resolve; existing missing citations only shrink.',
  command: 'node scripts/guards/docs-paths.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/**/*.md', '**/AGENTS.md'],
  baseline: 'scripts/guards/docs-paths-baseline.json',
  proof: {
    file: 'scripts/guards/docs-paths.test.mjs',
    test: 'docs-paths rejects broken citations with the registered command and passes once baselined',
  },
}
