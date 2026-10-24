export default {
  id: 'boss-view-constants-listed',
  rule: 'Every boss TUI `View` constant is listed, in iota order, in the list the view routing guards iterate.',
  command: "go -C services/boss test ./api/tui/ui/ -run '^TestAllViewConstantsAreListed$'",
  wiring: { make: 'test-boss', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/boss/api/tui/ui/*.go'],
  baseline: null,
  proof: {
    file: 'services/boss/api/tui/ui/nav_test.go',
    test: 'TestViewConstantListGuardReportsASeededUnlistedConstant',
  },
}
