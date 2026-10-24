export default {
  id: 'action-pins',
  rule: 'Every non-local GitHub Actions `uses:` is pinned to a commit SHA, wrapped actions go through their composite, and no workflow grants write permissions at the top level.',
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
