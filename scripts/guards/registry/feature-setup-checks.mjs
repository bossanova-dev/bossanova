export default {
  id: 'feature-setup-checks',
  rule: 'Features declare setup checks.',
  command: 'node scripts/guards/feature-setup-checks.mjs --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['docs/features/*.md', 'services/bossd/internal/setupcheck/ids.json'],
  baseline: 'scripts/guards/feature-setup-checks-baseline.json',
  proof: {
    file: 'scripts/guards/feature-setup-checks.test.mjs',
    test: 'feature-setup-checks rejects a feature that declares neither setup checks nor an exemption',
  },
}
