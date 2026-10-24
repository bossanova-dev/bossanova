export default {
  id: 'agent-runner-conformance',
  rule: 'Every AgentRunnerService plugin passes the shared conformance suite and returns Unimplemented for exactly the RPCs it declares unsupported.',
  command:
    "go test -count=1 -run 'TestAgentRunnerConformance|TestCheck' ./lib/bossalib/contract/plugin/plugin/agentrunnertest/... ./plugins/bossd-plugin-claude/... ./plugins/bossd-plugin-codex/... ./plugins/bossd-plugin-opencode/... ./plugins/bossd-plugin-stub-runner/...",
  wiring: { make: 'test', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: [
    'lib/bossalib/contract/plugin/plugin/agentrunnertest/**',
    'plugins/bossd-plugin-claude/**',
    'plugins/bossd-plugin-codex/**',
    'plugins/bossd-plugin-opencode/**',
    'plugins/bossd-plugin-stub-runner/**',
  ],
  baseline: null,
  proof: {
    file: 'lib/bossalib/contract/plugin/plugin/agentrunnertest/agentrunnertest_test.go',
    test: 'TestCheckFailsOnSeededViolations',
  },
}
