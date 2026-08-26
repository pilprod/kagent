package codingagent_test

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/claude"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codex"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	schemev1 "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const testImage = "example.com/coding-agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

type testReader struct{ client.Client }

func (r testReader) Get(ctx context.Context, key types.NamespacedName, object runtime.Object) error {
	return r.Client.Get(ctx, key, object.(client.Object))
}

func TestCodexAndClaudeCompilersRenderPortableResolvedBundle(t *testing.T) {
	for _, test := range []struct {
		name     string
		runtime  codingagent.Runtime
		provider v1alpha3.ModelProvider
	}{
		{name: "codex", runtime: codingagent.RuntimeCodex, provider: v1alpha3.ModelProviderOpenAI},
		{name: "claude", runtime: codingagent.RuntimeClaude, provider: v1alpha3.ModelProviderAnthropic},
	} {
		t.Run(test.name, func(t *testing.T) {
			revision := compileFixture(t, test.runtime, test.provider)
			config, err := codingagent.Decode(revision.ConfigJSON)
			require.NoError(t, err)
			require.Equal(t, test.runtime, config.Runtime)
			require.Equal(t, "coordinator", config.Root.TemplateName)
			require.Equal(t, "coordinate", config.Root.Instruction)
			require.Equal(t, string(test.provider), config.Root.Model.Provider)
			if test.runtime == codingagent.RuntimeCodex {
				require.Equal(t, "high", config.Root.Model.ReasoningEffort)
			} else {
				require.Empty(t, config.Root.Model.ReasoningEffort)
			}
			require.Len(t, config.Root.MCPServers, 1)
			require.Equal(t, "search", config.Root.MCPServers[0].Server)
			require.Equal(t, []string{"lookup", "search"}, config.Root.MCPServers[0].Tools)
			require.Equal(t, "https://search.example.com/mcp", config.Root.MCPServers[0].Connection.URL)
			require.Equal(t, "STREAMABLE_HTTP", config.Root.MCPServers[0].Connection.Transport)
			require.Equal(t, "review", config.Root.Skills[0].Name)
			require.Equal(t, "deploy", config.Root.Plugins[0].Skills[0])
			require.Len(t, config.Root.SharedAgents, 1)
			require.Equal(t, "researcher", config.Root.SharedAgents[0].Name)
			require.Equal(t, "research-template", config.Root.SharedAgents[0].Agent.TemplateName)
			for _, host := range []string{"search.example.com", "ghcr.io", "github.com"} {
				require.Truef(t, slices.Contains(revision.EgressDestinations, host), "egress %v lacks %q", revision.EgressDestinations, host)
			}
			providerHost := "api.openai.com"
			if test.runtime == codingagent.RuntimeClaude {
				providerHost = "api.anthropic.com"
			}
			require.Contains(t, revision.EgressDestinations, providerHost)
			require.Contains(t, string(revision.Provenance), `"kind":"RemoteMCPServer"`)
			require.Equal(t, testImage, revision.Image)
			require.Equal(t, "workers", revision.WorkerPoolName)
			require.Equal(t, "snapshots", revision.SnapshotLocation)
		})
	}
}

func compileFixture(t *testing.T, selectedRuntime codingagent.Runtime, provider v1alpha3.ModelProvider) *translator.Revision {
	t.Helper()
	require.NoError(t, v1alpha3.AddToScheme(schemev1.Scheme))
	effort := v1alpha3.OpenAIReasoningEffort("high")
	rootModel := &v1alpha3.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "root-model", Namespace: "test"},
		Spec:       v1alpha3.ModelConfigSpec{Provider: provider, Model: "model-root"},
	}
	childModel := &v1alpha3.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "child-model", Namespace: "test"},
		Spec:       v1alpha3.ModelConfigSpec{Provider: provider, Model: "model-child"},
	}
	if selectedRuntime == codingagent.RuntimeCodex {
		rootModel.Spec.OpenAI = &v1alpha3.OpenAIConfig{ReasoningEffort: &effort}
	}
	server := &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "test"},
		Spec: v1alpha3.RemoteMCPServerSpec{
			URL: "https://search.example.com/mcp", Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp,
		},
	}
	child := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "research-template", Namespace: "test", Labels: map[string]string{"runtime": "coding"}},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig: v1alpha3.AgentTemplateLocalReference{Name: childModel.Name}, SystemPrompt: "research",
		},
	}
	root := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "coordinator", Namespace: "test", Labels: map[string]string{"runtime": "coding"}},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig: v1alpha3.AgentTemplateLocalReference{Name: rootModel.Name}, SystemPrompt: "coordinate",
			Tools: []v1alpha3.ToolBinding{
				{MCP: &v1alpha3.MCPToolBinding{Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"search", "lookup"}}},
				{MCP: &v1alpha3.MCPToolBinding{Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"lookup"}}},
				{Agent: &v1alpha3.AgentToolBinding{Name: "researcher", Description: "research the subject", TemplateRef: v1alpha3.AgentTemplateLocalReference{Name: child.Name}}},
			},
			Skills: []v1alpha3.AgentTemplateSkill{{Name: "review", Source: v1alpha3.ArtifactSource{
				OCI: "ghcr.io/acme/review@sha256:" + strings.Repeat("b", 64),
			}}},
			Plugins: []v1alpha3.PluginBundle{{
				Source: v1alpha3.ArtifactSource{Git: &v1alpha3.GitArtifact{
					URL: "https://github.com/acme/plugin", Commit: strings.Repeat("c", 40),
				}},
				Skills: []string{"deploy"},
			}},
		},
	}
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{Name: string(selectedRuntime), Namespace: "test"},
		Spec: v1alpha3.HarnessSpec{
			AllowedAgentTemplates: &v1alpha3.HarnessAgentTemplateAdmission{Selector: metav1.LabelSelector{MatchLabels: map[string]string{"runtime": "coding"}}},
			Workload:              v1alpha3.HarnessWorkload{Image: testImage},
			Substrate: v1alpha3.HarnessSubstratePolicy{
				WorkerPoolRef: corev1.LocalObjectReference{Name: "workers"}, SnapshotPolicy: v1alpha3.HarnessSnapshotPolicy{Location: "snapshots"},
			},
		},
	}
	objects := []client.Object{rootModel, childModel, server, child}
	kube := fake.NewClientBuilder().WithScheme(schemev1.Scheme).WithObjects(objects...).Build()
	reader := testReader{kube}
	compilers := map[translator.HarnessType]translator.HarnessCompiler{}
	if selectedRuntime == codingagent.RuntimeCodex {
		harness.Spec.Codex = &v1alpha3.CodexHarness{}
		compilers[translator.HarnessTypeCodex] = codex.NewCompiler(reader)
	} else {
		harness.Spec.Claude = &v1alpha3.ClaudeHarness{}
		compilers[translator.HarnessTypeClaude] = claude.NewCompiler(reader)
	}
	revision, err := translator.NewCompiler(reader, compilers).CompileAgentTemplate(context.Background(), harness, root)
	require.NoError(t, err)
	return revision
}

func TestCodingAgentCompilersFailClosedOnCredentialAndTLSInputs(t *testing.T) {
	base := func() (*codingagent.Compiler, *translator.HarnessInput) {
		compiler := codex.NewCompiler(nil)
		harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{
			Codex: &v1alpha3.CodexHarness{}, Workload: v1alpha3.HarnessWorkload{Image: testImage},
			Substrate: v1alpha3.HarnessSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "workers"}, SnapshotPolicy: v1alpha3.HarnessSnapshotPolicy{Location: "snapshots"}},
		}}
		model := &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test"}, Spec: v1alpha3.ModelConfigSpec{
			Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5",
		}}
		template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "test"}}
		return compiler, &translator.HarnessInput{Harness: harness, Root: &translator.AgentInput{Template: template, ModelConfig: model}}
	}

	t.Run("Harness credentialRef", func(t *testing.T) {
		compiler, input := base()
		input.Harness.Spec.Env = []v1alpha3.HarnessEnvVar{{Name: "OPENAI_API_KEY", CredentialRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "model-auth"}, Key: "token",
		}}}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "does not support environment variables")
	})

	t.Run("ModelConfig secret", func(t *testing.T) {
		compiler, input := base()
		input.Root.ModelConfig.Spec.APIKeySecret = "model-auth"
		input.Root.ModelConfig.Spec.APIKeySecretKey = "token"
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "do not materialize ModelConfig credentials")
	})

	t.Run("MCP headers", func(t *testing.T) {
		compiler, input := base()
		secretValue := "Bearer must-not-serialize"
		server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test"}, Spec: v1alpha3.RemoteMCPServerSpec{
			URL: "https://mcp.example.com", HeadersFrom: []v1alpha3.ValueRef{{Name: "Authorization", Value: secretValue}},
		}}
		input.Root.MCPTools = []translator.ResolvedMCPTool{{
			Binding: v1alpha3.MCPToolBinding{Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"lookup"}},
			Server:  server,
		}}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "credential relay")
		if revision != nil {
			require.False(t, bytes.Contains(revision.ConfigJSON, []byte(secretValue)))
		}
	})

	t.Run("MCP TLS", func(t *testing.T) {
		compiler, input := base()
		server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test"}, Spec: v1alpha3.RemoteMCPServerSpec{
			URL: "https://mcp.example.com", TLS: &v1alpha3.TLSConfig{DisableVerify: true},
		}}
		input.Root.MCPTools = []translator.ResolvedMCPTool{{
			Binding: v1alpha3.MCPToolBinding{Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"lookup"}},
			Server:  server,
		}}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "trust injection")
	})

	t.Run("provider request options", func(t *testing.T) {
		compiler, input := base()
		input.Root.ModelConfig.Spec.OpenAI = &v1alpha3.OpenAIConfig{BaseURL: "https://gateway.example.com"}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "provider request options are runtime-owned")
	})

	t.Run("mutable workload image", func(t *testing.T) {
		compiler, input := base()
		input.Harness.Spec.Workload.Image = "example.com/coding-agent:latest"
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "requires pinned workload")
	})
}

func TestLiteralHarnessEnvironmentIsRejected(t *testing.T) {
	compiler := codex.NewCompiler(nil)
	literal := "non-secret-runtime-setting"
	harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{
		Codex: &v1alpha3.CodexHarness{}, Workload: v1alpha3.HarnessWorkload{Image: testImage},
		Env:       []v1alpha3.HarnessEnvVar{{Name: "RUNTIME_MODE", Value: &literal}},
		Substrate: v1alpha3.HarnessSubstratePolicy{WorkerPoolRef: corev1.LocalObjectReference{Name: "workers"}, SnapshotPolicy: v1alpha3.HarnessSnapshotPolicy{Location: "snapshots"}},
	}}
	model := &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test"}, Spec: v1alpha3.ModelConfigSpec{
		Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5",
	}}
	template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "test"}}
	revision, err := compiler.Compile(context.Background(), &translator.HarnessInput{
		Harness: harness, Root: &translator.AgentInput{Template: template, ModelConfig: model},
	})
	require.Nil(t, revision)
	require.ErrorContains(t, err, "does not support environment variables")
}
