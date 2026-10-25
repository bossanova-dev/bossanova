export default {
  id: 'settings-readiness-figure',
  rule: 'The worst-case session-start figure on the docs Settings page equals the figure the three Go constants compose.',
  command: 'node scripts/guards/check-settings-readiness-figure.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'services/docs/docs/reference/settings.md',
    'lib/bossalib/kit/config/config.go',
    'services/bossd/internal/adapter/tmux/tmux.go',
    'services/bossd/internal/adapter/tmux/tmux_modal.go',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-settings-readiness-figure.test.mjs',
    test: 'a wrong worst-case figure fails naming expected and found',
  },
}
