package substrate

import (
	"bytes"
	"encoding/json"
	"testing"

	atev1alpha1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	corev1 "k8s.io/api/core/v1"
)

func TestActorTemplateForRevision(t *testing.T) {
	spec := &translator.Revision{
		Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent",
		Image:          "agent.example/image@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Placement:      translator.RevisionPlacementKubernetesPod,
		WorkerPoolName: "default", SnapshotLocation: "snapshots",
		ConfigJSON: []byte(`{"instruction":"help"}`), AgentCardJSON: []byte(`{"name":"helper"}`),
		Environment: []corev1.EnvVar{{Name: "API_KEY", Value: "secret"}},
		MCPPolicy: translator.MCPPolicyV1{Version: translator.MCPPolicyVersionV1, Bindings: []translator.MCPPolicyBinding{{
			ID: "private-policy-marker",
		}}},
	}
	revisionID, err := spec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	template, err := ActorTemplateForRevision(spec, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if template.Name != "helper-kagent-"+revisionID.Short() {
		t.Fatalf("ActorTemplate = %+v", template)
	}
	container := template.Spec.Containers[0]
	if template.Spec.SandboxClass != atev1alpha1.SandboxClassGvisor || container.Readyz.HTTPGet.Path != "/readyz" || container.Readyz.HTTPGet.Port != 8081 || container.Readyz.TimeoutSeconds != 30 {
		t.Fatalf("unexpected runtime contract: %+v", template.Spec)
	}
	if template.Spec.WorkerProvider != atev1alpha1.WorkerProviderKubernetesPod || template.Spec.WorkerSelector == nil || len(template.Spec.Volumes) != 1 || len(container.VolumeMounts) != 1 {
		t.Fatalf("unexpected KubernetesPod placement contract: %+v", template.Spec)
	}
	if template.Spec.SnapshotsConfig.OnResume.FromData != atev1alpha1.ResumeSourceColdBoot {
		t.Fatalf("unexpected snapshot resume default: %+v", template.Spec.SnapshotsConfig.OnResume)
	}
	environment := map[string]atev1alpha1.EnvVar{}
	for _, variable := range container.Env {
		environment[variable.Name] = variable
	}
	if environment["KAGENT_CONFIG_JSON"].Value != string(spec.ConfigJSON) {
		t.Fatal("config was not embedded as a non-secret literal")
	}
	raw, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-policy-marker")) {
		t.Fatal("private MCP policy was materialized into the ActorTemplate")
	}
}

func TestActorTemplateForExternalSlotRevision(t *testing.T) {
	spec := &translator.Revision{
		Namespace: "agents", AgentTemplateName: "helper", HarnessName: "codex",
		Image:         "agent.example/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Placement:     translator.RevisionPlacementExternalSlot,
		ConfigJSON:    []byte(`{"runtime":"codex"}`),
		AgentCardJSON: []byte(`{"name":"helper"}`),
		Environment:   []corev1.EnvVar{{Name: "LITERAL", Value: "value"}},
	}
	revisionID, err := spec.Digest()
	if err != nil {
		t.Fatal(err)
	}
	template, err := ActorTemplateForRevision(spec, revisionID)
	if err != nil {
		t.Fatal(err)
	}
	if template.Spec.WorkerProvider != atev1alpha1.WorkerProviderExternalSlot {
		t.Fatalf("worker provider = %q", template.Spec.WorkerProvider)
	}
	if template.Spec.WorkerSelector != nil || len(template.Spec.Volumes) != 0 || template.Spec.SnapshotsConfig.Location != "" {
		t.Fatalf("external template contains Kubernetes-only placement state: %+v", template.Spec)
	}
	if template.Spec.SandboxClass != "" || len(template.Spec.Containers) != 1 || len(template.Spec.Containers[0].VolumeMounts) != 0 {
		t.Fatalf("external template contains sandbox or volume mounts: %+v", template.Spec)
	}
	environment := map[string]string{}
	for _, variable := range template.Spec.Containers[0].Env {
		environment[variable.Name] = variable.Value
	}
	if environment["LITERAL"] != "value" || environment["KAGENT_CONFIG_JSON"] != string(spec.ConfigJSON) || environment["KAGENT_AGENT_CARD_JSON"] != string(spec.AgentCardJSON) {
		t.Fatalf("external runtime bundle was not retained: %+v", environment)
	}
}
