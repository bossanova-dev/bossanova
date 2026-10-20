export default {
  id: 'bossd-detachgate',
  rule: "bossd's rotation, server, session and upstream packages make no raw singleflight `DoChan` call.",
  command:
    "go -C services/bossd test ./internal/rotation/ ./internal/server/ ./internal/session/ ./internal/upstream/ -run '^TestNoRawDoChanCallInThisPackage$'",
  wiring: { make: 'test-bossd', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: [
    'services/bossd/internal/rotation/*.go',
    'services/bossd/internal/server/*.go',
    'services/bossd/internal/session/*.go',
    'services/bossd/internal/upstream/*.go',
  ],
  baseline: null,
  proof: {
    file: 'services/bossd/internal/detach/detachgate/detachgate_test.go',
    test: 'TestScanReportsARawDoChanCallWithFileLineAndFunction',
  },
}
