export default {
  id: 'roadmap-only-skip',
  rule: 'The bazel.yml jobs a roadmap-only PR skips read nothing under docs/roadmap/',
  command: 'node --test scripts/guards/check-roadmap-only-skip.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    '.github/workflows/bazel.yml',
    'docs/roadmap/**',
    'scripts/guards/registry/**',
    'scripts/test-infra/select-affected-tests.mjs',
  ],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-roadmap-only-skip.test.mjs',
    test: 'a guard wired to a skippable job that scans docs/ is reported',
  },
}
