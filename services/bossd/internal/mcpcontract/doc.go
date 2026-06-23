// Package mcpcontract holds a cross-module contract test that drives the
// bossmcp MCP tools against a real bossd daemon. It lives in the bossd module
// because that is the only place able to import both the shared bossmcp tool
// package (lib/bossalib) and bossd's internal test harness.
//
// The test guards against a class of bug where an MCP tool builds a protobuf
// request that omits a field bossd validates as required (for example, the
// create_session tool that originally never set Title, or
// clone_and_register_repo that never set LocalPath). The fake-backend unit
// tests in lib/bossalib/bossmcp cannot catch this because they never exercise
// bossd's real server-side validation.
package mcpcontract
