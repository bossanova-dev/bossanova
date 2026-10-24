export default {
  boundary: true,
  id: 'depguard-plugin-boundary',
  rule: 'The depguard plugin deny list names every host service Go module under services/.',
  command: 'node --test scripts/guards/check-depguard-plugin-boundary.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/*/go.mod', '.golangci.yml'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-depguard-plugin-boundary.test.mjs',
    test: 'a host service module missing from the depguard deny list fails the gate',
  },
}
