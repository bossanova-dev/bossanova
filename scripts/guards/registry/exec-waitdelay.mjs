export default {
  id: 'exec-waitdelay',
  rule: 'A Go `exec.Cmd` that captures stdout or stderr into a non-file writer sets `WaitDelay`.',
  command: 'node scripts/check-exec-waitdelay.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'lib/**/*.go',
    'services/**/*.go',
    'plugins/**/*.go',
    'scripts/exec-waitdelay-baseline.json',
  ],
  baseline: 'scripts/exec-waitdelay-baseline.json',
  proof: {
    file: 'scripts/check-exec-waitdelay.test.mjs',
    test: 'violating captured stderr site is flagged',
  },
}
