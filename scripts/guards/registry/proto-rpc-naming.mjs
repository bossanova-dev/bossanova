export default {
  id: 'proto-rpc-naming',
  rule: 'RPC request/response messages are named <Rpc>Request/<Rpc>Response; exceptions are named per RPC with buf:lint:ignore.',
  command: 'make lint-proto',
  wiring: { make: 'lint-proto', ci: { workflow: 'test-proto.yml', job: 'proto' } },
  scans: ['buf.yaml', 'proto/**/*.proto'],
  baseline: null,
  proof: {
    file: 'scripts/guards/proto-rpc-naming.test.mjs',
    test: 'buf lint fails a seeded RPC whose request message name does not match',
  },
}
