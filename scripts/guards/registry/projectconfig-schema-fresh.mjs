export default {
  id: 'projectconfig-schema-fresh',
  rule: 'The .boss-project.json JSON Schema is generated from the projectconfig Go types and is never edited by hand.',
  command: 'make projectconfig-schema-check',
  wiring: { make: 'projectconfig-schema-check', ci: { workflow: 'bazel.yml', job: 'go-test' } },
  scans: [
    'lib/bossalib/projectconfig/*.go',
    'lib/bossalib/projectconfig/schema/boss-project.schema.json',
  ],
  baseline: null,
  proof: {
    file: 'lib/bossalib/projectconfig/schema_gen_test.go',
    test: 'TestSchemaGenerated',
  },
}
