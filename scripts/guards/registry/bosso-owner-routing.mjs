export default {
  id: 'bosso-owner-routing',
  rule: "bosso reaches a session's pod through owner routing, never a pod-local command bypass from an unallowed call site.",
  command:
    "go -C services/bosso test ./internal/server/ -run '^TestOwnerRoutingArchitectureHasNoPodLocalCommandBypasses$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/bosso/internal/server/*.go'],
  baseline: null,
  proof: {
    file: 'services/bosso/internal/server/routing_architecture_test.go',
    test: 'TestOwnerRoutingScanReportsASeededSendCommandBypass',
  },
}
