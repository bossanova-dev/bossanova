export default {
  id: 'public-mirror-workflows',
  rule: "The public-mirror workflow restores the public CI set, keeps its leak-prevention clauses, and waits for the mirror PR's checks before merging it.",
  command: 'node scripts/guards/check-public-mirror-workflows.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['.github/workflows/mirror-public.yml', '.github/scripts/export-public-tree.sh'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-public-mirror-workflows.test.mjs',
    test: 'rejects merging the mirror pull request without waiting for its checks',
  },
}
