export default {
  id: 'skill-config-validator-fresh',
  rule: 'The .boss-skills.json validator is compiled from scripts/skills/skill-config-schema/schema.mjs and is never edited by hand.',
  command: 'node scripts/skills/skill-config-schema/build.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'scripts/skills/skill-config-schema/schema.mjs',
    'scripts/skills/skill-config-schema/build.mjs',
    'skills-toolbox/skill-config-validate.generated.mjs',
  ],
  baseline: null,
  proof: {
    file: 'scripts/skills/skill-config-schema/build.test.mjs',
    test: 'build --check fails after a seeded schema edit',
  },
}
