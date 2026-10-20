export default {
  id: 'published-core-identity-leaks',
  rule: 'The shipped published-core skill payloads carry no Bossanova identity (`BOS-123`, `bossanova-*`, …).',
  command: "go -C lib/bossalib test ./skillpayload/ -run '^TestPublishedCoresAreProjectAgnostic$'",
  wiring: { make: 'test-bossalib', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: ['lib/bossalib/skillpayload/skills/**'],
  baseline: null,
  proof: {
    file: 'lib/bossalib/skillpayload/skills_manifest_test.go',
    test: 'TestIdentityLeakScanReportsASeededProjectMCPServer',
  },
}
