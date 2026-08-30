package codingagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeRejectsAmbiguousAndUnknownJSON(t *testing.T) {
	valid := `{"version":"v2","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`
	config, err := Decode([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, RuntimeCodex, config.Runtime)

	_, err = Decode([]byte(`{"version":"v2","version":"v2","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`))
	require.ErrorContains(t, err, `duplicate key "version"`)

	_, err = Decode([]byte(`{"version":"v2","runtime":"codex","credential":"secret","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`))
	require.ErrorContains(t, err, "unknown field")

	_, err = Decode([]byte(`{"version":"v2","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"},"mcpGrants":[{"id":"mcp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tools":["lookup"],"url":"https://private.example/mcp","capability":"secret"}]}}`))
	require.ErrorContains(t, err, "unknown field")

	_, err = Decode(append([]byte(valid), []byte(` {}`)...))
	require.ErrorContains(t, err, "more than one JSON value")

	_, err = Decode([]byte{0xff})
	require.ErrorContains(t, err, "valid UTF-8")

	tooDeep := strings.Repeat("[", maxJSONDepth+1) + "0" + strings.Repeat("]", maxJSONDepth+1)
	_, err = Decode([]byte(tooDeep))
	require.ErrorContains(t, err, "maximum JSON depth")
}

func TestConfigValidationPinsRuntimeAndImmutableSources(t *testing.T) {
	config := Config{
		Version: ConfigVersion,
		Runtime: RuntimeCodex,
		Root: AgentConfig{
			TemplateName: "assistant",
			Model:        ModelConfig{Provider: "OpenAI", Name: "gpt-5", ReasoningEffort: "ultra", ServiceTier: "fast"},
			MCPGrants:    []MCPGrant{{ID: "mcp-" + strings.Repeat("a", 64), Tools: []string{"get_issue"}}},
			Skills: []Skill{{Name: "review", Source: ArtifactSource{
				OCI: "ghcr.io/acme/review@sha256:" + strings.Repeat("a", 64),
			}}},
		},
	}
	require.NoError(t, config.Validate())

	mutable := config
	mutable.Root.Skills = []Skill{{Name: "review", Source: ArtifactSource{
		Git: &GitArtifact{URL: "https://github.com/acme/review", Commit: "main"},
	}}}
	require.ErrorContains(t, mutable.Validate(), "full commit")

	duplicateTool := config
	duplicateTool.Root.MCPGrants = append([]MCPGrant(nil), config.Root.MCPGrants...)
	duplicateTool.Root.MCPGrants[0].Tools = []string{"get_issue", "get_issue"}
	require.ErrorContains(t, duplicateTool.Validate(), "sorted and unique")

	duplicateAcrossAgents := config
	duplicateAcrossAgents.Root.SharedAgents = []SharedBinding{{
		Name: "helper", Description: "helper", Agent: AgentConfig{
			TemplateName: "helper", Model: ModelConfig{Provider: "OpenAI", Name: "gpt-5"},
			MCPGrants: []MCPGrant{{ID: config.Root.MCPGrants[0].ID, Tools: []string{"get_issue"}}},
		},
	}}
	require.ErrorContains(t, duplicateAcrossAgents.Validate(), "duplicates a grant owned by agent")

	claude := config
	claude.Runtime = RuntimeClaude
	claude.Root.Model.Provider = "Anthropic"
	require.ErrorContains(t, claude.Validate(), "reasoningEffort")

	invalidTier := config
	invalidTier.Root.Model.ServiceTier = "slow"
	require.ErrorContains(t, invalidTier.Validate(), "serviceTier")

	unsafeBinding := config
	unsafeBinding.Root.SharedAgents = []SharedBinding{{
		Name: "../escape", Description: "unsafe", Agent: AgentConfig{
			TemplateName: "child", Model: ModelConfig{Provider: "OpenAI", Name: "gpt-5"},
		},
	}}
	require.ErrorContains(t, unsafeBinding.Validate(), "safe runtime name")

	unsafeModel := config
	unsafeModel.Root.Model.Name = "gpt-5\x00override"
	require.ErrorContains(t, unsafeModel.Validate(), "control or whitespace")

	unnormalizedPath := config
	unnormalizedPath.Root.Skills = []Skill{{Name: "review", Source: ArtifactSource{
		OCI: "ghcr.io/acme/review@sha256:" + strings.Repeat("a", 64), Path: "skills//review",
	}}}
	require.ErrorContains(t, unnormalizedPath.Validate(), "path must be canonical")
}

func TestConfigValidationBoundsRuntimeIdentifiers(t *testing.T) {
	base := Config{Version: ConfigVersion, Runtime: RuntimeCodex, Root: AgentConfig{
		TemplateName: "assistant", Model: ModelConfig{Provider: "OpenAI", Name: "gpt-5"},
	}}
	for _, test := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "template", mutate: func(config *Config) { config.Root.TemplateName = strings.Repeat("a", 254) }, want: "valid Kubernetes object name"},
		{name: "model", mutate: func(config *Config) { config.Root.Model.Name = strings.Repeat("m", 257) }, want: "between 1 and 256 bytes"},
		{name: "MCP grant", mutate: func(config *Config) {
			config.Root.MCPGrants = []MCPGrant{{ID: "mcp-not-a-digest", Tools: []string{"lookup"}}}
		}, want: "MCP grant ID"},
		{name: "MCP tool", mutate: func(config *Config) {
			config.Root.MCPGrants = []MCPGrant{{ID: "mcp-" + strings.Repeat("a", 64), Tools: []string{strings.Repeat("t", 129)}}}
		}, want: "between 1 and 128 bytes"},
		{name: "Shared binding", mutate: func(config *Config) {
			config.Root.SharedAgents = []SharedBinding{{
				Name: strings.Repeat("b", 129), Description: "child", Agent: AgentConfig{
					TemplateName: "child", Model: ModelConfig{Provider: "OpenAI", Name: "gpt-5"},
				},
			}}
		}, want: "safe runtime name"},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			require.ErrorContains(t, config.Validate(), test.want)
		})
	}
}

func TestDecodeRequiresCanonicalOrdering(t *testing.T) {
	unsorted := `{"version":"v2","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"},"mcpGrants":[{"id":"mcp-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","tools":["z","a"]},{"id":"mcp-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","tools":["a"]}]}}`
	_, err := Decode([]byte(unsorted))
	require.ErrorContains(t, err, "sorted and unique")

	whitespace := `{"version": "v2","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`
	_, err = Decode([]byte(whitespace))
	require.ErrorContains(t, err, "not canonical JSON")

	unsortedSkills := Config{Version: ConfigVersion, Runtime: RuntimeCodex, Root: AgentConfig{
		TemplateName: "assistant", Model: ModelConfig{Provider: "OpenAI", Name: "gpt-5"},
		Skills: []Skill{
			{Name: "zeta", Source: ArtifactSource{OCI: "ghcr.io/acme/zeta@sha256:" + strings.Repeat("a", 64)}},
			{Name: "alpha", Source: ArtifactSource{OCI: "ghcr.io/acme/alpha@sha256:" + strings.Repeat("b", 64)}},
		},
	}}
	require.ErrorContains(t, unsortedSkills.Validate(), "standalone skills must be sorted and unique")
}

func TestGeneratedConfigStaysBelowEnvironmentLimit(t *testing.T) {
	config := Config{Version: ConfigVersion, Runtime: RuntimeCodex, Root: AgentConfig{
		TemplateName: "assistant", Instruction: strings.Repeat("x", MaxConfigBytes),
		Model: ModelConfig{Provider: "OpenAI", Name: "gpt-5"},
	}}
	raw, err := json.Marshal(config)
	require.NoError(t, err)
	require.Greater(t, len(raw), MaxConfigBytes)
	_, err = Decode(raw)
	require.ErrorContains(t, err, "config size")
}
