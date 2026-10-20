export default {
  id: 'depguard-plugin-host-imports',
  rule: 'Plugin binaries under `plugins/bossd-plugin-*` never import a host boss, bossd, bosso or mcp package.',
  command: 'node scripts/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['plugins/bossd-plugin-*/**/*.go', '.golangci.yml'],
  baseline: null,
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard plugins-no-host-imports rejects a plugin importing a host bossd package',
  },
}
