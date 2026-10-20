export default {
  id: 'dispatch-contract',
  rule: 'Skill prose and agent guidance never claim that subagent dispatch accepts `run_in_background`.',
  command: 'node scripts/check-dispatch-contract.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '.claude/skills/**/*.md',
    'lib/bossalib/skillpayload/skills/**/*.md',
    'CLAUDE.md',
    'services/bossd/internal/session/tmux_chat.go',
  ],
  baseline: null,
  proof: {
    file: 'scripts/check-dispatch-contract.test.mjs',
    test: 'flags an affirmative claim that subagent dispatch accepts run_in_background',
  },
}
