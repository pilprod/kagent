package codingagent_test

import (
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
				require.Equal(t, "fast", config.Root.Model.ServiceTier)
			} else {
				require.Empty(t, config.Root.Model.ReasoningEffort)
				require.Empty(t, config.Root.Model.ServiceTier)
			}
			require.Len(t, config.Root.MCPGrants, 2)
			policyByID := map[string]translator.MCPPolicyBinding{}
			for _, binding := range revision.MCPPolicy.Bindings {
				policyByID[binding.ID] = binding
			}
			for _, grant := range config.Root.MCPGrants {
				binding, found := policyByID[grant.ID]
				require.True(t, found)
				require.Equal(t, binding.Tools, grant.Tools)
				require.Equal(t, []string{"root"}, binding.SubjectPath)
			}
			require.Equal(t, "review", config.Root.Skills[0].Name)
			require.Equal(t, "deploy", config.Root.Plugins[0].Skills[0])
			require.Len(t, config.Root.SharedAgents, 1)
			require.Equal(t, "researcher", config.Root.SharedAgents[0].Name)
			require.Equal(t, "research-template", config.Root.SharedAgents[0].Agent.TemplateName)
			require.Len(t, config.Root.SharedAgents[0].Agent.MCPGrants, 1)
			sharedGrant := config.Root.SharedAgents[0].Agent.MCPGrants[0]
			require.Equal(t, []string{"lookup"}, sharedGrant.Tools)
			require.Equal(t, []string{"root", "researcher"}, policyByID[sharedGrant.ID].SubjectPath)
			for _, host := range []string{"ghcr.io", "github.com"} {
				require.Truef(t, slices.Contains(revision.EgressDestinations, host), "egress %v lacks %q", revision.EgressDestinations, host)
			}
			require.NotContains(t, revision.EgressDestinations, "search.example.com")
			providerHost := "api.openai.com"
			if test.runtime == codingagent.RuntimeClaude {
				providerHost = "api.anthropic.com"
			} else {
				require.Contains(t, revision.EgressDestinations, "auth.openai.com")
				require.Contains(t, revision.EgressDestinations, "chatgpt.com")
			}
			require.Contains(t, revision.EgressDestinations, providerHost)
			require.Contains(t, string(revision.Provenance), `"kind":"RemoteMCPServer"`)
			for _, forbidden := range []string{"https://search.example.com/mcp", "Bearer must-not-serialize", "Authorization", "connection", "transport"} {
				require.NotContains(t, string(revision.ConfigJSON), forbidden)
				require.NotContains(t, string(revision.AgentCardJSON), forbidden)
				require.NotContains(t, string(revision.Provenance), forbidden)
			}
			require.Equal(t, testImage, revision.Image)
			require.Equal(t, translator.RevisionPlacementExternalSlot, revision.Placement)
			require.Empty(t, revision.WorkerPoolName)
			require.Empty(t, revision.SnapshotLocation)
			require.NotContains(t, string(revision.AgentCardJSON), `"streaming":true`)
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
		responses := v1alpha3.OpenAIAPIFormatResponses
		fast := v1alpha3.OpenAIServiceTierFast
		rootModel.Spec.OpenAI = &v1alpha3.OpenAIConfig{
			APIFormat: &responses, ReasoningEffort: &effort, ServiceTier: &fast,
		}
	}
	server := &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "search", Namespace: "test", UID: types.UID("uid-search")},
		Spec: v1alpha3.RemoteMCPServerSpec{
			URL: "https://search.example.com/mcp", Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp,
			HeadersFrom: []v1alpha3.ValueRef{{Name: "Authorization", Value: "Bearer must-not-serialize"}},
		},
	}
	child := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "research-template", Namespace: "test", Labels: map[string]string{"runtime": "coding"}},
		Spec: v1alpha3.AgentTemplateSpec{
			ModelConfig: v1alpha3.AgentTemplateLocalReference{Name: childModel.Name}, SystemPrompt: "research",
			Tools: []v1alpha3.ToolBinding{{MCP: &v1alpha3.MCPToolBinding{
				Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"lookup"},
			}}},
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

func TestCodingAgentCompilersFailClosedOnUnauthorizedInputs(t *testing.T) {
	base := func() (*codingagent.Compiler, *translator.HarnessInput) {
		compiler := codex.NewCompiler(nil)
		harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{
			Codex: &v1alpha3.CodexHarness{}, Workload: v1alpha3.HarnessWorkload{Image: testImage},
		}}
		model := &v1alpha3.ModelConfig{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test"}, Spec: v1alpha3.ModelConfigSpec{
			Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5",
		}}
		template := &v1alpha3.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "test"}}
		return compiler, &translator.HarnessInput{
			Harness: harness,
			Root:    &translator.AgentInput{Template: template, ModelConfig: model},
			MCPPolicy: translator.MCPPolicyV1{
				Version: translator.MCPPolicyVersionV1, Bindings: []translator.MCPPolicyBinding{},
			},
		}
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

	t.Run("Kubernetes Substrate policy", func(t *testing.T) {
		compiler, input := base()
		input.Harness.Spec.Substrate = &v1alpha3.HarnessSubstratePolicy{
			WorkerPoolRef:  corev1.LocalObjectReference{Name: "workers"},
			SnapshotPolicy: v1alpha3.HarnessSnapshotPolicy{Location: "snapshots"},
		}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "does not accept Kubernetes Substrate policy")
	})

	t.Run("ModelConfig secret", func(t *testing.T) {
		compiler, input := base()
		input.Root.ModelConfig.Spec.APIKeySecret = "model-auth"
		input.Root.ModelConfig.Spec.APIKeySecretKey = "token"
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "do not materialize ModelConfig credentials")
	})

	t.Run("MCP tools without exact private policy", func(t *testing.T) {
		compiler, input := base()
		server := &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test"}, Spec: v1alpha3.RemoteMCPServerSpec{
			URL: "https://mcp.example.com",
		}}
		input.Root.MCPTools = []translator.ResolvedMCPTool{{
			Binding: v1alpha3.MCPToolBinding{Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name}, Tools: []string{"lookup"}},
			Server:  server,
		}}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "no exact private relay grant")
	})

	t.Run("provider request options", func(t *testing.T) {
		compiler, input := base()
		input.Root.ModelConfig.Spec.OpenAI = &v1alpha3.OpenAIConfig{BaseURL: "https://gateway.example.com"}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "provider request options are runtime-owned")
	})

	t.Run("service tier without Responses API", func(t *testing.T) {
		compiler, input := base()
		fast := v1alpha3.OpenAIServiceTierFast
		input.Root.ModelConfig.Spec.OpenAI = &v1alpha3.OpenAIConfig{ServiceTier: &fast}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "serviceTier requires OpenAI apiFormat")
	})

	t.Run("unknown service tier", func(t *testing.T) {
		compiler, input := base()
		responses := v1alpha3.OpenAIAPIFormatResponses
		unknown := v1alpha3.OpenAIServiceTier("slow")
		input.Root.ModelConfig.Spec.OpenAI = &v1alpha3.OpenAIConfig{APIFormat: &responses, ServiceTier: &unknown}
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "invalid serviceTier")
	})

	t.Run("mutable workload image", func(t *testing.T) {
		compiler, input := base()
		input.Harness.Spec.Workload.Image = "example.com/coding-agent:latest"
		revision, err := compiler.Compile(context.Background(), input)
		require.Nil(t, revision)
		require.ErrorContains(t, err, "requires pinned workload")
	})
}

func TestCodingAgentCompilerRejectsDuplicateExactMCPBinding(t *testing.T) {
	server := &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
		Spec:       v1alpha3.RemoteMCPServerSpec{URL: "https://mcp.example.com", Protocol: v1alpha3.RemoteMCPServerProtocolStreamableHttp},
	}
	binding := v1alpha3.MCPToolBinding{
		Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: server.Name},
		Tools:  []string{"lookup"},
	}
	_, err := compileMinimalCodex(t, server, []v1alpha3.MCPToolBinding{binding, binding}, nil)
	require.ErrorContains(t, err, "repeats exact private relay grant")
}

func TestCodingAgentCompilerRejectsUnenforceableMCPUpstreams(t *testing.T) {
	for _, test := range []struct {
		name   string
		server *v1alpha3.RemoteMCPServer
		mutate func(*v1alpha3.AgentTemplate)
		want   string
	}{
		{
			name: "invalid URL",
			server: &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
				Spec: v1alpha3.RemoteMCPServerSpec{URL: "file:///tmp/mcp"}},
			want: "must be an absolute HTTP(S) URL",
		},
		{
			name: "unsupported TLS",
			server: &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
				Spec: v1alpha3.RemoteMCPServerSpec{URL: "https://mcp.example.com", TLS: &v1alpha3.TLSConfig{DisableVerify: true}}},
			want: "TLS is unsupported by the private relay",
		},
		{
			name: "model egress collision",
			server: &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
				Spec: v1alpha3.RemoteMCPServerSpec{URL: "https://API.OPENAI.COM/mcp"}},
			want: "overlaps direct runtime egress",
		},
		{
			name: "root dot and port model egress collision",
			server: &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
				Spec: v1alpha3.RemoteMCPServerSpec{URL: "https://API.OPENAI.COM.:8443/mcp"}},
			want: "overlaps direct runtime egress",
		},
		{
			name: "non ASCII hostname",
			server: &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
				Spec: v1alpha3.RemoteMCPServerSpec{URL: "https://mcp.exämple.com/mcp"}},
			want: "must contain only ASCII characters",
		},
		{
			name: "artifact egress collision",
			server: &v1alpha3.RemoteMCPServer{ObjectMeta: metav1.ObjectMeta{Name: "private", Namespace: "test", UID: types.UID("uid-private")},
				Spec: v1alpha3.RemoteMCPServerSpec{URL: "https://github.com/private/mcp"}},
			mutate: func(root *v1alpha3.AgentTemplate) {
				root.Spec.Skills = []v1alpha3.AgentTemplateSkill{{Name: "review", Source: v1alpha3.ArtifactSource{
					Git: &v1alpha3.GitArtifact{URL: "https://github.com/acme/review", Commit: strings.Repeat("a", 40)},
				}}}
			},
			want: "overlaps direct runtime egress",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			binding := v1alpha3.MCPToolBinding{
				Server: v1alpha3.AgentTemplateTypedLocalReference{Kind: "RemoteMCPServer", Name: test.server.Name},
				Tools:  []string{"lookup"},
			}
			_, err := compileMinimalCodex(t, test.server, []v1alpha3.MCPToolBinding{binding}, test.mutate)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func compileMinimalCodex(t *testing.T, server *v1alpha3.RemoteMCPServer, bindings []v1alpha3.MCPToolBinding, mutate func(*v1alpha3.AgentTemplate)) (*translator.Revision, error) {
	t.Helper()
	require.NoError(t, v1alpha3.AddToScheme(schemev1.Scheme))
	model := &v1alpha3.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test"},
		Spec:       v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderOpenAI, Model: "gpt-5"},
	}
	root := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "assistant", Namespace: "test", Labels: map[string]string{"runtime": "coding"}},
		Spec:       v1alpha3.AgentTemplateSpec{ModelConfig: v1alpha3.AgentTemplateLocalReference{Name: model.Name}},
	}
	for index := range bindings {
		binding := bindings[index]
		root.Spec.Tools = append(root.Spec.Tools, v1alpha3.ToolBinding{MCP: &binding})
	}
	if mutate != nil {
		mutate(root)
	}
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: "test"},
		Spec: v1alpha3.HarnessSpec{
			Codex: &v1alpha3.CodexHarness{},
			AllowedAgentTemplates: &v1alpha3.HarnessAgentTemplateAdmission{
				Selector: metav1.LabelSelector{MatchLabels: map[string]string{"runtime": "coding"}},
			},
			Workload: v1alpha3.HarnessWorkload{Image: testImage},
		},
	}
	objects := []client.Object{model, server}
	kube := fake.NewClientBuilder().WithScheme(schemev1.Scheme).WithObjects(objects...).Build()
	reader := testReader{kube}
	return translator.NewCompiler(reader, map[translator.HarnessType]translator.HarnessCompiler{
		translator.HarnessTypeCodex: codex.NewCompiler(reader),
	}).CompileAgentTemplate(context.Background(), harness, root)
}

func TestLiteralHarnessEnvironmentIsRejected(t *testing.T) {
	compiler := codex.NewCompiler(nil)
	literal := "non-secret-runtime-setting"
	harness := &v1alpha3.Harness{ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: "test"}, Spec: v1alpha3.HarnessSpec{
		Codex: &v1alpha3.CodexHarness{}, Workload: v1alpha3.HarnessWorkload{Image: testImage},
		Env: []v1alpha3.HarnessEnvVar{{Name: "RUNTIME_MODE", Value: &literal}},
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
