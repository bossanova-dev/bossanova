export default {
  id: 'script-file-names',
  rule: 'Script and toolbox paths use kebab-case with dotted suffixes, and every baseline belongs to a registered guard.',
  command: 'node scripts/guards/script-file-names.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/**', 'skills-toolbox/**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/script-file-names.test.mjs',
    test: 'script-file-names rejects a camel-case file with the registered command',
  },
}
