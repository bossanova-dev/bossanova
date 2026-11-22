export default {
  id: 'web-css-colours',
  rule: 'Web CSS colours come from tokens.',
  command: 'node scripts/guards/check-web-css-colours.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/web/src/**/*.css', 'scripts/guards/web-css-colours-baseline.json'],
  baseline: 'scripts/guards/web-css-colours-baseline.json',
  baselineFormat: 'json',
  proof: {
    file: 'scripts/guards/check-web-css-colours.test.mjs',
    test: 'a new hard-coded colour in a web stylesheet fails the ratchet',
  },
}
