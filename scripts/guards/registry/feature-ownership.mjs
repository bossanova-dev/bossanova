export default {
  boundary: true,
  id: 'feature-ownership',
  rule: 'Every non-ignored file has at most one feature owner; user-facing files and feature folders have an owner or an explicit shrinking baseline entry.',
  command: 'node scripts/guards/feature-map.mjs ownership --check',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: [
    'docs/features/**/*.md',
    'services/boss/api/**',
    'services/boss/cmd/**',
    'services/web/src/**',
    'services/bossd/api/daemon/**',
    'services/bossd/api/hooks/**',
    'services/bosso/api/orchestrator/**',
    'services/bosso/internal/daemonproxy/**',
    'services/mcp/**',
    'services/*/internal/**',
  ],
  baseline: 'scripts/guards/feature-map-baseline.json',
  proof: {
    file: 'scripts/guards/feature-map.test.mjs',
    test: 'feature ownership rejects an unowned file',
  },
}
