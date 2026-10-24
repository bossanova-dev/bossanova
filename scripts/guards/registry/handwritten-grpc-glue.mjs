export default {
  id: 'handwritten-grpc-glue',
  rule: 'Plugin-facing gRPC descriptors and method paths come from generated bindings.',
  command: 'node scripts/guards/check-handwritten-grpc-glue.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['plugins/**/*.go', 'services/**/*.go', 'lib/**/*.go'],
  baseline: 'scripts/guards/handwritten-grpc-glue-baseline.json',
  proof: {
    file: 'scripts/guards/check-handwritten-grpc-glue.test.mjs',
    test: 'guard rejects seeded descriptors and method paths',
  },
}
