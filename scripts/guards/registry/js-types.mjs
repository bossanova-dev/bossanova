export default {
  id: 'js-types',
  rule: 'Non-test scripts/ and skills-toolbox/ .mjs files type-check under tsc --checkJs; only baselined files may fail, and a clean baselined file must leave the baseline.',
  command: 'node scripts/guards/check-js-types.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.mjs', 'skills-toolbox/**/*.mjs', 'scripts/tsconfig.json'],
  baseline: 'scripts/guards/check-js-types-baseline.json',
  proof: {
    file: 'scripts/guards/check-js-types.test.mjs',
    test: 'checkJsTypes fails a seeded type error in a file outside the baseline',
  },
}
