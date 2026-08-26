package external_test

import (
	"context"
	"encoding/json"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	externaltranslator "github.com/kagent-dev/kagent/go/core/v2/translator/external"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestCompilerProducesCanonicalSanitizedProfile(t *testing.T) {
	compiler := newCompiler(t, dbpkg.ExternalRuntimeCodex)
	input := externalInput(dbpkg.ExternalRuntimeCodex)
	input.Root.Instruction = "review carefully"
	input.Root.MCPTools = []v2translator.ResolvedMCPTool{
		resolvedTool("z-tools", "write", "read", "write"),
		resolvedTool("a-tools", "search", "fetch"),
		resolvedTool("z-tools", "inspect"),
	}

	revision, err := compiler.Compile(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, dbpkg.RuntimeBackendKindExternal, revision.BackendKind)
	require.Equal(t, dbpkg.ExternalRuntimeCodex, revision.ExternalRuntime)
	require.JSONEq(t, `{
		"version":"v1",
		"instruction":"review carefully",
		"tools":[
			{"server":"a-tools","allow":["fetch","search"]},
			{"server":"z-tools","allow":["inspect","read","write"]}
		]
	}`, string(revision.ExternalProfile))
	require.Equal(t, `{"version":"v1","instruction":"review carefully","tools":[{"server":"a-tools","allow":["fetch","search"]},{"server":"z-tools","allow":["inspect","read","write"]}]}`, string(revision.ExternalProfile))
	require.Empty(t, revision.Image)
	require.Nil(t, revision.Environment)
	require.Nil(t, revision.ConfigJSON)
	require.NotEmpty(t, revision.AgentCardJSON)
	var card a2atype.AgentCard
	require.NoError(t, json.Unmarshal(revision.AgentCardJSON, &card))
	require.Equal(t, "assistant", card.Name)
	require.Equal(t, "v1", card.Version)
	require.Len(t, card.SupportedInterfaces, 1)
	require.Equal(t, a2atype.TransportProtocolJSONRPC, card.SupportedInterfaces[0].ProtocolBinding)
	require.Equal(t, a2atype.Version, card.SupportedInterfaces[0].ProtocolVersion)
	require.NotEmpty(t, card.DefaultInputModes)
	require.NotEmpty(t, card.DefaultOutputModes)
	require.NotContains(t, string(revision.AgentCardJSON), "execution-profile")
	require.Empty(t, revision.WorkerPoolName)
	require.Empty(t, revision.SnapshotLocation)
	require.NotEmpty(t, revision.Provenance)
	require.Equal(t, `{"version":"v1","harness":{"kind":"Harness","namespace":"test-namespace","name":"codex"},"agentTemplate":{"kind":"AgentTemplate","namespace":"test-namespace","name":"assistant"},"mcpServers":[{"kind":"RemoteMCPServer","namespace":"test-namespace","name":"a-tools"},{"kind":"RemoteMCPServer","namespace":"test-namespace","name":"z-tools"}]}`, string(revision.Provenance))
	require.NotNil(t, revision.EgressDestinations)
	require.Empty(t, revision.EgressDestinations)
	require.NotContains(t, string(revision.ExternalProfile), "mcp.example.test")
	require.NotContains(t, string(revision.Provenance), "mcp.example.test")
	require.NotContains(t, string(revision.ExternalProfile), "test-namespace")
}

func TestCompilerAllowsEmptyProfile(t *testing.T) {
	revision, err := newCompiler(t, dbpkg.ExternalRuntimeClaude).Compile(context.Background(), externalInput(dbpkg.ExternalRuntimeClaude))
	require.NoError(t, err)
	require.Equal(t, `{"version":"v1","instruction":"","tools":[]}`, string(revision.ExternalProfile))
}

func TestCompilerDigestIgnoresModelConfiguration(t *testing.T) {
	compiler := newCompiler(t, dbpkg.ExternalRuntimeCodex)
	firstInput := externalInput(dbpkg.ExternalRuntimeCodex)
	effort := v1alpha3.OpenAIReasoningEffort("low")
	firstInput.Root.ModelConfig.Spec = v1alpha3.ModelConfigSpec{
		Model: "gpt-first", Provider: v1alpha3.ModelProviderOpenAI,
		APIKeySecret: "cluster-model-key", APIKeySecretKey: "token",
		DefaultHeaders: map[string]string{"Authorization": "credential"},
		TLS:            &v1alpha3.TLSConfig{CACertSecretRef: "cluster-ca", CACertSecretKey: "ca.crt"},
		OpenAI:         &v1alpha3.OpenAIConfig{ReasoningEffort: &effort, BaseURL: "https://first.example.test"},
	}
	firstInput.Root.ModelConfig.Name = "first-model"
	firstInput.Root.ModelConfig.UID = "first-model-uid"
	firstInput.Root.ModelConfig.Generation = 7

	secondInput := externalInput(dbpkg.ExternalRuntimeCodex)
	secondInput.Root.ModelConfig = nil

	first, err := compiler.Compile(context.Background(), firstInput)
	require.NoError(t, err)
	second, err := compiler.Compile(context.Background(), secondInput)
	require.NoError(t, err)
	require.Equal(t, first.ExternalProfile, second.ExternalProfile)
	require.Equal(t, first.AgentCardJSON, second.AgentCardJSON)
	require.NotContains(t, string(first.ExternalProfile), "cluster-model-key")
	require.NotContains(t, string(first.AgentCardJSON), "cluster-model-key")
	firstDigest, err := first.Digest()
	require.NoError(t, err)
	secondDigest, err := second.Digest()
	require.NoError(t, err)
	require.Equal(t, firstDigest, secondDigest)
}

func TestCompilerRejectsUnsupportedConfiguration(t *testing.T) {
	literal := "value"
	tests := []struct {
		name    string
		mutate  func(*v2translator.HarnessInput)
		wantErr string
	}{
		{
			name: "workload",
			mutate: func(input *v2translator.HarnessInput) {
				input.Harness.Spec.Workload = &v1alpha3.HarnessWorkload{Image: "example.test/runtime@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			},
			wantErr: "workload, substrate, or env",
		},
		{
			name: "env",
			mutate: func(input *v2translator.HarnessInput) {
				input.Harness.Spec.Env = []v1alpha3.HarnessEnvVar{{Name: "TOKEN", Value: &literal}}
			},
			wantErr: "workload, substrate, or env",
		},
		{
			name: "skills",
			mutate: func(input *v2translator.HarnessInput) {
				input.Root.Template.Spec.Skills = []v1alpha3.AgentTemplateSkill{{Name: "review"}}
			},
			wantErr: "skills",
		},
		{
			name: "plugins",
			mutate: func(input *v2translator.HarnessInput) {
				input.Root.Template.Spec.Plugins = []v1alpha3.PluginBundle{{}}
			},
			wantErr: "plugins",
		},
		{
			name: "shared child",
			mutate: func(input *v2translator.HarnessInput) {
				input.Root.Shared = []v2translator.AgentInputBinding{{Name: "child", Agent: &v2translator.AgentInput{}}}
			},
			wantErr: "Shared",
		},
		{
			name: "MCP headers",
			mutate: func(input *v2translator.HarnessInput) {
				tool := resolvedTool("tools", "read")
				tool.Server.Spec.HeadersFrom = []v1alpha3.ValueRef{{Name: "Authorization", Value: "secret"}}
				input.Root.MCPTools = []v2translator.ResolvedMCPTool{tool}
			},
			wantErr: "headers or TLS",
		},
		{
			name: "empty MCP allowlist",
			mutate: func(input *v2translator.HarnessInput) {
				input.Root.MCPTools = []v2translator.ResolvedMCPTool{resolvedTool("tools")}
			},
			wantErr: "non-empty MCP tool allowlist",
		},
		{
			name: "MCP cluster CA",
			mutate: func(input *v2translator.HarnessInput) {
				tool := resolvedTool("tools", "read")
				tool.Server.Spec.TLS = &v1alpha3.TLSConfig{CACertSecretRef: "mcp-ca", CACertSecretKey: "ca.crt"}
				input.Root.MCPTools = []v2translator.ResolvedMCPTool{tool}
			},
			wantErr: "headers or TLS",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := externalInput(dbpkg.ExternalRuntimeCodex)
			test.mutate(input)
			_, err := newCompiler(t, dbpkg.ExternalRuntimeCodex).Compile(context.Background(), input)
			require.ErrorContains(t, err, test.wantErr)
		})
	}
}

func TestCompilerRejectsMismatchedAndInvalidRuntimes(t *testing.T) {
	_, err := externaltranslator.NewCompiler(dbpkg.ExternalRuntime("other"))
	require.ErrorContains(t, err, "not supported")

	compiler := newCompiler(t, dbpkg.ExternalRuntimeCodex)
	_, err = compiler.Compile(context.Background(), externalInput(dbpkg.ExternalRuntimeClaude))
	require.ErrorContains(t, err, "does not match")
}

func newCompiler(t *testing.T, runtime dbpkg.ExternalRuntime) *externaltranslator.Compiler {
	t.Helper()
	compiler, err := externaltranslator.NewCompiler(runtime)
	require.NoError(t, err)
	return compiler
}

func externalInput(runtime dbpkg.ExternalRuntime) *v2translator.HarnessInput {
	harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: string(runtime), Namespace: "test-namespace"}}
	switch runtime {
	case dbpkg.ExternalRuntimeCodex:
		harness.Spec.Codex = &v1alpha3.CodexHarness{}
	case dbpkg.ExternalRuntimeClaude:
		harness.Spec.Claude = &v1alpha3.ClaudeHarness{}
	}
	return &v2translator.HarnessInput{
		Harness: harness,
		Root: &v2translator.AgentInput{
			Template: &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "test-namespace"}},
			ModelConfig: &v1alpha3.ModelConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "ignored-model", Namespace: "test-namespace"},
				Spec:       v1alpha3.ModelConfigSpec{Model: "ignored", Provider: v1alpha3.ModelProviderOpenAI},
			},
		},
	}
}

func resolvedTool(server string, allow ...string) v2translator.ResolvedMCPTool {
	return v2translator.ResolvedMCPTool{
		Binding: v1alpha3.MCPToolBinding{
			Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server},
			Tools:  allow,
		},
		Server: &v1alpha3.RemoteMCPServer{
			ObjectMeta: metav1.ObjectMeta{Name: server, Namespace: "test-namespace"},
			Spec: v1alpha3.RemoteMCPServerSpec{
				URL:      "https://mcp.example.test/" + server,
				Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp,
			},
		},
	}
}
