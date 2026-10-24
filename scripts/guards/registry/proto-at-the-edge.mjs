export default {
  boundary: true,
  id: 'proto-at-the-edge',
  rule: 'Bosso and bossd protobuf imports stay at the API edge; legacy file exceptions only shrink.',
  command: 'node scripts/guards/proto-at-the-edge.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/bosso/**/*.go', 'services/bossd/**/*.go', '.golangci.yml'],
  baseline: 'scripts/guards/proto-at-the-edge-baseline.json',
  proof: {
    file: 'scripts/guards/proto-at-the-edge.test.mjs',
    test: 'proto-at-the-edge real depguard rejects seeded protobuf imports and accepts the repair',
  },
}
