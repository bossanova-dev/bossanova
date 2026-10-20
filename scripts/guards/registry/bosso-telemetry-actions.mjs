export default {
  id: 'bosso-telemetry-actions',
  rule: 'Every `OrchestratorService` procedure has a telemetry action classification.',
  command:
    "go -C services/bosso test ./internal/server/ -run '^TestTelemetryActionsClassifiesEveryOrchestratorMethod$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/bosso/internal/server/telemetry_actions*.go', 'proto/bossanova/v1/*.proto'],
  baseline: null,
  proof: {
    file: 'services/bosso/internal/server/telemetry_actions_test.go',
    test: 'TestTelemetryClassificationReportsASeededUnclassifiedMethod',
  },
}
