export default {
  id: 'cli-flag-parsers',
  rule: 'New CLIs in `scripts/` and `skills-toolbox/` parse argv with `node:util` `parseArgs` (strict); the hand-rolled parsers in the baseline only shrink.',
  command: 'node scripts/check-cli-flag-parsers.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.mjs', 'skills-toolbox/**/*.mjs', 'scripts/cli-flag-parsers-baseline.json'],
  baseline: 'scripts/cli-flag-parsers-baseline.json',
  proof: {
    file: 'scripts/check-cli-flag-parsers.test.mjs',
    test: 'a new hand-rolled parser fails and names parseArgs as the fix',
  },
}
