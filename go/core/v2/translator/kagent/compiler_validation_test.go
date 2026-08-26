package kagent

import (
	"context"
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/stretchr/testify/require"
)

func TestCompilerValidatesRuntimeInputsBeforeDereferencingThem(t *testing.T) {
	tests := []struct {
		name    string
		input   *v2translator.HarnessInput
		wantErr string
	}{
		{name: "nil input", wantErr: "requires a Harness"},
		{
			name: "missing root",
			input: &v2translator.HarnessInput{Harness: &v1alpha3.Harness{Spec: v1alpha3.HarnessSpec{
				Kagent: &v1alpha3.KagentHarness{}, Workload: &v1alpha3.HarnessWorkload{}, Substrate: &v1alpha3.HarnessSubstratePolicy{},
			}}},
			wantErr: "resolved root AgentTemplate and ModelConfig",
		},
		{
			name: "wrong runtime",
			input: &v2translator.HarnessInput{
				Harness: &v1alpha3.Harness{Spec: v1alpha3.HarnessSpec{Codex: &v1alpha3.CodexHarness{}}},
				Root:    resolvedRoot(),
			},
			wantErr: "requires the kagent Harness runtime",
		},
		{
			name: "missing substrate policy",
			input: &v2translator.HarnessInput{
				Harness: &v1alpha3.Harness{Spec: v1alpha3.HarnessSpec{Kagent: &v1alpha3.KagentHarness{}, Workload: &v1alpha3.HarnessWorkload{}}},
				Root:    resolvedRoot(),
			},
			wantErr: "requires workload and substrate",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (&Compiler{}).Compile(context.Background(), test.input)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func resolvedRoot() *v2translator.AgentInput {
	return &v2translator.AgentInput{Template: &v1alpha3.AgentTemplate{}, ModelConfig: &v1alpha3.ModelConfig{}}
}
