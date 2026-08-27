package translator

import (
	"encoding/json"
	"strings"
	"testing"
)

func testMCPPolicyV1(t *testing.T) MCPPolicyV1 {
	t.Helper()
	binding := MCPPolicyBinding{
		SubjectPath: []string{"root"},
		Server: MCPServerIdentity{
			Namespace: "agents",
			Name:      "knowledge",
			UID:       "server-uid",
			SpecHash:  strings.Repeat("a", 64),
		},
		Tools: []string{"search"},
	}
	var err error
	binding.ID, err = mcpBindingID(binding)
	if err != nil {
		t.Fatal(err)
	}
	return MCPPolicyV1{Version: MCPPolicyVersionV1, Bindings: []MCPPolicyBinding{binding}}
}

func TestDecodeMCPPolicyV1StrictAndCanonical(t *testing.T) {
	policy := testMCPPolicyV1(t)
	raw, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	decoded, err := DecodeMCPPolicyV1(raw)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bindings[0].ID != policy.Bindings[0].ID {
		t.Fatalf("decoded binding = %q, want %q", decoded.Bindings[0].ID, policy.Bindings[0].ID)
	}
	canonical, err := CanonicalMCPPolicyJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(want) {
		t.Fatalf("canonical policy = %s, want %s", canonical, want)
	}
}

func TestDecodeMCPPolicyV1RejectsNonStrictJSON(t *testing.T) {
	valid, err := json.Marshal(testMCPPolicyV1(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		raw  string
	}{
		{name: "duplicate field", raw: `{"version":"v1","version":"v1","bindings":[]}`},
		{name: "unknown field", raw: `{"version":"v1","bindings":[],"connectionURL":"https://cluster.internal"}`},
		{name: "trailing value", raw: string(valid) + ` {}`},
		{name: "non canonical nil bindings", raw: `{"version":"v1","bindings":null}`},
		{name: "oversized", raw: strings.Repeat(" ", maxMCPPolicyJSONBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeMCPPolicyV1([]byte(test.raw)); err == nil {
				t.Fatal("DecodeMCPPolicyV1() succeeded")
			}
		})
	}
}
