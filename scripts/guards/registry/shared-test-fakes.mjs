export default {
  id: 'shared-test-fakes',
  rule: 'A fake, stub or mock type name is declared in at most one test directory; baselined package counts only shrink.',
  command: 'node scripts/guards/check-shared-test-fakes.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*_test.go', 'scripts/guards/shared-test-fakes-baseline.json'],
  baseline: 'scripts/guards/shared-test-fakes-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'scripts/guards/check-shared-test-fakes.test.mjs',
    test: 'a seeded second declaration of a shared fake fails',
  },
}
