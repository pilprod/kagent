package adapter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/codex/config"
)

func TestPrepareInClusterUsesPrivateHomeAndEnvironmentOnlyCredentials(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	codexHome := filepath.Join(durableDir, "codex")
	workspace := filepath.Join(durableDir, "workspace")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// Existing user configuration is not an input layer and must disappear.
	if err := os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte("model = \"host-model\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Production("gpt-test", "high", "Use the compiler policy.")
	processConfig, err := prepare(context.Background(), Input{
		ConfigJSON: mustJSON(t, cfg), Mode: ModeInCluster,
		Workspace: workspace, DurableDir: durableDir, CodexHome: codexHome,
		Environment: []string{"PATH=/bin", "CODEX_HOME=/host/codex", "OPENAI_API_KEY=secret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(codexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(contents), "host-model") || strings.Contains(string(contents), "secret-value") {
		t.Fatalf("compiler-owned config leaked host config or secret:\n%s", contents)
	}
	if !strings.Contains(string(contents), `env_key = "OPENAI_API_KEY"`) {
		t.Fatalf("compiler-owned config omits provider env key:\n%s", contents)
	}
	if got, found, duplicate := environmentValue(processConfig.Environment, codexHomeEnvironment); !found || duplicate || got != codexHome {
		t.Fatalf("driver CODEX_HOME = %q, %t, %t", got, found, duplicate)
	}
	if processConfig.ModelProvider != cfg.ModelProvider || processConfig.SandboxPolicy.ExternalSandbox == nil ||
		processConfig.SandboxPolicy.ExternalSandbox.NetworkAccess != cfg.NetworkAccess {
		t.Fatalf("driver provider/sandbox = %q, %#v", processConfig.ModelProvider, processConfig.SandboxPolicy)
	}
	if processConfig.HandshakeTimeout != cfg.HandshakeTimeout() || processConfig.ShutdownGrace != cfg.ShutdownGrace() {
		t.Fatalf("driver handshake/shutdown = %s, %s", processConfig.HandshakeTimeout, processConfig.ShutdownGrace)
	}
	for _, path := range []string{durableDir, workspace, codexHome} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("%s permissions = %v, %v", path, info, err)
		}
	}
	if info, err := os.Stat(filepath.Join(codexHome, "config.toml")); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config permissions = %v, %v", info, err)
	}
}

func TestPrepareExternalHostPreservesAuthOutsideSnapshot(t *testing.T) {
	root := t.TempDir()
	durableDir := filepath.Join(root, "runtime-data")
	workspace := filepath.Join(root, "workspace")
	codexHome := filepath.Join(root, "host-auth", "codex")
	for _, path := range []string{workspace, codexHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte("host-managed-auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExternalHomeMarker(t, codexHome)
	staleSkill := filepath.Join(durableDir, "generated-codex", "skills", "removed", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(staleSkill), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staleSkill, []byte("stale revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := portableCodexConfig("gpt-test", "medium", "", "")
	processConfig, err := prepare(context.Background(), Input{
		ConfigJSON: cfg, Mode: ModeExternalHost,
		Workspace: workspace, DurableDir: durableDir, CodexHome: codexHome,
		CodexExecutable: "/opt/yourown-chat/bin/codex",
		Environment: []string{
			"PATH=/bin", "CODEX_HOME=/wrong", "KAGENT_CONFIG_JSON=private",
			"KAGENT_AGENT_CARD_JSON=private", "YOUROWN_CHAT_READINESS_TOKEN=" + strings.Repeat("a", 64),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := os.ReadFile(authPath)
	if err != nil || string(auth) != "host-managed-auth" {
		t.Fatalf("host auth = %q, %v", auth, err)
	}
	if _, err := os.Stat(filepath.Join(durableDir, "codex")); !os.IsNotExist(err) {
		t.Fatalf("external credentials were copied below durable dir: %v", err)
	}
	if _, err := os.Stat(staleSkill); !os.IsNotExist(err) {
		t.Fatalf("skill from a previous activation was retained: %v", err)
	}
	if got, found, duplicate := environmentValue(processConfig.Environment, codexHomeEnvironment); !found || duplicate || got != codexHome {
		t.Fatalf("driver CODEX_HOME = %q, %t, %t", got, found, duplicate)
	}
	if processConfig.Executable != "/opt/yourown-chat/bin/codex" {
		t.Fatalf("driver executable = %q", processConfig.Executable)
	}
	if processConfig.SandboxPolicy.PermissionProfile == nil || processConfig.SandboxPolicy.ExternalSandbox != nil ||
		processConfig.SandboxPolicy.PermissionProfile.ID != externalPermissionProfile {
		t.Fatalf("external driver sandbox = %#v", processConfig.SandboxPolicy)
	}
	if processConfig.CredentialReadProbe != authPath {
		t.Fatalf("credential read probe = %q, want %q", processConfig.CredentialReadProbe, authPath)
	}
	for _, name := range []string{"KAGENT_CONFIG_JSON", "KAGENT_AGENT_CARD_JSON", "YOUROWN_CHAT_READINESS_TOKEN", "YOUROWN_CHAT_TRANSPORT_TOKEN"} {
		if _, found, _ := environmentValue(processConfig.Environment, name); found {
			t.Fatalf("driver inherited harness control environment %s", name)
		}
	}
}

func TestPrepareRejectsCredentialBoundaryViolations(t *testing.T) {
	root := t.TempDir()
	durableDir := filepath.Join(root, "data")
	workspace := filepath.Join(root, "workspace")
	externalHome := filepath.Join(root, "external-codex")
	for _, path := range []string{workspace, externalHome} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeExternalHomeMarker(t, externalHome)
	production := config.Production("gpt-test", "medium", "")
	preauthenticated := config.Preauthenticated("gpt-test", "medium", "")
	portable := portableCodexConfig("gpt-test", "medium", "", "")
	withMCPSecret := config.Production("gpt-test", "medium", "")
	withMCPSecret.MCPServers = map[string]config.MCPServer{"tools": {
		Type: config.MCPTypeHTTP, URL: "https://mcp.example.com", Headers: map[string]string{"Authorization": "${MCP_TOKEN}"},
	}}
	tests := map[string]Input{
		"in-cluster without API key": {
			ConfigJSON: mustJSON(t, production), Mode: ModeInCluster,
			Workspace: filepath.Join(durableDir, "workspace"), DurableDir: durableDir,
			CodexHome: filepath.Join(durableDir, "codex"), Environment: []string{"PATH=/bin"},
		},
		"pre-auth home below snapshot root": {
			ConfigJSON: portable, Mode: ModeExternalHost,
			Workspace: workspace, DurableDir: durableDir,
			CodexHome: filepath.Join(durableDir, "codex"), Environment: []string{"PATH=/bin"},
		},
		"external host with API key": {
			ConfigJSON: portable, Mode: ModeExternalHost,
			Workspace: workspace, DurableDir: durableDir,
			CodexHome: externalHome, CodexExecutable: "/opt/codex", Environment: []string{"OPENAI_API_KEY=secret"},
		},
		"external host with access token": {
			ConfigJSON: portable, Mode: ModeExternalHost,
			Workspace: workspace, DurableDir: durableDir,
			CodexHome: externalHome, CodexExecutable: "/opt/codex", Environment: []string{"CODEX_ACCESS_TOKEN=secret"},
		},
		"in-cluster with pre-auth config": {
			ConfigJSON: mustJSON(t, preauthenticated), Mode: ModeInCluster,
			Workspace: filepath.Join(durableDir, "workspace"), DurableDir: durableDir,
			CodexHome: filepath.Join(durableDir, "codex"), Environment: []string{"OPENAI_API_KEY=secret"},
		},
		"missing MCP credential environment": {
			ConfigJSON: mustJSON(t, withMCPSecret), Mode: ModeInCluster,
			Workspace: filepath.Join(durableDir, "workspace"), DurableDir: durableDir,
			CodexHome: filepath.Join(durableDir, "codex"), Environment: []string{"OPENAI_API_KEY=secret"},
		},
	}
	tests["external host without owner executable"] = Input{
		ConfigJSON: portable, Mode: ModeExternalHost,
		Workspace: workspace, DurableDir: durableDir, CodexHome: externalHome, Environment: []string{"PATH=/bin"},
	}
	overlappingHome := filepath.Join(workspace, "codex")
	if err := os.MkdirAll(overlappingHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlappingHome, "auth.json"), []byte("auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExternalHomeMarker(t, overlappingHome)
	tests["external workspace and home overlap"] = Input{
		ConfigJSON: portable, Mode: ModeExternalHost,
		Workspace: workspace, DurableDir: durableDir, CodexHome: overlappingHome,
		CodexExecutable: "/opt/codex", Environment: []string{"PATH=/bin"},
	}
	hiddenHome := filepath.Join(durableDir, "hidden", "codex")
	if err := os.MkdirAll(hiddenHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hiddenHome, "auth.json"), []byte("auth"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeExternalHomeMarker(t, hiddenHome)
	aliasedParent := filepath.Join(root, "aliased-durable-child")
	if err := os.Symlink(filepath.Dir(hiddenHome), aliasedParent); err == nil {
		tests["external durable and home overlap through ancestor symlink"] = Input{
			ConfigJSON: portable, Mode: ModeExternalHost,
			Workspace: workspace, DurableDir: durableDir, CodexHome: filepath.Join(aliasedParent, "codex"),
			CodexExecutable: "/opt/codex", Environment: []string{"PATH=/bin"},
		}
	}
	unmarkedHome := filepath.Join(root, "unmarked-codex")
	if err := os.MkdirAll(unmarkedHome, 0o700); err != nil {
		t.Fatal(err)
	}
	tests["external host without owner marker"] = Input{
		ConfigJSON: portable, Mode: ModeExternalHost,
		Workspace: workspace, DurableDir: durableDir, CodexHome: unmarkedHome,
		CodexExecutable: "/opt/codex", Environment: []string{"PATH=/bin"},
	}
	nonemptyMarkerHome := filepath.Join(root, "nonempty-marker-codex")
	if err := os.MkdirAll(nonemptyMarkerHome, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nonemptyMarkerHome, externalHomeMarker), []byte("managed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests["external host with nonempty owner marker"] = Input{
		ConfigJSON: portable, Mode: ModeExternalHost,
		Workspace: workspace, DurableDir: durableDir, CodexHome: nonemptyMarkerHome,
		CodexExecutable: "/opt/codex", Environment: []string{"PATH=/bin"},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := prepare(context.Background(), input); err == nil {
				t.Fatal("prepare() accepted a credential boundary violation")
			}
		})
	}
}

func writeExternalHomeMarker(t *testing.T, codexHome string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(codexHome, externalHomeMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	contents, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contents
}

func portableCodexConfig(model, effort, serviceTier, instruction string) []byte {
	type portableModel struct {
		Provider        string `json:"provider"`
		Name            string `json:"name"`
		ReasoningEffort string `json:"reasoningEffort,omitempty"`
		ServiceTier     string `json:"serviceTier,omitempty"`
	}
	type portableRoot struct {
		TemplateName string        `json:"templateName"`
		Instruction  string        `json:"instruction,omitempty"`
		Model        portableModel `json:"model"`
	}
	raw, err := json.Marshal(struct {
		Version string       `json:"version"`
		Runtime string       `json:"runtime"`
		Root    portableRoot `json:"root"`
	}{
		Version: "v2", Runtime: "codex",
		Root: portableRoot{
			TemplateName: "agent-one", Instruction: instruction,
			Model: portableModel{Provider: "OpenAI", Name: model, ReasoningEffort: effort, ServiceTier: serviceTier},
		},
	})
	if err != nil {
		panic(err)
	}
	return raw
}
