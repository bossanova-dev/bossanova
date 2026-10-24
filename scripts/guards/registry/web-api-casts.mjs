export default {
  id: 'web-api-casts',
  rule: 'Web test API clients use the typed fakeApi builder instead of Api casts; the cast baseline only shrinks.',
  command: 'node scripts/guards/check-web-api-casts.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/web/src/**/*.test.ts',
    'services/web/src/**/*.test.tsx',
    'services/web/tests/**',
    'services/web/e2e/fakes/api.ts',
  ],
  baseline: 'scripts/guards/web-api-casts-baseline.json',
  proof: {
    file: 'scripts/guards/check-web-api-casts.test.mjs',
    test: 'seeded as Api fails naming the test file',
  },
}
