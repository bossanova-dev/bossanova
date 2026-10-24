export default {
  id: 'dispatch-contract',
  rule: 'Skill prose and agent guidance never claim that subagent dispatch accepts `run_in_background`.',
  command: 'node scripts/guards/check-dispatch-contract.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '.claude/skills/**/*.md',
    'lib/bossalib/contract/plugin/skillpayload/skills/**/*.md',
    'AGENTS.md',
    'docs/guides/agent-shell-traps.md',
    'services/bossd/internal/session/tmux_chat.go',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-dispatch-contract.test.mjs',
    test: 'flags an affirmative claim that subagent dispatch accepts run_in_background',
  },
}
