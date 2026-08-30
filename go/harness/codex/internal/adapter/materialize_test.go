package adapter

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
)

func TestRenderCodexConfigOwnsProviderMCPAndSkills(t *testing.T) {
	cfg := config.Production("gpt-test", "high", "Do the requested work.")
	cfg.SkillResources = &agentplugin.Resources{
		Skills:  []agentplugin.Skill{{Name: "review"}},
		Plugins: []agentplugin.Bundle{{Skills: []string{"deploy"}}},
	}
	cfg.MCPServers = map[string]config.MCPServer{
		"tools": {
			Type: config.MCPTypeStreamable, URL: "https://mcp.example.com/mcp",
			Tools: []string{"lookup", "search"}, StartupTimeoutMS: 5_000, ToolTimeoutSeconds: 30,
			Headers: map[string]string{
				"Authorization": "${KAGENT_CODEX_MCP_CREDENTIAL_1}",
				"X-Tenant":      "tenant-a",
			},
		},
	}
	raw, err := renderCodexConfig(cfg, "/data/workspace", "/data/codex", "/data/codex/skills")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(raw)
	var parsed map[string]any
	if _, err := toml.Decode(contents, &parsed); err != nil {
		t.Fatalf("generated invalid TOML: %v\n%s", err, contents)
	}
	for _, expected := range []string{
		`model = "gpt-test"`,
		`model_provider = "kagent-openai"`,
		`model_reasoning_effort = "high"`,
		`developer_instructions = "Do the requested work."`,
		`web_search = "disabled"`,
		`check_for_update_on_startup = false`,
		`[shell_environment_policy]`,
		`inherit = "core"`,
		`ignore_default_excludes = false`,
		`"OPENAI_API_KEY" = "exclude"`,
		`"CODEX_ACCESS_TOKEN" = "exclude"`,
		`"KAGENT_CODEX_MCP_CREDENTIAL_1" = "exclude"`,
		`[apps._default]`,
		`enabled = false`,
		`[features]`,
		`multi_agent = false`,
		`[model_providers."kagent-openai"]`,
		`env_key = "OPENAI_API_KEY"`,
		`wire_api = "responses"`,
		`requires_openai_auth = false`,
		`[projects."/data/workspace"]`,
		`trust_level = "untrusted"`,
		`[mcp_servers."tools"]`,
		`default_tools_approval_mode = "approve"`,
		`enabled_tools = ["lookup", "search"]`,
		`startup_timeout_ms = 5000`,
		`tool_timeout_sec = 30`,
		`http_headers = { "X-Tenant" = "tenant-a" }`,
		`env_http_headers = { "Authorization" = "KAGENT_CODEX_MCP_CREDENTIAL_1" }`,
		`path = "/data/codex/skills/deploy"`,
		`path = "/data/codex/skills/review"`,
	} {
		if !strings.Contains(contents, expected) {
			t.Errorf("config.toml does not contain %q:\n%s", expected, contents)
		}
	}
	if strings.Contains(contents, "${KAGENT_CODEX_MCP_CREDENTIAL_1}") {
		t.Fatalf("config.toml persisted a credential placeholder as a static value:\n%s", contents)
	}
}

func TestRenderPreauthenticatedConfigHasNoCustomProvider(t *testing.T) {
	cfg := config.Preauthenticated("gpt-test", "medium", "")
	raw, err := renderCodexConfig(cfg, "/workspace", "/host/codex", "/host/codex/skills")
	if err != nil {
		t.Fatal(err)
	}
	contents := string(raw)
	if !strings.Contains(contents, `model_provider = "openai"`) ||
		!strings.Contains(contents, `cli_auth_credentials_store = "file"`) ||
		!strings.Contains(contents, `default_permissions = "yourown_chat_local"`) ||
		!strings.Contains(contents, `[permissions."yourown_chat_local".filesystem]`) ||
		!strings.Contains(contents, `"/" = "read"`) ||
		!strings.Contains(contents, `"/workspace" = "write"`) ||
		!strings.Contains(contents, `"/host/codex/auth.json" = "deny"`) ||
		!strings.Contains(contents, `[permissions."yourown_chat_local".network]`) ||
		strings.Contains(contents, "[model_providers.") {
		t.Fatalf("pre-authenticated config =\n%s", contents)
	}
}
