export default {
  id: 'docs-layout',
  rule: 'Docs use allowlisted top-level directories and moved paths have no stale references.',
  command: 'node scripts/guards/docs-layout.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['**'],
  baseline: 'scripts/guards/docs-layout-baseline.json',
  proof: {
    file: 'scripts/guards/docs-layout.test.mjs',
    test: 'docs-layout rejects a seeded directory with the registered command',
  },
}
