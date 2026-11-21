export default {
  id: 'gh-hosts-parser-fresh',
  rule: 'The portable gh host-key reader is an import-free generated bundle of the pinned YAML parser.',
  command: 'node scripts/skills/gh-hosts-parser/build.mjs --check',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'scripts/skills/gh-hosts-parser/keys.mjs',
    'scripts/skills/gh-hosts-parser/build.mjs',
    'skills-toolbox/ci/gh-hosts.generated.cjs',
    'package.json',
    'pnpm-lock.yaml',
  ],
  baseline: null,
  proof: {
    file: 'scripts/skills/gh-hosts-parser/build.test.mjs',
    test: 'build --check rejects a modified generated parser',
  },
}
