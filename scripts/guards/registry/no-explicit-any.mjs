export default {
  id: 'no-explicit-any',
  rule: 'No `noExplicitAny` suppression comment or `as any` cast in the web and marketing TypeScript sources.',
  command: 'node scripts/guards/check-no-explicit-any.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/web/src/**', 'services/marketing/src/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-no-explicit-any.test.mjs',
    test: 'biome-ignore line naming noExplicitAny is detected',
  },
}
