export default {
  id: 'go-test-makefiles',
  rule: 'Every Go module Makefile dry-runs from the repo root into the shared Go test rules: coverage in `test-all`, `-short` without coverage in `test`.',
  command: 'node --test scripts/check-go-test-makefiles.test.mjs',
  wiring: { make: 'test-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'lib/bossalib/Makefile',
    'services/boss/Makefile',
    'services/bossd/Makefile',
    'services/bosso/Makefile',
    'plugins/bossd-plugin-*/Makefile',
  ],
  baseline: null,
  proof: {
    file: 'scripts/check-go-test-makefiles.test.mjs',
    test: 'a module Makefile without the shared Go test rules fails the gate',
  },
}
