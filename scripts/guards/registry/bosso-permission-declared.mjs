export default {
  id: 'bosso-permission-declared',
  rule: 'Every `OrchestratorService` method declares a permission.',
  command:
    "go -C services/bosso test ./api/orchestrator/ -run '^TestPermissionDeclaredForEveryOrchestratorMethod$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: [
    'services/bosso/api/orchestrator/permission*.go',
    'services/bosso/internal/domain/authzlevel/authzlevel.go',
    'services/bosso/internal/domain/permission/permission.go',
    'proto/bossanova/v1/*.proto',
  ],
  baseline: null,
  proof: {
    file: 'services/bosso/api/orchestrator/permission_interceptor_test.go',
    test: 'TestPermissionCompletenessReportsASeededUndeclaredMethod',
  },
}
