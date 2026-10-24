export default {
  id: 'agent-test-guidance',
  rule: 'AGENTS.md is a regular file and CLAUDE.md is a symlink to AGENTS.md, so one edit reaches both agents.',
  command: 'node --test scripts/guards/check-agent-test-guidance.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['CLAUDE.md', 'AGENTS.md'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-agent-test-guidance.test.mjs',
    test: 'a copied CLAUDE.md fails the symlink gate',
  },
}
