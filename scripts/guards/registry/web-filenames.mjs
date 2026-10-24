export default {
  id: 'web-filenames',
  rule: 'Web TypeScript files use camelCase and TSX files use PascalCase, with explicit conventions and shrinking legacy exceptions.',
  command: 'node scripts/guards/check-web-filenames.mjs',
  wiring: {
    make: 'lint-scripts',
    ci: {
      workflow: 'test-scripts.yml',
      job: 'scripts',
    },
  },
  scans: [
    'services/web/biome.json',
    'services/web/src/**/*.ts',
    'services/web/src/**/*.tsx',
    'services/web/tests/**/*.ts',
    'services/web/tests/**/*.tsx',
    'services/web/e2e/**/*.ts',
    'services/web/e2e/**/*.tsx',
  ],
  baseline: 'scripts/guards/web-filenames-baseline.json',
  proof: {
    file: 'scripts/guards/check-web-filenames.test.mjs',
    test: 'checkWebFilenames fails a seeded kebab-case .ts file',
  },
}
