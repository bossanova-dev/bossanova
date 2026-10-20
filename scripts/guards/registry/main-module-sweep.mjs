export default {
  id: 'main-module-sweep',
  rule: 'Script entry points guard with the shared `isMainModule` helper from skills-toolbox/main-module.mjs, never a hand-rolled argv comparison.',
  command: 'node --test skills-toolbox/main-module.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**/*.mjs', 'skills-toolbox/**/*.mjs'],
  baseline: null,
  proof: {
    file: 'skills-toolbox/main-module.test.mjs',
    test: 'the main-module sweep reports a seeded hand-rolled argv guard',
  },
}
