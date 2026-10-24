export default {
  id: 'docs-absolute-selflinks',
  rule: 'A docs page links to another docs page relatively, never by an absolute docs-site URL the broken-link check cannot see.',
  command: 'node scripts/guards/check-docs-absolute-selflinks.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/docs/docs/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-docs-absolute-selflinks.test.mjs',
    test: 'checkDocsAbsoluteSelfLinks names the file, line, and URL of an offender',
  },
}
