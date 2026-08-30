package codingagent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/kagent-dev/kagent/go/core/v2/translator/claude"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codex"
	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestExternalSlotTestbedBundleRendersAndCompiles(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	renderer := filepath.Join(repositoryRoot, "examples", "external-slot-testbed", "render.sh")
	evidence := filepath.Join(repositoryRoot, "examples", "external-slot-testbed", "testdata", "release-evidence.json")

	for _, test := range []struct {
		name          string
		evidence      string
		provider      v1alpha3.ModelProvider
		image         string
		newCompiler   func(translator.Reader) translator.HarnessCompiler
		configRuntime codingagent.Runtime
	}{
		{
			name: "codex", evidence: "release-evidence.json", provider: v1alpha3.ModelProviderOpenAI,
			image:         "ghcr.io/example/kagent/codex-harness@sha256:" + strings.Repeat("a", 64),
			newCompiler:   func(reader translator.Reader) translator.HarnessCompiler { return codex.NewCompiler(reader) },
			configRuntime: codingagent.RuntimeCodex,
		},
		{
			name: "claude", evidence: "claude-release-evidence.json", provider: v1alpha3.ModelProviderAnthropic,
			image:         "ghcr.io/example/kagent/claude-harness@sha256:" + strings.Repeat("b", 64),
			newCompiler:   func(reader translator.Reader) translator.HarnessCompiler { return claude.NewCompiler(reader) },
			configRuntime: codingagent.RuntimeClaude,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command(renderer, test.name, filepath.Join(filepath.Dir(evidence), test.evidence))
			rendered, err := command.CombinedOutput()
			require.NoError(t, err, string(rendered))
			require.NotContains(t, string(rendered), "@@")
			require.NotContains(t, strings.ToLower(string(rendered)), "secret")
			require.NotContains(t, string(rendered), "substrate:")

			harness, model, template := decodeTestbedObjects(t, rendered)
			require.Equal(t, test.image, harness.Spec.Workload.Image)
			require.Empty(t, harness.Spec.Env)
			require.Nil(t, harness.Spec.Substrate)
			require.Equal(t, test.provider, model.Spec.Provider)
			require.Empty(t, model.Spec.APIKeySecret)
			require.Empty(t, model.Spec.APIKeySecretKey)
			require.False(t, model.Spec.APIKeyPassthrough)

			scheme := k8sruntime.NewScheme()
			require.NoError(t, v1alpha3.AddToScheme(scheme))
			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(model).Build()
			reader := bundleReader{kube}
			harnessType := translator.HarnessType(test.name)
			revision, err := translator.NewCompiler(reader, map[translator.HarnessType]translator.HarnessCompiler{
				harnessType: test.newCompiler(reader),
			}).CompileAgentTemplate(context.Background(), harness, template)
			require.NoError(t, err)
			require.Equal(t, translator.RevisionPlacementExternalSlot, revision.Placement)
			require.Equal(t, translator.SandboxClassHostProcessHardened, revision.SandboxClass)
			require.Empty(t, revision.WorkerPoolName)
			require.Empty(t, revision.SnapshotLocation)
			require.Equal(t, test.image, revision.Image)
			_, err = revision.Digest()
			require.NoError(t, err)

			config, err := codingagent.Decode(revision.ConfigJSON)
			require.NoError(t, err)
			require.Equal(t, test.configRuntime, config.Runtime)
			require.Equal(t, string(test.provider), config.Root.Model.Provider)
			require.Equal(t, model.Spec.Model, config.Root.Model.Name)
		})
	}
}

func TestExternalSlotTestbedStandardEvidenceKeepsClaudeTermsGateClosed(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	renderer := filepath.Join(repositoryRoot, "examples", "external-slot-testbed", "render.sh")
	evidence := filepath.Join(repositoryRoot, "examples", "external-slot-testbed", "testdata", "release-evidence.json")
	command := exec.Command(renderer, "claude", evidence)
	output, err := command.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "does not contain runtime_images.claudeHarness")
}

func TestExternalSlotTestbedEvidenceSeparatesDeploymentAndRuntimeImages(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	raw, err := os.ReadFile(filepath.Join(
		repositoryRoot,
		"examples",
		"external-slot-testbed",
		"testdata",
		"release-evidence.json",
	))
	require.NoError(t, err)

	var evidence struct {
		SchemaVersion    int    `json:"schemaVersion"`
		SourceRepository string `json:"source_repository"`
		SourceCommit     string `json:"source_commit"`
		ChartSource      struct {
			Path                    string `json:"path"`
			Tree                    string `json:"tree"`
			SkillsInitRemovalCommit string `json:"skills_init_removal_commit"`
		} `json:"chart_source"`
		ImageRefs     map[string]string `json:"image_refs"`
		RuntimeImages map[string]string `json:"runtime_images"`
		Charts        map[string]struct {
			Ref     string `json:"ref"`
			Version string `json:"version"`
		} `json:"charts"`
	}
	require.NoError(t, json.Unmarshal(raw, &evidence))
	require.Equal(t, 3, evidence.SchemaVersion)
	require.Equal(t, "https://github.com/pilprod/kagent", evidence.SourceRepository)
	require.Regexp(t, `^[0-9a-f]{40}$`, evidence.SourceCommit)
	require.Equal(t, "helm/kagent", evidence.ChartSource.Path)
	require.Regexp(t, `^[0-9a-f]{40}$`, evidence.ChartSource.Tree)
	require.Equal(t, "059c01b68584dea113ccdf80f2e356c2d051e02a", evidence.ChartSource.SkillsInitRemovalCommit)
	require.Len(t, evidence.ImageRefs, 2)
	for _, key := range []string{"controller", "ui"} {
		require.Regexp(t, `^[^[:space:]@]+@sha256:[0-9a-f]{64}$`, evidence.ImageRefs[key])
	}
	require.NotContains(t, evidence.ImageRefs, "codexHarness")
	require.Len(t, evidence.RuntimeImages, 2)
	require.Regexp(t, `^[^[:space:]@]+@sha256:[0-9a-f]{64}$`, evidence.RuntimeImages["kagentHarness"])
	require.Regexp(t, `^[^[:space:]@]+@sha256:[0-9a-f]{64}$`, evidence.RuntimeImages["codexHarness"])
	require.NotContains(t, evidence.RuntimeImages, "claudeHarness")
	require.Len(t, evidence.Charts, 2)
	for _, key := range []string{"application", "crds"} {
		require.Regexp(t, `^oci://[^[:space:]@]+@sha256:[0-9a-f]{64}$`, evidence.Charts[key].Ref)
		require.Regexp(t, `^[0-9]+\.[0-9]+\.[0-9]+-[0-9A-Za-z.-]+\.kap\.[0-9]+$`, evidence.Charts[key].Version)
	}
}

func TestExternalSlotTestbedRendererRejectsUnpinnedOrMissingImages(t *testing.T) {
	repositoryRoot := testRepositoryRoot(t)
	renderer := filepath.Join(repositoryRoot, "examples", "external-slot-testbed", "render.sh")
	for _, test := range []struct {
		name     string
		evidence string
		want     string
	}{
		{
			name:     "mutable tag",
			evidence: `{"schemaVersion":3,"runtime_images":{"codexHarness":"ghcr.io/example/kagent/codex-harness:latest"}}`,
			want:     "is not digest-qualified",
		},
		{
			name:     "missing image",
			evidence: `{"schemaVersion":3,"runtime_images":{}}`,
			want:     "does not contain runtime_images.codexHarness",
		},
		{
			name:     "legacy schema",
			evidence: `{"schemaVersion":2,"runtime_images":{"codexHarness":"ghcr.io/example/kagent/codex-harness@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}`,
			want:     "must use schemaVersion 3",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			evidence := filepath.Join(t.TempDir(), "release-evidence.json")
			require.NoError(t, os.WriteFile(evidence, []byte(test.evidence), 0o600))
			command := exec.Command(renderer, "codex", evidence)
			output, err := command.CombinedOutput()
			require.Error(t, err)
			require.Contains(t, string(output), test.want)
		})
	}
}

type bundleReader struct{ client.Client }

var _ translator.Reader = bundleReader{}

func (r bundleReader) Get(ctx context.Context, key client.ObjectKey, object k8sruntime.Object) error {
	return r.Client.Get(ctx, key, object.(client.Object))
}

func testRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := stdruntime.Caller(0)
	require.True(t, ok)
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "../../../../../"))
}

func decodeTestbedObjects(t *testing.T, rendered []byte) (*v1alpha3.Harness, *v1alpha3.ModelConfig, *v1alpha3.AgentTemplate) {
	t.Helper()
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(rendered), 4096)
	var harness *v1alpha3.Harness
	var model *v1alpha3.ModelConfig
	var template *v1alpha3.AgentTemplate
	count := 0
	for {
		var object unstructured.Unstructured
		err := decoder.Decode(&object)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if len(object.Object) == 0 {
			continue
		}
		count++
		raw, err := json.Marshal(object.Object)
		require.NoError(t, err)
		switch object.GroupVersionKind() {
		case schema.GroupVersionKind{Group: v1alpha3.GroupVersion.Group, Version: v1alpha3.GroupVersion.Version, Kind: "Harness"}:
			harness = &v1alpha3.Harness{}
			decodeStrictJSON(t, raw, harness)
		case schema.GroupVersionKind{Group: v1alpha3.GroupVersion.Group, Version: v1alpha3.GroupVersion.Version, Kind: "ModelConfig"}:
			model = &v1alpha3.ModelConfig{}
			decodeStrictJSON(t, raw, model)
		case schema.GroupVersionKind{Group: v1alpha3.GroupVersion.Group, Version: v1alpha3.GroupVersion.Version, Kind: "AgentTemplate"}:
			template = &v1alpha3.AgentTemplate{}
			decodeStrictJSON(t, raw, template)
		default:
			t.Fatalf("unexpected testbed object %s", object.GroupVersionKind())
		}
	}
	require.Equal(t, 3, count)
	require.NotNil(t, harness)
	require.NotNil(t, model)
	require.NotNil(t, template)
	return harness, model, template
}

func decodeStrictJSON(t *testing.T, raw []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(target))
	require.ErrorIs(t, decoder.Decode(&struct{}{}), io.EOF)
}
