export default {
  id: 'skill-shell',
  rule: 'Every fenced shell block in the skill trees passes `bash -n` and the skill-shell hazard checks.',
  command: 'node scripts/check-skill-shell.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.claude/skills/**/*.md', 'lib/bossalib/skillpayload/skills/**/*.md'],
  baseline: null,
  proof: {
    file: 'scripts/check-skill-shell.test.mjs',
    test: 'a genuine syntax error is reported with its file, fence line and kind:syntax',
  },
}
