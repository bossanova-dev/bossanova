export default {
  id: 'no-inline-stop-hooks',
  rule: 'The bossd Stop-hook filter lives only in `skills-toolbox/remove-bossd-stop-hooks.mjs`, never copied into a SKILL.md.',
  command: 'node scripts/check-no-inline-stop-hooks.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '.claude/skills/**/SKILL.md',
    '.codex/skills/**/SKILL.md',
    'lib/bossalib/skillpayload/skills/**/SKILL.md',
  ],
  baseline: null,
  proof: {
    file: 'scripts/check-no-inline-stop-hooks.test.mjs',
    test: 'findInlineStopHookCopies returns offending SKILL.md paths',
  },
}
