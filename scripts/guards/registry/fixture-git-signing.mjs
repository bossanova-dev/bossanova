export default {
  id: 'fixture-git-signing',
  rule: 'Every test fixture that inits a git repo and commits disables commit signing.',
  command: 'node --test scripts/check-fixture-git-signing.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.test.mjs', 'skills-toolbox/**/*.test.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/check-fixture-git-signing.test.mjs',
    test: 'the detector discriminates protected from unprotected fixtures',
  },
}
