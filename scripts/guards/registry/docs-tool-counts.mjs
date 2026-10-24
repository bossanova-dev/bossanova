export default {
  id: 'docs-tool-counts',
  rule: "Live docs that state the local MCP server's tool count state the count of the registered bossmcp tool set.",
  command: 'node scripts/guards/check-docs-tool-counts.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['README.md', 'services/docs/docs/guides/mcp.md', 'lib/bossalib/bossmcp/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-docs-tool-counts.test.mjs',
    test: 'checkDocsToolCounts fails stale claims with file and line',
  },
}
