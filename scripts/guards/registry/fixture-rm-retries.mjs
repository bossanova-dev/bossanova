export default {
  id: 'fixture-rm-retries',
  rule: 'Recursive `rm` calls in `*.test.mjs` fixtures pass `maxRetries`; the per-file counts in `scripts/fixture-rm-retries-baseline.json` only shrink.',
  command: 'node scripts/check-fixture-rm-retries.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*.test.mjs', 'scripts/fixture-rm-retries-baseline.json'],
  baseline: null,
  proof: {
    file: 'scripts/check-fixture-rm-retries.test.mjs',
    test: 'a new recursive rm without retries fails the ratchet',
  },
}
