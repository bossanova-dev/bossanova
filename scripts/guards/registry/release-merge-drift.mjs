export default {
  id: 'release-merge-drift',
  rule: "A release PR's merge adds nothing its head lacks, outside the files the release workflows write.",
  command: 'node scripts/guards/check-release-merge-drift.mjs --head "$HEAD_SHA" --merge HEAD',
  wiring: { make: null, ci: { workflow: 'bazel.yml', job: 'release-drift' } },
  scans: ['**'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-release-merge-drift.test.mjs',
    test: 'a release branch that kept a hunk main reverted is flagged (the #3095 route)',
  },
}
