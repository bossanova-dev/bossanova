export default {
  boundary: true,
  id: 'bosso-tenant-access-compile',
  rule: 'The cloud-session and organization-note stores require decided tenant access or a reasoned system grant; raw strings and variable system reasons fail to compile.',
  command: 'node scripts/guards/bosso-tenant-access-compile.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/bosso/internal/**/*.go', 'services/bosso/api/**/*.go'],
  baseline: null,
  proof: {
    file: 'scripts/guards/bosso-tenant-access-compile.test.mjs',
    test: 'tenant access compile guard rejects a seeded permissive fixture',
  },
}
