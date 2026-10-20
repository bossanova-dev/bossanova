export default {
  id: 'docs-mcp-props',
  rule: 'Every docs `<CommandTabs mcp="…">` prop names a tool the bossmcp server registers.',
  command: 'node scripts/check-docs-mcp-props.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['services/docs/docs/**', 'lib/bossalib/bossmcp/**'],
  baseline: null,
  proof: {
    file: 'scripts/check-docs-mcp-props.test.mjs',
    test: 'checkDocsMcpProps names the file, line, and value of an unregistered tool',
  },
}
