export default {
  id: 'bosso-entitlement-declared',
  rule: 'Every `OrchestratorService` method declares an entitlement.',
  command:
    "go -C services/bosso test ./api/orchestrator/ -run '^TestEntitlementDeclaredForEveryOrchestratorMethod$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: [
    'services/bosso/api/orchestrator/entitlement*.go',
    'services/bosso/internal/domain/authzlevel/authzlevel.go',
    'proto/bossanova/v1/*.proto',
  ],
  baseline: null,
  proof: {
    file: 'services/bosso/api/orchestrator/entitlement_interceptor_test.go',
    test: 'TestEntitlementCompletenessReportsASeededUndeclaredMethod',
  },
}
