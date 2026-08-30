package codingagent_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/v2/agentplugins"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
	clauderuntime "github.com/kagent-dev/kagent/go/harness/claude/config"
	codexruntime "github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// This test crosses the compiler/runtime package boundary: a Revision emitted
// by the real Codex compiler must be consumable directly by the external-host
// adapter translator, without rewriting ConfigJSON in Substrate or the host.
func TestCodexCompilerRevisionMatchesExternalAdapterPortableContract(t *testing.T) {
	unusedServer := &v1alpha3.RemoteMCPServer{
		ObjectMeta: metav1.ObjectMeta{Name: "unused", Namespace: "test", UID: types.UID("unused-server")},
		Spec:       v1alpha3.RemoteMCPServerSpec{URL: "https://unused.example.test/mcp"},
	}
	revision, err := compileMinimalCodex(t, unusedServer, nil, func(template *v1alpha3.AgentTemplate) {
		template.Spec.SystemPrompt = "Stay within the assigned repository."
		template.Spec.Skills = []v1alpha3.AgentTemplateSkill{{Name: "review", Source: v1alpha3.ArtifactSource{
			OCI: "ghcr.io/acme/codex/skills/review@sha256:" + strings.Repeat("a", 64),
		}}}
	})
	require.NoError(t, err)

	translated, err := codexruntime.ParsePortableExternalForRepositoryPrefix(revision.ConfigJSON, "ghcr.io/acme/codex/skills/")
	require.NoError(t, err)
	require.Equal(t, "gpt-5", translated.Model)
	require.Equal(t, "Stay within the assigned repository.", translated.DeveloperInstructions)
	require.Equal(t, "openai", translated.ModelProvider)
	require.Nil(t, translated.Provider)
	require.Equal(t, codexruntime.PinnedCodexVersion, translated.ExpectedCodexVersion)
	require.True(t, translated.StrictVersion)
	require.Equal(t, codexruntime.NetworkRestricted, translated.NetworkAccess)
	require.Len(t, translated.SkillResources.Skills, 1)
	require.Equal(t, "review", translated.SkillResources.Skills[0].Name)
	assertMaterializesCompilerSkill(t, *translated.SkillResources)
}

func TestClaudeCompilerRevisionMatchesExternalAdapterPortableSkillContract(t *testing.T) {
	harness := &v1alpha3.Harness{
		ObjectMeta: metav1.ObjectMeta{Name: "claude", Namespace: "test"},
		Spec: v1alpha3.HarnessSpec{
			Claude:   &v1alpha3.ClaudeHarness{},
			Workload: v1alpha3.HarnessWorkload{Image: "ghcr.io/acme/claude@sha256:" + strings.Repeat("b", 64)},
		},
	}
	template := &v1alpha3.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "claude-conformance", Namespace: "test"},
		Spec: v1alpha3.AgentTemplateSpec{Skills: []v1alpha3.AgentTemplateSkill{{
			Name: "review", Source: v1alpha3.ArtifactSource{OCI: "ghcr.io/acme/claude/skills/review@sha256:" + strings.Repeat("a", 64)},
		}}},
	}
	model := &v1alpha3.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "claude-model", Namespace: "test"},
		Spec:       v1alpha3.ModelConfigSpec{Provider: v1alpha3.ModelProviderAnthropic, Model: "claude-sonnet-4-6"},
	}
	revision, err := codingagent.NewCompiler(codingagent.RuntimeClaude, nil).Compile(context.Background(), &translator.HarnessInput{
		Harness: harness,
		Root:    &translator.AgentInput{Template: template, ModelConfig: model, Instruction: "Stay scoped."},
		MCPPolicy: translator.MCPPolicyV1{
			Version: translator.MCPPolicyVersionV1, Bindings: []translator.MCPPolicyBinding{},
		},
	})
	require.NoError(t, err)

	translated, err := clauderuntime.ParsePortableExternal(revision.ConfigJSON, "ghcr.io/acme/claude/skills/")
	require.NoError(t, err)
	require.Equal(t, "claude-sonnet-4-6", translated.Model)
	require.Equal(t, "Stay scoped.", translated.AppendSystemPrompt)
	require.Len(t, translated.SkillResources.Skills, 1)
	require.Equal(t, "review", translated.SkillResources.Skills[0].Name)
	assertMaterializesCompilerSkill(t, *translated.SkillResources)
}

func assertMaterializesCompilerSkill(t *testing.T, resources agentplugin.Resources) {
	t.Helper()
	root := t.TempDir()
	packageRoot := filepath.Join(root, "packages", "standalone-0")
	require.NoError(t, os.MkdirAll(packageRoot, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(packageRoot, "SKILL.md"), []byte("# Review"), 0o644))
	sourceRaw, err := json.Marshal(resources.Skills[0].Source)
	require.NoError(t, err)
	digest := sha256.Sum256(sourceRaw)
	marker := []byte(fmt.Sprintf(`{"version":1,"digest":"sha256:%x"}`, digest[:]))
	require.NoError(t, os.WriteFile(packageRoot+".source.json", marker, 0o600))
	require.NoError(t, agentplugins.MaterializeSkills(t.Context(), resources, agentplugins.SkillPaths{
		Plugins: filepath.Join(root, "packages"), Skills: filepath.Join(root, "skills"),
	}))
	contents, err := os.ReadFile(filepath.Join(root, "skills", "review", "SKILL.md"))
	require.NoError(t, err)
	require.Equal(t, "# Review", string(contents))
}
