package codingagent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeRejectsAmbiguousAndUnknownJSON(t *testing.T) {
	valid := `{"version":"v1","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`
	config, err := Decode([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, RuntimeCodex, config.Runtime)

	_, err = Decode([]byte(`{"version":"v1","version":"v2","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`))
	require.ErrorContains(t, err, `duplicate key "version"`)

	_, err = Decode([]byte(`{"version":"v1","runtime":"codex","credential":"secret","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`))
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
			Model:        ModelConfig{Provider: "OpenAI", Name: "gpt-5", ReasoningEffort: "high"},
			MCPServers: []MCPBinding{{
				Server: "github", Tools: []string{"get_issue"},
				Connection: MCPConnection{Transport: "STREAMABLE_HTTP", URL: "https://mcp.example.com/mcp"},
			}},
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
	duplicateTool.Root.MCPServers = append([]MCPBinding(nil), config.Root.MCPServers...)
	duplicateTool.Root.MCPServers[0].Tools = []string{"get_issue", "get_issue"}
	require.ErrorContains(t, duplicateTool.Validate(), "sorted and unique")

	claude := config
	claude.Runtime = RuntimeClaude
	claude.Root.Model.Provider = "Anthropic"
	require.ErrorContains(t, claude.Validate(), "reasoningEffort")

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
		{name: "MCP server", mutate: func(config *Config) {
			config.Root.MCPServers = []MCPBinding{{
				Server: strings.Repeat("s", 254), Tools: []string{"lookup"},
				Connection: MCPConnection{Transport: "STREAMABLE_HTTP", URL: "https://mcp.example.com"},
			}}
		}, want: "valid Kubernetes object name"},
		{name: "MCP tool", mutate: func(config *Config) {
			config.Root.MCPServers = []MCPBinding{{
				Server: "search", Tools: []string{strings.Repeat("t", 257)},
				Connection: MCPConnection{Transport: "STREAMABLE_HTTP", URL: "https://mcp.example.com"},
			}}
		}, want: "between 1 and 256 bytes"},
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
	unsorted := `{"version":"v1","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"},"mcpServers":[{"server":"zeta","tools":["z","a"],"connection":{"transport":"STREAMABLE_HTTP","url":"https://z.example.com"}},{"server":"alpha","tools":["a"],"connection":{"transport":"SSE","url":"https://a.example.com"}}]}}`
	_, err := Decode([]byte(unsorted))
	require.ErrorContains(t, err, "sorted and unique")

	whitespace := `{"version": "v1","runtime":"codex","root":{"templateName":"assistant","model":{"provider":"OpenAI","name":"gpt-5"}}}`
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
