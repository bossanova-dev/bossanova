export default {
  id: 'guidance-audit',
  rule: 'No new test pins the wording of agent guidance or reads source as text; the pins in `scripts/guards/guidance-audit-baseline.json` only shrink.',
  command:
    'node skills-toolbox/retro/guidance-audit.mjs --gate --baseline scripts/guards/guidance-audit-baseline.json',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '**/*.test.*',
    '**/*.spec.ts',
    '**/*_test.go',
    'scripts/guards/guidance-audit-baseline.json',
  ],
  baseline: 'scripts/guards/guidance-audit-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'skills-toolbox/retro/guidance-audit.test.mjs',
    test: 'gate compares counts, not line positions, and removed pins are stale',
  },
}
