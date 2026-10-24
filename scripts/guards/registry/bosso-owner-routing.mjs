export default {
  boundary: true,
  id: 'bosso-owner-routing',
  rule: "bosso reaches a session's pod through owner routing, never a pod-local command bypass from an unallowed call site.",
  command:
    "go -C services/bosso test ./api/orchestrator/... -run '^TestOwnerRoutingArchitectureHasNoPodLocalCommandBypasses$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/bosso/api/orchestrator/**/*.go', 'services/bosso/internal/daemonproxy/*.go'],
  baseline: null,
  proof: {
    file: 'services/bosso/api/orchestrator/routing_architecture_test.go',
    test: 'TestOwnerRoutingScanReportsASeededSendCommandBypass',
  },
}
