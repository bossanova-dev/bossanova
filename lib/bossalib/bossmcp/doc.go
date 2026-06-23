// Package bossmcp defines the bossanova MCP tool set once, independent of
// transport (stdio or Streamable HTTP) and deployment (local socket or hosted
// orchestrator). Tool handlers are written against the narrow Backend
// interface declared here; each binary supplies a Backend implementation.
//
// This package imports only the shared generated proto types
// (github.com/recurser/bossalib/gen/bossanova/v1) for payloads and the MCP
// SDK. It must never import services/boss/internal/* (module-boundary rule).
package bossmcp
