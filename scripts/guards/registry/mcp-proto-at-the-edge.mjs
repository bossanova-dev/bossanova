export default {
  id: 'mcp-proto-at-the-edge',
  rule: 'MCP wire types stay in doors, cmd and adapters, outside internal features.',
  command: 'node scripts/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: [
    'services/mcp/internal/**/*.go',
    'services/mcp-gateway/internal/**/*.go',
    '.golangci.yml',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard mcp-proto-at-the-edge rejects wire imports from both MCP features',
  },
}
