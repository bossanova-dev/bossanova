export default {
  id: 'doc-make-targets',
  rule: 'Every `make <target>` documented in the agent instructions, README, build docs and skill docs names a target its Makefile defines.',
  command: 'node --test scripts/check-doc-make-targets.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'CLAUDE.md',
    'AGENTS.md',
    'README.md',
    'docs/build-and-ci.md',
    'docs/testing/test-command-manifest.md',
    '.claude/skills/**/SKILL.md',
    'lib/bossalib/skillpayload/skills/**/SKILL.md',
    'Makefile',
    'mk/*.mk',
  ],
  baseline: null,
  proof: {
    file: 'scripts/check-doc-make-targets.test.mjs',
    test: 'checkDocMakeTargets flags dead make targets referenced in skill docs',
  },
}
