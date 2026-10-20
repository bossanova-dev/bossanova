export default {
  id: 'entity-names',
  rule: 'Core entity identifiers use canonical names, with existing violations in a shrinking baseline.',
  command: 'node scripts/guards/entity-names.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'docs/structure/entities.yaml',
    '**/*.go',
    'proto/**/*.proto',
    'services/bossd/migrations/*.sql',
    'services/bosso/migrations_postgres/*.sql',
    'services/web/src/**/*.ts',
    'services/web/src/**/*.tsx',
  ],
  baseline: 'scripts/guards/entity-names-baseline.json',
  proof: {
    file: 'scripts/guards/entity-names.test.mjs',
    test: 'entity-names fails a seeded ClaudeChat Go declaration and names AgentChat',
  },
}
