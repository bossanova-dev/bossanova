export default {
  id: 'go-format',
  rule: 'Every tracked Go file outside `gen/` is gofmt- and goimports-clean.',
  command: 'node scripts/check-go-format.mjs',
  wiring: { make: 'lint-go-fmt', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['**/*.go'],
  baseline: null,
  proof: {
    file: 'scripts/check-go-format.test.mjs',
    test: 'checkFormat unions gofmt and goimports offenders, deduped and sorted',
  },
}
