package translator

import (
	"strings"
	"testing"
)

func TestRevisionDigestIncludesProvenance(t *testing.T) {
	revision := &Revision{Namespace: "agents", AgentTemplateName: "helper", HarnessName: "kagent", Provenance: []byte(`[{"kind":"Secret","hash":"first"}]`)}
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
		MCPPolicy: MCPPolicyV1{Version: MCPPolicyVersionV1, Bindings: []MCPPolicyBinding{}},
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
