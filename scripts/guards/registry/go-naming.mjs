export default {
  id: 'go-naming',
  rule: 'Go names use standard initialisms, consistent receivers, no package-name stutter and no underscored package names (staticcheck ST1003/ST1016, revive exported).',
  command: 'node scripts/lint/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['.golangci.yml', '**/*.go'],
  baseline: null,
  proof: {
    file: 'scripts/guards/go-naming.test.mjs',
    test: 'golangci reports a seeded UserId, mixed receiver name and stutter type',
  },
}
