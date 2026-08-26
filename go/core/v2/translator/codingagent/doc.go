// Package codingagent defines the versioned, credential-free ConfigJSON
// contract shared by the Codex and Claude Harness compilers.
//
// This package is compile-only. Controller registration is blocked until pinned
// runtime images consume this contract, materialize MCP and immutable artifact
// inputs, expose /readyz and the private A2A service, persist state in DurableDir,
// and pass the Harness conformance suite.
package codingagent
