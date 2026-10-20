export default {
  id: 'template-literal-comments',
  rule: 'No comment nested inside a JavaScript template literal contains a backtick.',
  command: 'node scripts/check-template-literal-comments.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.mjs', 'skills-toolbox/**/*.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/check-template-literal-comments.test.mjs',
    test: 'reports a backtick in a line comment inside a template literal',
  },
}
