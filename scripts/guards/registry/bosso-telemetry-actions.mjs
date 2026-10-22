export default {
  id: 'bosso-telemetry-actions',
  rule: 'Every `OrchestratorService` procedure has a telemetry action classification.',
  command:
    "go -C services/bosso test ./api/orchestrator/... -run '^TestTelemetryActionsClassifiesEveryOrchestratorMethod$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/bosso/api/orchestrator/telemetry_actions*.go', 'proto/bossanova/v1/*.proto'],
  baseline: null,
  proof: {
    file: 'services/bosso/api/orchestrator/telemetry_actions_test.go',
    test: 'TestTelemetryClassificationReportsASeededUnclassifiedMethod',
  },
}
