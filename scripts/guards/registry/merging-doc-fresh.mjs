export default {
  id: 'merging-doc-fresh',
  rule: 'The merge-reason reference in docs/philosophy/merging.md is generated from the merge gate’s reason lists and config, and is never edited by hand.',
  command: 'node scripts/guards/merging-doc.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'docs/philosophy/merging.md',
    'skills-toolbox/epic/merge-eligibility.mjs',
    'skills-toolbox/build/completion-gate.mjs',
    'skills-toolbox/verify/verify-gate.mjs',
    'skills-toolbox/skill-config.mjs',
    '.boss-skills.json',
    '.boss-project.json',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/merging-doc.test.mjs',
    test: '--check fails when a merge-eligibility reason is added without regenerating',
  },
}
