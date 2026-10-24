export default {
  id: 'bossd-detachgate',
  rule: "bossd's rotation, server, session and upstream packages make no raw singleflight `DoChan` call.",
  command:
    "go -C services/bossd test ./internal/rotation/ ./api/daemon/ ./internal/session/ ./api/upstream/ -run '^TestNoRawDoChanCallInThisPackage$'",
  wiring: { make: 'test-bossd', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: [
    'services/bossd/internal/rotation/*.go',
    'services/bossd/api/daemon/*.go',
    'services/bossd/internal/session/*.go',
    'services/bossd/api/upstream/*.go',
  ],
  baseline: null,
  proof: {
    file: 'services/bossd/internal/detach/detachgate/detachgate_test.go',
    test: 'TestScanReportsARawDoChanCallWithFileLineAndFunction',
  },
}
