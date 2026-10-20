export default {
  id: 'email-course',
  rule: 'The trial onboarding email course keeps its send structure, length bands, voice, published skill names, live docs links and trial event contract.',
  command: 'node scripts/check-email-course.mjs',
  wiring: { make: 'lint-scripts', ci: { workflow: 'test-scripts.yml', job: 'scripts' } },
  scans: [
    'docs/email-course/**',
    'services/docs/docs/**',
    'lib/bossalib/skillpayload/skills_manifest_test.go',
    'services/bosso/cmd/trial_enrollment.go',
  ],
  baseline: null,
  proof: { file: 'scripts/check-email-course.test.mjs', test: 'voice: a marketing verb fails' },
}
