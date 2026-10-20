export default {
  id: 'boss-view-constants-listed',
  rule: 'Every boss TUI `View` constant is listed, in iota order, in the list the view routing guards iterate.',
  command: "go -C services/boss test ./internal/views/ -run '^TestAllViewConstantsAreListed$'",
  wiring: { make: 'test-boss', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/boss/internal/views/*.go'],
  baseline: null,
  proof: {
    file: 'services/boss/internal/views/app_guards_test.go',
    test: 'TestViewConstantListGuardReportsASeededUnlistedConstant',
  },
}
