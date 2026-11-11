export default {
  id: 'web-iframe-sandbox',
  rule: 'Web frames: JSX iframes only, sandbox at most allow-scripts, allow=""; none in injected HTML.',
  command: 'node scripts/guards/check-web-iframe-sandbox.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/web/src/**/*.ts', 'services/web/src/**/*.tsx'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-web-iframe-sandbox.test.mjs',
    test: 'an iframe with allow-same-origin fails naming the file and line',
  },
}
