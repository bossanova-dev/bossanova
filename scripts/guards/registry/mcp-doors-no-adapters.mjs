export default {
  id: 'mcp-doors-no-adapters',
  rule: 'The MCP doors declare consumer interfaces and never import adapters; cmd wires them.',
  command: 'node scripts/lint-affected.mjs',
  wiring: { make: 'lint', ci: { workflow: 'bazel.yml', job: 'go-lint' } },
  scans: ['services/mcp/api/**/*.go', 'services/mcp-gateway/api/**/*.go', '.golangci.yml'],
  baseline: null,
  proof: {
    file: 'scripts/guards/golangci/rules.test.mjs',
    test: 'depguard mcp-doors-no-adapters rejects adapter imports from both MCP doors',
  },
}
