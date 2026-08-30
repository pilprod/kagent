package adapter

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	harnessruntime "github.com/kagent-dev/kagent/go/harness/runtime"
)

func TestNewMaterializesDurableDirectories(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	ephemeralDir := filepath.Join(t.TempDir(), "credentials")
	workspace := filepath.Join(durableDir, "workspace")
	runner, err := New(context.Background(), Input{
		ConfigJSON: []byte(`{"version":3,"claude_executable":"claude","expected_claude_version":"2.1.236","strict_version":true,"max_event_bytes":100,"max_stderr_bytes":100,"interrupt_grace_millis":100}`),
		Workspace:  workspace,
		DurableDir: durableDir, EphemeralDir: ephemeralDir,
		Environment: []string{"PATH=/bin", "CLAUDE_CONFIG_DIR=/wrong", "DISABLE_UPDATES=0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if runner == nil {
		t.Fatal("New() returned a nil runner")
	}
	for _, path := range []string{workspace, filepath.Join(durableDir, "claude"), filepath.Join(durableDir, "generated-claude")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o700 {
			t.Errorf("%s permissions = %o, want 700", path, info.Mode().Perm())
		}
	}
}

func TestNewMaterializesSkillsAndMCPConfig(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	repository, commit := createSkillRepository(t)
	cfg := config.Production("claude-test", "help")
	cfg.StrictVersion = false
	cfg.SkillResources = &agentplugin.Resources{Skills: []agentplugin.Skill{{
		Name: "review", Source: agentplugin.Source{Git: &agentplugin.GitSource{URL: repository, Commit: commit}},
	}}}
	cfg.MCPServers = map[string]config.MCPServer{"tools": {Type: "http", URL: "https://mcp.example.com/mcp"}}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ephemeralDir := filepath.Join(t.TempDir(), "generated")
	runner, err := New(context.Background(), Input{
		ConfigJSON: raw, Workspace: filepath.Join(durableDir, "workspace"), DurableDir: durableDir,
		EphemeralDir: ephemeralDir, Environment: []string{"PATH=/bin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pluginDir := filepath.Join(durableDir, "generated-claude", "portable-skills-plugin")
	for path, want := range map[string]string{
		filepath.Join(pluginDir, "skills", "review", "SKILL.md"): "# Review",
		filepath.Join(ephemeralDir, "mcp.json"):                  `{"mcpServers":{"tools":{"type":"http","url":"https://mcp.example.com/mcp"}}}`,
	} {
		contents, err := os.ReadFile(path)
		if err != nil || string(contents) != want {
			t.Fatalf("%s = %q, %v; want %q", path, contents, err, want)
		}
	}
	arguments := strings.Join(runner.Args(harnessruntime.Turn{Prompt: "hello"}), "\n")
	if !strings.Contains(arguments, "--plugin-dir\n"+pluginDir) {
		t.Fatalf("Claude arguments do not select the activation-scoped skill plugin: %s", arguments)
	}
}

func TestNewReplacesActivationScopedSkills(t *testing.T) {
	durableDir := filepath.Join(t.TempDir(), "data")
	workspace := filepath.Join(durableDir, "workspace")
	ephemeralDir := filepath.Join(t.TempDir(), "generated")

	prepare := func(name, contents string) {
		t.Helper()
		repository, commit := createSkillRepositoryWithContent(t, contents)
		cfg := config.Production("claude-test", "help")
		cfg.StrictVersion = false
		cfg.SkillResources = &agentplugin.Resources{Skills: []agentplugin.Skill{{
			Name: name, Source: agentplugin.Source{Git: &agentplugin.GitSource{URL: repository, Commit: commit}},
		}}}
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(context.Background(), Input{
			ConfigJSON: raw, Workspace: workspace, DurableDir: durableDir,
			EphemeralDir: ephemeralDir, Environment: []string{"PATH=/bin"},
		}); err != nil {
			t.Fatal(err)
		}
	}

	prepare("old", "# Old revision")
	oldPath := filepath.Join(durableDir, "generated-claude", "portable-skills-plugin", "skills", "old", "SKILL.md")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatal(err)
	}
	prepare("new", "# New revision")
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("skill from a previous activation was retained: %v", err)
	}
	newPath := filepath.Join(durableDir, "generated-claude", "portable-skills-plugin", "skills", "new", "SKILL.md")
	contents, err := os.ReadFile(newPath)
	if err != nil || string(contents) != "# New revision" {
		t.Fatalf("replacement skill = %q, %v", contents, err)
	}
}

func createSkillRepository(t *testing.T) (string, string) {
	return createSkillRepositoryWithContent(t, "# Review")
}

func createSkillRepositoryWithContent(t *testing.T, contents string) (string, string) {
	t.Helper()
	repository := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", repository}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init")
	git("config", "user.email", "test@example.com")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repository, "SKILL.md"), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "SKILL.md")
	git("commit", "-m", "skill")
	return repository, git("rev-parse", "HEAD")
}

func TestNewRejectsInvalidInput(t *testing.T) {
	input := Input{ConfigJSON: []byte(`{}`), Workspace: "relative", DurableDir: "relative", EphemeralDir: "relative"}
	if _, err := New(context.Background(), input); err == nil {
		t.Fatal("New() accepted invalid input")
	}
}

func TestExternalHostUsesOwnerPathsAndFailClosedClaudeSandbox(t *testing.T) {
	root := t.TempDir()
	durable := filepath.Join(root, "activation")
	workspace := filepath.Join(root, "workspace")
	claudeHome := filepath.Join(root, "provider", "claude-slot")
	for _, directory := range []string{workspace, claudeHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(claudeHome, externalHomeMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeHome, ".credentials.json"), []byte(`{"oauth":"redacted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw := portableClaudeConfig("claude-test", "owner policy")
	runner, err := New(t.Context(), Input{
		ConfigJSON: raw, Mode: ModeExternalHost, Workspace: workspace,
		DurableDir: durable, EphemeralDir: filepath.Join(durable, "ephemeral"),
		ClaudeConfigDir: claudeHome, ClaudeExecutable: executable,
		Environment: []string{"PATH=/bin", "KAGENT_CONFIG_JSON=private"},
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments := strings.Join(runner.Args(harnessruntime.Turn{Prompt: "hello"}), "\n")
	mcpConfigPath := filepath.Join(durable, "generated-claude", "mcp.json")
	for _, required := range []string{"--setting-sources", "--settings", "--permission-mode", "acceptEdits", "--strict-mcp-config", "--no-chrome", "--mcp-config", mcpConfigPath} {
		if !strings.Contains(arguments, required) {
			t.Fatalf("external arguments do not contain %q: %s", required, arguments)
		}
	}
	args := runner.Args(harnessruntime.Turn{Prompt: "hello"})
	settingSources := -1
	for index, argument := range args {
		if argument == "--setting-sources" {
			settingSources = index
			break
		}
	}
	if settingSources < 0 || settingSources+1 >= len(args) || args[settingSources+1] != "" {
		t.Fatalf("external arguments enable ambient setting sources: %#v", args)
	}
	for _, forbidden := range []string{"--bare", "--dangerously-skip-permissions", "bypassPermissions", "user,project", "project", "local"} {
		for _, argument := range args {
			if argument == forbidden {
				t.Fatalf("external arguments contain forbidden configuration surface %q: %#v", forbidden, args)
			}
		}
	}
	settingsPath := filepath.Join(durable, "external-settings.json")
	settings, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{`"failIfUnavailable":true`, `"allowUnsandboxedCommands":false`, claudeHome, `"disableBypassPermissionsMode":"disable"`} {
		if !strings.Contains(string(settings), required) {
			t.Fatalf("external settings do not contain %q: %s", required, settings)
		}
	}
	if info, err := os.Stat(settingsPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("external settings permissions = %v, %v", info, err)
	}
	mcpConfig, err := os.ReadFile(mcpConfigPath)
	if err != nil || string(mcpConfig) != `{"mcpServers":{}}` {
		t.Fatalf("external MCP configuration = %q, %v", mcpConfig, err)
	}
	if info, err := os.Stat(mcpConfigPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("external MCP configuration permissions = %v, %v", info, err)
	}

	_, err = New(t.Context(), Input{
		ConfigJSON: raw, Mode: ModeExternalHost, Workspace: workspace,
		DurableDir: filepath.Join(root, "other-activation"), EphemeralDir: filepath.Join(root, "other-activation", "ephemeral"),
		ClaudeConfigDir: claudeHome, ClaudeExecutable: executable,
		Environment: []string{"ANTHROPIC_API_KEY=cluster-secret"},
	})
	if err == nil || !strings.Contains(err.Error(), "must not be injected") {
		t.Fatalf("external Claude accepted a credential environment override: %v", err)
	}
}

func portableClaudeConfig(model, instruction string) []byte {
	type portableModel struct {
		Provider string `json:"provider"`
		Name     string `json:"name"`
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
		Version: "v2", Runtime: "claude",
		Root: portableRoot{TemplateName: "agent-one", Instruction: instruction,
			Model: portableModel{Provider: "Anthropic", Name: model}},
	})
	if err != nil {
		panic(err)
	}
	return raw
}

func TestMaterializeGoogleCredentials(t *testing.T) {
	dir := t.TempDir()
	raw := `{"type":"service_account","project_id":"test"}`
	environment, err := materializeGoogleCredentials([]string{"A=1", config.GoogleCredentialsJSONEnvName + "=" + raw}, dir)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "google-credentials.json")
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != raw {
		t.Fatalf("credentials = %q", contents)
	}
	if len(environment) != 2 || environment[0] != "A=1" || environment[1] != googleApplicationCredentialsEnv+"="+path {
		t.Fatalf("environment = %v", environment)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential permissions = %v, %v", info, err)
	}
}

func TestSetEnvironmentOverridesExistingValue(t *testing.T) {
	got := setEnvironment([]string{"A=1", "A=2", "B=3"}, "A", "4")
	want := []string{"B=3", "A=4"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("setEnvironment() = %v, want %v", got, want)
	}
}
