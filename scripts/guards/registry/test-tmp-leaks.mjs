export default {
  id: 'test-tmp-leaks',
  rule: 'Script tests remove their temporary directories; empirical TMPDIR residue only shrinks.',
  command: 'make test-scripts',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'scripts/**/*.test.mjs',
    'skills-toolbox/**/*.test.mjs',
    '.claude/skills/verify/lib/*.test.mjs',
    'scripts/guards/test-tmp-leaks-baseline.json',
  ],
  baseline: 'scripts/guards/test-tmp-leaks-baseline.json',
  proof: {
    file: 'scripts/guards/check-test-tmp-leaks.test.mjs',
    test: 'a leaking command fails and the root is removed',
  },
}
