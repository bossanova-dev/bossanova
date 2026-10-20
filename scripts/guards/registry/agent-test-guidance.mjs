export default {
  id: 'agent-test-guidance',
  rule: 'AGENTS.md is a symlink to CLAUDE.md, so one edit reaches both agents.',
  command: 'node --test scripts/check-agent-test-guidance.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['AGENTS.md', 'CLAUDE.md'],
  baseline: null,
  proof: {
    file: 'scripts/check-agent-test-guidance.test.mjs',
    test: 'a copied AGENTS.md fails the symlink gate',
  },
}
