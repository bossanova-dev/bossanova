export default {
  id: 'guidance-audit',
  rule: 'No new test pins the wording of agent guidance; the pins in `scripts/guidance-audit-baseline.json` only shrink.',
  command:
    'node skills-toolbox/guidance-audit.mjs --gate --baseline scripts/guidance-audit-baseline.json',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['**/*.test.mjs', '**/*_test.go', 'scripts/guidance-audit-baseline.json'],
  baseline: null,
  proof: {
    file: 'skills-toolbox/guidance-audit.test.mjs',
    test: 'gate compares counts, not line positions, and removed pins are stale',
  },
}
