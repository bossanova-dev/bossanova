export default {
  id: 'race-budget',
  rule: "Each budgeted Go test target's `-race` wall-clock stays under its budget in `scripts/guards/race-budgets.json`.",
  command: 'node scripts/guards/check-race-budget.mjs --bep "$bep"',
  wiring: { make: 'check-race-budget', ci: { workflow: 'bazel-linux-smoke.yml', job: 'smoke' } },
  scans: ['scripts/guards/race-budgets.json'],
  baseline: null,
  proof: {
    file: 'scripts/guards/check-race-budget.test.mjs',
    test: 'checkRaceBudget FAILS on a synthetic BEP at the pre-fix duration',
  },
}
