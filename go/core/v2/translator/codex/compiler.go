// Package codex exposes the Codex Harness compiler. The controller must not
// register it until a pinned runtime image passes the Harness conformance suite.
package codex

import (
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
)

// NewCompiler constructs the compile-only Codex adapter.
func NewCompiler(kube translator.Reader) *codingagent.Compiler {
	return codingagent.NewCompiler(codingagent.RuntimeCodex, kube)
}
