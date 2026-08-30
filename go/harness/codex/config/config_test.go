package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
)

func TestProductionRoundTrip(t *testing.T) {
	cfg := Production("gpt-test", "high", "Work carefully.")
	cfg.ServiceTier = "fast"
	cfg.SkillResources = &agentplugin.Resources{Skills: []agentplugin.Skill{{
		Name: "review",
		Source: agentplugin.Source{Git: &agentplugin.GitSource{
			URL: "https://example.com/skills", Commit: strings.Repeat("a", 40),
		}},
	}}}
	cfg.MCPServers = map[string]MCPServer{"tools": {
		Type: MCPTypeStreamable, URL: "https://mcp.example.com/mcp",
		Headers: map[string]string{"Authorization": "${KAGENT_CODEX_MCP_CREDENTIAL_1}"},
		Tools:   []string{"lookup"}, StartupTimeoutMS: 5_000, ToolTimeoutSeconds: 30,
	}}
	contents, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(contents)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ExpectedCodexVersion != PinnedCodexVersion || parsed.Model != "gpt-test" || parsed.ModelProvider != DefaultModelProvider || parsed.ReasoningEffort != "high" || parsed.ServiceTier != "fast" {
		t.Fatalf("parsed config = %#v", parsed)
	}
	if parsed.HandshakeTimeout() != 10*time.Second || parsed.ShutdownGrace() != 2*time.Second {
		t.Fatalf("parsed limits = %s, %s", parsed.HandshakeTimeout(), parsed.ShutdownGrace())
	}
}

func TestPreauthenticatedUsesBuiltInProvider(t *testing.T) {
	cfg := Preauthenticated("gpt-test", "medium", "")
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ModelProvider != "openai" || cfg.Provider != nil {
		t.Fatalf("pre-authenticated config = %#v", cfg)
	}
	if cfg.ServiceTier != "" {
		t.Fatalf("default service tier = %q, want empty", cfg.ServiceTier)
	}
	contents, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "service_tier") {
		t.Fatalf("default config serialized service_tier: %s", contents)
	}
}

func TestHeaderEnvironment(t *testing.T) {
	if got, ok := HeaderEnvironment("${TOKEN_1}"); !ok || got != "TOKEN_1" {
		t.Fatalf("HeaderEnvironment() = %q, %t", got, ok)
	}
	for _, value := range []string{"Bearer ${TOKEN}", "${not-valid}", "TOKEN"} {
		if _, ok := HeaderEnvironment(value); ok {
			t.Fatalf("HeaderEnvironment(%q) succeeded", value)
		}
	}
}

func TestSensitiveHeader(t *testing.T) {
	for _, name := range []string{"Authorization", "X-Auth-Token", "XApiKey", "X-Authorization-Value", "Ocp-Apim-Subscription-Key"} {
		if !SensitiveHeader(name) {
			t.Errorf("SensitiveHeader(%q) = false", name)
		}
	}
	if SensitiveHeader("X-Tenant") {
		t.Fatal("SensitiveHeader(X-Tenant) = true")
	}
}

func TestParseRejectsUnknownAndTrailingValues(t *testing.T) {
	if _, err := Parse([]byte(`{"version":1,"surprise":true}`)); err == nil {
		t.Fatal("Parse() accepted an unknown field")
	}
	if _, err := Parse([]byte(`{} {}`)); err == nil {
		t.Fatal("Parse() accepted a trailing JSON value")
	}
}

func TestValidateRejectsInvalidSettings(t *testing.T) {
	tests := map[string]func(*Config){
		"empty model":                func(cfg *Config) { cfg.Model = " " },
		"unknown effort":             func(cfg *Config) { cfg.ReasoningEffort = "enormous" },
		"unknown service tier":       func(cfg *Config) { cfg.ServiceTier = "slow" },
		"unknown network":            func(cfg *Config) { cfg.NetworkAccess = "maybe" },
		"reserved provider override": func(cfg *Config) { cfg.ModelProvider = "openai" },
		"invalid provider URL":       func(cfg *Config) { cfg.Provider.BaseURL = "https://user@example.com/v1" },
		"invalid provider env":       func(cfg *Config) { cfg.Provider.APIKeyEnv = "NOT-VALID" },
		"zero event limit":           func(cfg *Config) { cfg.MaxEventBytes = 0 },
		"invalid server name":        func(cfg *Config) { cfg.MCPServers = map[string]MCPServer{"bad name": validServer()} },
		"invalid transport": func(cfg *Config) {
			server := validServer()
			server.Type = "stdio"
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"invalid URL": func(cfg *Config) {
			server := validServer()
			server.URL = "file:///tmp/mcp"
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"MCP URL query": func(cfg *Config) {
			server := validServer()
			server.URL = "https://mcp.example.com/mcp?token=secret"
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"empty MCP URL query": func(cfg *Config) {
			server := validServer()
			server.URL = "https://mcp.example.com/mcp?"
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"static credential header": func(cfg *Config) {
			server := validServer()
			server.Headers = map[string]string{"X-Auth-Token": "secret"}
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"mixed header interpolation": func(cfg *Config) {
			server := validServer()
			server.Headers = map[string]string{"Authorization": "Bearer ${TOKEN}"}
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"duplicate selected tool": func(cfg *Config) {
			server := validServer()
			server.Tools = []string{"lookup", "lookup"}
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
		"negative MCP timeout": func(cfg *Config) {
			server := validServer()
			server.StartupTimeoutMS = -1
			cfg.MCPServers = map[string]MCPServer{"tools": server}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := Production("gpt-test", "medium", "")
			mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("Validate() accepted %#v", cfg)
			}
		})
	}
}

func validServer() MCPServer {
	return MCPServer{Type: MCPTypeHTTP, URL: "https://mcp.example.com/mcp"}
}
