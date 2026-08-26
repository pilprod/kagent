// Package claude exposes the Claude Harness compiler. The controller must not
// register it until a pinned runtime image passes the Harness conformance suite.
package claude

import (
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
)

// NewCompiler constructs the compile-only Claude adapter.
func NewCompiler(kube translator.Reader) *codingagent.Compiler {
	return codingagent.NewCompiler(codingagent.RuntimeClaude, kube)
}
