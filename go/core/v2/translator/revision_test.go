package translator

import (
	"strings"
	"testing"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
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

func TestRevisionDigestIncludesBackendIdentity(t *testing.T) {
	revision := &Revision{
		Namespace: "agents", AgentTemplateName: "helper", HarnessName: "codex",
		BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex,
		ExternalProfile: []byte(`{"version":"v1"}`),
	}
	first, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	revision.ExternalRuntime = dbpkg.ExternalRuntimeClaude
	second, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("external runtime did not change runtime revision")
	}
	revision.ExternalRuntime = dbpkg.ExternalRuntimeCodex
	revision.ExternalProfile = []byte(`{"version":"v1","instruction":"help"}`)
	third, err := revision.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("external profile did not change runtime revision")
	}
}
