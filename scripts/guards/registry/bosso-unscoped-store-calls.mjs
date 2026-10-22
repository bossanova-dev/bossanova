export default {
  id: 'bosso-unscoped-store-calls',
  rule: 'bosso handlers call `…Unscoped` store methods only from allowlisted functions.',
  command:
    "go -C services/bosso test ./api/orchestrator/... -run '^TestHandlersDoNotCallUnscopedStoreMethods$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/bosso/api/orchestrator/**/*.go', 'services/bosso/internal/server/*.go'],
  baseline: null,
  proof: {
    file: 'services/bosso/api/orchestrator/unscoped_store_calls_test.go',
    test: 'TestUnscopedStoreCallScanReportsASeededUnallowlistedCall',
  },
}
