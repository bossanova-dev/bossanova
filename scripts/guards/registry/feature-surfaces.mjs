export default {
  id: 'feature-surfaces',
  rule: 'Every feature reaches the CLI and MCP through named commands and tools, or a reasoned exemption.',
  command: 'node scripts/guards/feature-surfaces.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'docs/features/*.md',
    'lib/bossalib/contract/plugin/skillpayload/skills/boss/references/**',
    'lib/bossalib/kit/bossmcp/**',
  ],
  baseline: 'scripts/guards/feature-surfaces-baseline.json',
  proof: {
    file: 'scripts/guards/feature-surfaces.test.mjs',
    test: 'web-only feature without CLI/MCP names or exemption fails with needs cli and needs mcp',
  },
}
