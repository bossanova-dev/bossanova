export default {
  id: 'bosso-policy-imports',
  rule: 'The bosso `policy` package imports only the standard library, and none of its storage packages.',
  command:
    "go -C services/bosso test ./internal/domain/policy/ -run '^TestPolicyImportsOnlyPureStandardLibrary$'",
  wiring: { make: 'test-bosso', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['services/bosso/internal/domain/policy/*.go'],
  baseline: null,
  proof: {
    file: 'services/bosso/internal/domain/policy/imports_test.go',
    test: 'TestPolicyImportScanReportsASeededStorageImport',
  },
}
