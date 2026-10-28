import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync, rmSync, existsSync, symlinkSync, statSync } from 'node:fs'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { runOperation, resolveOperation, toolboxRoot } from './operation-runner.mjs'
import { runCli } from './cli.mjs'
import { DEFAULT_CONFIG, mergeConfig } from '../skill-config.mjs'

function fixture(t, source) {
  const root = mkdtempSync(path.join(tmpdir(), 'tracker-operation-'))
  t.after(() => rmSync(root, { recursive: true, force: true, maxRetries: 3 }))
  writeFileSync(path.join(root, 'helper.mjs'), source)
  return root
}
const command = {
  transport: 'command',
  command: { helper: 'helper.mjs', verb: 'echo', args: ['fixed'] },
  summary: '{body}',
}
const echo = `import {readFileSync} from 'node:fs'; const argv=process.argv.slice(2); console.log(JSON.stringify({argv,args:JSON.parse(readFileSync(argv.at(-1),'utf8'))}));`

test('resolver excludes inherited and ambiguous operations', () => {
  assert.equal(resolveOperation({}, 'constructor'), null)
  assert.equal(resolveOperation({ x: { tool: 'mcp', command: command.command } }, 'x'), null)
  assert.deepEqual(resolveOperation({ x: { tool: 'mcp', summary: '{}' } }, 'x'), {
    transport: 'mcp',
    tool: 'mcp',
    summary: '{}',
  })
})
test('MCP operations only emit descriptors', () => {
  assert.deepEqual(
    runOperation({ transport: 'mcp', tool: 'mcp', summary: '{}' }, { body: 'hello' }),
    { transport: 'mcp', tool: 'mcp', args: { body: 'hello' }, outcome: 'descriptor-emitted' },
  )
})
test('command arguments travel intact through a private args file and are cleaned', (t) => {
  const root = fixture(t, echo)
  const args = { body: 'multi\n日本語\n`$()"' }
  const result = runOperation(command, args, { toolboxRoot: () => root })
  assert.equal(result.outcome, 'operation-executed')
  assert.deepEqual(result.result.args, args)
  assert.deepEqual(result.result.argv.slice(0, 3), ['echo', 'fixed', '--args-file'])
  assert.equal(existsSync(result.result.argv.at(-1)), false)
})
test('command failures never expose a result', (t) => {
  for (const [source, pattern] of [
    ["console.error('first\\nlast diagnostic');process.exit(7)", /7.*last diagnostic/],
    ["console.log('not json')", /JSON/],
    ['setInterval(()=>{},1000)', /ETIMEDOUT|timeout/],
  ]) {
    const root = fixture(t, source)
    assert.throws(
      () =>
        runOperation(
          command,
          {},
          { toolboxRoot: () => root, timeoutMs: source.includes('setInterval') ? 200 : 5000 },
        ),
      pattern,
    )
  }
  const root = fixture(t, '')
  rmSync(path.join(root, 'helper.mjs'))
  assert.throws(() => runOperation(command, {}, { toolboxRoot: () => root }), /missing|ENOENT/)
})
test('absolute, escaping and symlink helpers are refused', (t) => {
  const root = fixture(t, echo)
  const outside = fixture(t, echo)
  symlinkSync(path.join(outside, 'helper.mjs'), path.join(root, 'linked.mjs'))
  for (const helper of ['/tmp/helper.mjs', '../helper.mjs', 'linked.mjs']) {
    assert.throws(
      () =>
        runOperation(
          { ...command, command: { helper, verb: 'echo' } },
          {},
          { toolboxRoot: () => root },
        ),
      /helper|outside|escape/,
    )
  }
})
async function cli(argv, map) {
  let stdout = '',
    stderr = ''
  const code = await runCli(argv, {
    write: (s) => {
      stdout += s
    },
    errWrite: (s) => {
      stderr += s
    },
    resolveAdapter: () => ({ operationMap: map }),
    loadConfig: () =>
      mergeConfig(DEFAULT_CONFIG, {
        trackerConfig: { linear: { team: 'Example', states: { unplanned: 'Todo' } } },
      }),
  })
  return { code, stdout, stderr }
}
test('invoke-operation distinguishes caller errors from missing operations', async (t) => {
  const root = fixture(t, echo)
  const bad = path.join(root, 'bad.json')
  writeFileSync(bad, '[]')
  for (const argv of [
    ['invoke-operation'],
    ['invoke-operation', 'constructor'],
    ['invoke-operation', 'getIssue', '--unknown'],
    ['invoke-operation', 'getIssue', '--args-file', bad],
  ]) {
    const out = await cli(argv, {})
    assert.equal(out.code, 64)
    assert.equal(out.stdout, '')
  }
  const absent = await cli(['invoke-operation', 'getIssue'], {})
  assert.equal(absent.code, 2)
  assert.equal(absent.stdout, '')
  const out = await cli(['invoke-operation', 'getIssue'], {
    getIssue: { tool: 'mcp', summary: '{}' },
  })
  assert.equal(out.code, 0)
  assert.deepEqual(JSON.parse(out.stdout), {
    transport: 'mcp',
    tool: 'mcp',
    args: {},
    outcome: 'descriptor-emitted',
  })
})
test('CLI operations and file writes execute command transports with the same logical arguments', async (t) => {
  const helper = `.tracker-operation-${process.pid}.mjs`
  writeFileSync(path.join(toolboxRoot(), helper), echo)
  t.after(() => rmSync(path.join(toolboxRoot(), helper), { force: true }))
  const root = fixture(t, echo),
    body = path.join(root, 'body.md')
  writeFileSync(body, 'hello\n世界')
  const argsFile = path.join(root, 'input.json')
  writeFileSync(argsFile, JSON.stringify({ body: 'hello\n世界' }))
  const op = { command: { helper, verb: 'echo' }, summary: '{id, body}' }
  const map = { getIssue: op, createIssue: op, updateComment: op, writeDescription: op }
  const listed = await cli(['operations', '--require', 'getIssue'], map)
  assert.equal(listed.code, 0)
  assert.equal(JSON.parse(listed.stdout).operationMap.getIssue.transport, 'command')
  for (const [argv, expected] of [
    [['invoke-operation', 'getIssue'], {}],
    [['invoke-operation', 'getIssue', '--args-file', argsFile], { body: 'hello\n世界' }],
    [['update-comment', '--id', 'id', '--body-file', body], { id: 'id', body: 'hello\n世界' }],
    [
      ['write-description', '--id', 'id', '--body-file', body],
      { id: 'id', description: 'hello\n世界' },
    ],
    [
      ['create-issue', '--title', 'title', '--body-file', body, '--state-role', 'unplanned'],
      { team: 'Example', title: 'title', description: 'hello\n世界', state: 'Todo', labels: [] },
    ],
  ]) {
    const out = await cli(argv, map)
    assert.equal(out.code, 0, out.stderr)
    const value = JSON.parse(out.stdout)
    assert.equal(value.outcome, 'operation-executed')
    assert.deepEqual(value.result.args, expected)
  }
  writeFileSync(body, '  ')
  for (const verb of ['update-comment', 'write-description', 'create-issue']) {
    const argv =
      verb === 'create-issue'
        ? [verb, '--title', 'title', '--state-role', 'unplanned', '--body-file', body]
        : [verb, '--id', 'id', '--body-file', body]
    const out = await cli(argv, map)
    assert.equal(out.code, 2)
    assert.equal(out.stdout, '')
    assert.match(out.stderr, /empty/)
  }
  const failed = await cli(['invoke-operation', 'getIssue'], {
    getIssue: { command: { helper: 'missing.mjs', verb: 'echo' }, summary: '{}' },
  })
  assert.equal(failed.code, 2)
  assert.equal(failed.stdout, '')
  assert.match(failed.stderr, /command failed/)
})

test('spawn is bounded, shell-free, and cleans private arguments after a failed helper', (t) => {
  const root = fixture(t, echo)
  let argsFile
  assert.throws(
    () =>
      runOperation(
        command,
        { body: 'private' },
        {
          toolboxRoot: () => root,
          spawn: (executable, argv, options) => {
            assert.equal(executable, process.execPath)
            assert.equal(options.timeout, 120_000)
            assert.ok(options.maxBuffer > 0)
            assert.notEqual(options.shell, true)
            argsFile = argv.at(-1)
            assert.equal(statSync(argsFile).mode & 0o777, 0o600)
            return { status: 9, signal: null, stderr: 'first\nlast', stdout: '{"partial":true}' }
          },
        },
      ),
    (error) => error.status === 9 && error.diagnostic === 'last' && !('result' in error),
  )
  assert.equal(existsSync(argsFile), false)
})
