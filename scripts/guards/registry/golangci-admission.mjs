export default {
  id: 'golangci-admission',
  rule: 'Every repository `golangci-lint run` launches through the host admission helper.',
  command: 'node scripts/guards/check-golangci-admission.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*js', 'skills-toolbox/**/*js'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-golangci-admission.test.mjs',
    test: 'raw analysis spawns are flagged while version probes and the admission helper pass',
  },
}
