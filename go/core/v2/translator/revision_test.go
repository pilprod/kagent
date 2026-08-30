package translator

import (
	"strings"
	"testing"
)

func TestRevisionDigestIncludesProvenance(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent", Placement: RevisionPlacementKubernetesPod, SandboxClass: SandboxClassGvisor, Provenance: []byte(`[{"kind":"Secret","hash":"first"}]`)}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.Provenance = []byte(`[{"kind":"Secret","hash":"second"}]`)
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("secret rotation did not change runtime revision")
	}
	if len(first.Short()) != 12 || !strings.HasPrefix(first.String(), first.Short()) {
		t.Fatalf("short revision %q is not a prefix of %q", first.Short(), first.String())
	}
}

func TestRevisionDigestIncludesPrivateMCPPolicy(t *testing.T) {
	revision := &Revision{
		Namespace: "agents", AgentTemplateName: "helper", HarnessName: "codex",
		Placement:    RevisionPlacementExternalSlot,
		SandboxClass: SandboxClassHostProcessHardened,
		MCPPolicy:    MCPPolicyV1{Version: MCPPolicyVersionV1, Bindings: []MCPPolicyBinding{}},
	}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.MCPPolicy.Version = "changed"
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("private MCP policy did not change runtime revision")
	}
}

func TestRevisionDigestExcludesWarnings(t *testing.T) {
	revision := &Revision{
		Namespace: "agents", AgentTemplateName: "helper", HarnessName: "codex",
		Placement:    RevisionPlacementExternalSlot,
		SandboxClass: SandboxClassHostProcessHardened,
	}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.Warnings = []string{"partial MCP selection is not enforced"}
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("non-behavioral warning changed runtime revision")
	}
}

func TestRevisionDigestIncludesPlacement(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "runtime", Placement: RevisionPlacementKubernetesPod, SandboxClass: SandboxClassGvisor}
	kubernetes, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.Placement = RevisionPlacementExternalSlot
	revision.SandboxClass = SandboxClassHostProcessHardened
	external, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if kubernetes == external {
		t.Fatal("placement did not change runtime revision")
	}
}

func TestRevisionDigestIncludesSandboxClass(t *testing.T) {
	revision := &Revision{Placement: RevisionPlacementKubernetesPod, SandboxClass: SandboxClassGvisor}
	gvisor, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.SandboxClass = SandboxClassMicroVM
	microVM, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if gvisor == microVM {
		t.Fatal("sandbox class did not change runtime revision")
	}
}

func TestRevisionDigestRejectsSandboxClassPlacementMismatch(t *testing.T) {
	for _, revision := range []*Revision{
		{Placement: RevisionPlacementKubernetesPod},
		{Placement: RevisionPlacementKubernetesPod, SandboxClass: SandboxClassHostProcessHardened},
		{Placement: RevisionPlacementExternalSlot},
		{Placement: RevisionPlacementExternalSlot, SandboxClass: SandboxClassGvisor},
	} {
		if _, err := revision.Digest(); err == nil {
			t.Fatalf("Digest accepted placement %q with sandbox class %q", revision.Placement, revision.SandboxClass)
		}
	}
}

func TestRevisionDigestRejectsUnknownPlacement(t *testing.T) {
	for _, placement := range []RevisionPlacement{"", "NativeProcess"} {
		revision := &Revision{Placement: placement}
		if _, err := revision.Digest(); err == nil {
			t.Fatalf("Digest accepted placement %q", placement)
		}
	}
}
