export default {
  id: 'mirror-leaks',
  rule: 'No commit pushed to the public mirror carries a private path or a private env-var assignment.',
  command: 'bash scripts/check-mirror-leaks.sh public/main public-mirror',
  wiring: { make: null, ci: { workflow: 'mirror-public.yml', job: 'mirror' } },
  scans: ['**'],
  baseline: null,
  proof: {
    file: 'scripts/check-mirror-leaks.test.mjs',
    test: 'fails on a forbidden root file (AGENTS.md / CLAUDE.md)',
  },
}
