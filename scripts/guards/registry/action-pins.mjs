export default {
  id: 'action-pins',
  rule: 'Every non-local Actions `uses:` is SHA-pinned, wrapped actions use their composite, replaced actions stay unused, and workflows grant no top-level write permissions.',
  command: 'node --test scripts/guards/check-action-pins.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '.github/workflows/**',
    '.github/actions/**',
    '.github/dependabot.yml',
    'scripts/guards/action-pins-baseline.json',
  ],
  baseline: 'scripts/guards/action-pins-baseline.json',
  proof: {
    file: 'scripts/guards/check-action-pins.test.mjs',
    test: 'an action pinned by tag fails',
  },
}
