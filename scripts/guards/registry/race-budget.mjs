export default {
  id: 'race-budget',
  rule: "Each budgeted Go test target's `-race` wall-clock stays under its budget in `scripts/race-budgets.json`.",
  command: 'node scripts/check-race-budget.mjs --bep "$bep"',
  wiring: { make: 'check-race-budget', ci: { workflow: 'bazel-linux-smoke.yml', job: 'smoke' } },
  scans: ['scripts/race-budgets.json'],
  baseline: null,
  proof: {
    file: 'scripts/check-race-budget.test.mjs',
    test: 'checkRaceBudget FAILS on a synthetic BEP at the pre-fix duration',
  },
}
