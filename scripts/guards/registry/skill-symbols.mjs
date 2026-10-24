export default {
  id: 'skill-symbols',
  rule: 'Every config role, toolbox export and helper verb a skill cites resolves against the repository.',
  command: 'node scripts/guards/check-skill-symbols.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '.claude/skills/**/*.md',
    'lib/bossalib/contract/plugin/skillpayload/skills/**/*.md',
    '.boss-skills.json',
    'skills-toolbox/**/*.mjs',
    'scripts/**/*.mjs',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-skill-symbols.test.mjs',
    test: 'checkSkillSymbols reports each finding with its file, line and kind',
  },
}
