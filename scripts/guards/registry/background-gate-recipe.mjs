export default {
  id: 'background-gate-recipe',
  rule: "`scripts/gate-run.mjs` reports the backgrounded gate's own exit status and verdict, never the launcher's.",
  command: 'node --test scripts/guards/check-background-gate-recipe.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: ['scripts/gate-run.mjs'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-background-gate-recipe.test.mjs',
    test: 'passing and failing gates report the child status',
  },
}
