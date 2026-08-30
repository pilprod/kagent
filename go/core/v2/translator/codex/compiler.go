// Package codex exposes the Codex Harness compiler for ExternalSlot revisions.
package codex

import (
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
)

// NewCompiler constructs the Codex ExternalSlot adapter.
func NewCompiler(kube translator.Reader) *codingagent.Compiler {
	return codingagent.NewCompiler(codingagent.RuntimeCodex, kube)
}
