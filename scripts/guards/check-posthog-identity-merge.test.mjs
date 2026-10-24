#!/usr/bin/env node

import assert from 'node:assert/strict'
import { test } from 'node:test'

import {
  FORBIDDEN_INIT_OPTIONS,
  MARKETING_ANALYTICS_PATH,
  WEB_PROVIDER_PATH,
  forbiddenInitOptions,
  identityMergeViolations,
  loadRepoSources,
  posthogWorkflowAssignments,
} from './check-posthog-identity-merge.mjs'

const cleanInit = `posthog.init(token, {
  api_host: host,
  autocapture: false,
  capture_pageview: false,
})
`

const cleanWorkflow = `jobs:
  web:
    env:
      VITE_PUBLIC_POSTHOG_PROJECT_TOKEN: phc_same
      VITE_PUBLIC_POSTHOG_HOST: https://k.example.test
  marketing:
    env:
      PUBLIC_POSTHOG_PROJECT_TOKEN: phc_same
      PUBLIC_POSTHOG_HOST: https://k.example.test
`

const cleanInputs = () => ({
  marketingAnalytics: cleanInit,
  webProvider: cleanInit,
  workflows: { '.github/workflows/release.yml': cleanWorkflow },
})

test('clean synthetic inputs produce no violations', () => {
  assert.deepEqual(identityMergeViolations(cleanInputs()), [])
})

for (const option of FORBIDDEN_INIT_OPTIONS) {
  for (const [label, key] of [
    [MARKETING_ANALYTICS_PATH, 'marketingAnalytics'],
    [WEB_PROVIDER_PATH, 'webProvider'],
  ]) {
    test(`${label} setting ${option} is exactly one violation`, () => {
      const inputs = cleanInputs()
      inputs[key] = cleanInit.replace(
        'autocapture: false,',
        `autocapture: false,\n  ${option}: 'x',`,
      )
      const violations = identityMergeViolations(inputs)
      assert.equal(violations.length, 1, violations.join('\n'))
      assert.match(violations[0], new RegExp(`^${label}: PostHog init sets ${option},`))
    })
  }
}

test('a quoted option key is detected', () => {
  assert.deepEqual(forbiddenInitOptions(`{ "persistence": "memory" }`), ['persistence'])
})

test('option names only match as whole keys', () => {
  assert.deepEqual(forbiddenInitOptions(`{ disable_persistence: true }`), ['disable_persistence'])
  assert.deepEqual(forbiddenInitOptions(`{ persistence_name: 'x' }`), ['persistence_name'])
  assert.deepEqual(forbiddenInitOptions(`const persistence = load()\nfoo(persistence)`), [])
  assert.deepEqual(forbiddenInitOptions(`{ my_persistence: 1, persistenceMode: 2 }`), [])
})

test('a workflow splitting the project token is exactly one violation', () => {
  const inputs = cleanInputs()
  inputs.workflows['.github/workflows/release.yml'] = cleanWorkflow.replace(
    'PUBLIC_POSTHOG_PROJECT_TOKEN: phc_same\n      PUBLIC_POSTHOG_HOST',
    'PUBLIC_POSTHOG_PROJECT_TOKEN: phc_other\n      PUBLIC_POSTHOG_HOST',
  )
  const violations = identityMergeViolations(inputs)
  assert.equal(violations.length, 1, violations.join('\n'))
  assert.match(violations[0], /^\.github\/workflows\/release\.yml: .*PostHog project token/)
})

test('a workflow splitting the host is exactly one violation', () => {
  const inputs = cleanInputs()
  inputs.workflows['.github/workflows/release.yml'] = cleanWorkflow.replace(
    '      PUBLIC_POSTHOG_HOST: https://k.example.test\n',
    '      PUBLIC_POSTHOG_HOST: https://other.example.test\n',
  )
  const violations = identityMergeViolations(inputs)
  assert.equal(violations.length, 1, violations.join('\n'))
  assert.match(violations[0], /^\.github\/workflows\/release\.yml: .*PostHog host/)
})

test('two assignments of one variable with different values in a file are a violation', () => {
  const inputs = cleanInputs()
  inputs.workflows['.github/workflows/release.yml'] =
    cleanWorkflow + `  later:\n    env:\n      VITE_PUBLIC_POSTHOG_PROJECT_TOKEN: "phc_drift"\n`
  const violations = identityMergeViolations(inputs)
  assert.equal(violations.length, 1, violations.join('\n'))
  assert.match(violations[0], /project token/)
})

test('different tokens across separate workflow files are allowed', () => {
  const inputs = cleanInputs()
  inputs.workflows['.github/workflows/staging.yml'] = cleanWorkflow.replaceAll(
    'phc_same',
    'phc_staging',
  )
  assert.deepEqual(identityMergeViolations(inputs), [])
})

test('quoted and commented workflow values normalize to one value', () => {
  const assignments = posthogWorkflowAssignments(
    `      PUBLIC_POSTHOG_HOST: 'https://k.example.test'\n      VITE_PUBLIC_POSTHOG_HOST: https://k.example.test # shared\n`,
  )
  assert.deepEqual([...assignments.get('PUBLIC_POSTHOG_HOST')], ['https://k.example.test'])
  assert.deepEqual([...assignments.get('VITE_PUBLIC_POSTHOG_HOST')], ['https://k.example.test'])
})

test('the real marketing, web and workflow sources hold every merge precondition', () => {
  const sources = loadRepoSources()
  // Non-vacuity: the release workflows the rule exists for are read, and the
  // parser actually sees both apps' token and host in each of them. A parser
  // that silently matched nothing would otherwise pass this test forever.
  for (const workflow of [
    '.github/workflows/perform-production-release.yml',
    '.github/workflows/perform-staging-release.yml',
  ]) {
    assert.ok(workflow in sources.workflows, `${workflow} was not loaded`)
    const assignments = posthogWorkflowAssignments(sources.workflows[workflow])
    for (const name of [
      'PUBLIC_POSTHOG_PROJECT_TOKEN',
      'VITE_PUBLIC_POSTHOG_PROJECT_TOKEN',
      'PUBLIC_POSTHOG_HOST',
      'VITE_PUBLIC_POSTHOG_HOST',
    ]) {
      assert.ok(assignments.has(name), `${workflow} assigns no ${name}`)
    }
  }
  assert.match(sources.marketingAnalytics, /posthog\.init\(/)
  assert.match(sources.webProvider, /PostHogProvider/)
  assert.deepEqual(identityMergeViolations(sources), [])
})
