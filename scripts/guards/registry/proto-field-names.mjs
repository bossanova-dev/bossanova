export default {
  id: 'proto-field-names',
  rule: 'Proto fields use boolean prefixes, timestamp suffixes, count names and canonical numeric units; baselined legacy violations only shrink.',
  command: 'node scripts/check-proto-field-names.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: ['proto/bossanova/v1/*.proto', 'scripts/proto-field-names-baseline.json'],
  baseline: 'scripts/proto-field-names-baseline.json',
  proof: {
    file: 'scripts/check-proto-field-names.test.mjs',
    test: 'checkProtoFieldNames fails a seeded Timestamp field named created',
  },
}
