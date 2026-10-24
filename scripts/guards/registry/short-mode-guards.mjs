export default {
  id: 'short-mode-guards',
  rule: 'The slow tmux and TUI Go test files guard themselves with `testing.Short()` and name the command that runs them in full.',
  command: 'node --test scripts/guards/check-short-mode-guards.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/boss/e2e/*_test.go',
    'services/bossd/internal/session/tmux_chat_test.go',
    'services/bossd/internal/status/tmux_poller_test.go',
    'services/bossd/testharness/e2e_record_chat_tmux_test.go',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-short-mode-guards.test.mjs',
    test: 'a slow test file without a testing.Short() guard fails the gate',
  },
}
