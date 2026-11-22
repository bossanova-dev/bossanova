export default {
  id: 'web-css-scales',
  rule: 'Web CSS scales come from app.css tokens.',
  command: 'node scripts/guards/check-web-css-scales.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/web/src/**/*.css', 'scripts/guards/web-css-scales-baseline.json'],
  baseline: 'scripts/guards/web-css-scales-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'scripts/guards/check-web-css-scales.test.mjs',
    test: 'a hard-coded radius, layer or duration in a web stylesheet fails the ratchet',
  },
}
