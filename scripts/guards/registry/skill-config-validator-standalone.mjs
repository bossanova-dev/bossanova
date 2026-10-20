export default {
  id: 'skill-config-validator-standalone',
  rule: 'The compiled .boss-skills.json validator imports nothing, so it loads under plain node with no node_modules.',
  command: 'make test-scripts',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['skills-toolbox/skill-config-validate.generated.mjs'],
  baseline: null,
  proof: {
    file: 'skills-toolbox/skill-config-validate.test.mjs',
    test: 'the generated validator imports and runs alone in an empty directory',
  },
}
