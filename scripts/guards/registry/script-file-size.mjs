export default {
  id: 'script-file-size',
  rule: 'Non-test, non-generated scripts and toolbox modules obey a 1,000-line default and measured subject budgets; retired and vanished subjects leave the budget.',
  command: 'node scripts/guards/file-size.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.mjs', 'skills-toolbox/**/*.mjs'],
  baseline: null,
  proof: { file: 'scripts/guards/file-size.test.mjs', test: 'script-file-size rejects a new file' },
}
