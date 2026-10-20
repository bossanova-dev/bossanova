export default {
  id: 'skill-node-fences',
  rule: "Every node heredoc inside a skill's fenced shell blocks parses.",
  command: 'node scripts/check-skill-node-fences.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.claude/skills/**/*.md', 'lib/bossalib/skillpayload/skills/**/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/check-skill-node-fences.test.mjs',
    test: 'unbalanced paren reports one SKILL coordinate with node syntax text',
  },
}
