export default {
  id: 'readme-assets',
  rule: 'Every local image and link target in README.md exists in the repository.',
  command: 'node scripts/guards/check-readme-assets.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['README.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-readme-assets.test.mjs',
    test: 'findMissingAssets reports targets that do not exist on disk',
  },
}
