// Package claude exposes the Claude Harness compiler for ExternalSlot revisions.
package claude

import (
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
)

// NewCompiler constructs the compile-only Claude adapter.
func NewCompiler(kube translator.Reader) *codingagent.Compiler {
	return codingagent.NewCompiler(codingagent.RuntimeClaude, kube)
}
