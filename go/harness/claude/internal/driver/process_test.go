package driver

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

type recordingSink struct {
	sessions []runtime.SessionStarted
}

func (s *recordingSink) SessionStarted(event runtime.SessionStarted) error {
	s.sessions = append(s.sessions, event)
	return nil
}
func (*recordingSink) TextDelta(runtime.TextDelta) error   { return nil }
func (*recordingSink) ToolCall(runtime.ToolCall) error     { return nil }
func (*recordingSink) ToolResult(runtime.ToolResult) error { return nil }

func TestProcessDriverArgumentsAndStream(t *testing.T) {
	dir := t.TempDir()
	capture := filepath.Join(dir, "args")
	stdinCapture := filepath.Join(dir, "stdin")
	systemCapture := filepath.Join(dir, "system-prompt")
	agentsCapture := filepath.Join(dir, "agents")
	executable := filepath.Join(dir, "claude")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo '2.1.236 (Claude Code)'; exit 0; fi
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then exit 0; fi
printf '%s\n' "$@" > "$CAPTURE"
previous=''
for argument in "$@"; do
  if [ "$previous" = "--append-system-prompt-file" ]; then
    cat "$argument" > "$SYSTEM_CAPTURE"
  fi
  if [ "$previous" = "--plugin-dir" ] && [ -d "$argument/agents" ]; then
    cat "$argument"/agents/*.md > "$AGENTS_CAPTURE"
  fi
  previous="$argument"
done
cat > "$STDIN_CAPTURE"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"11111111-1111-4111-8111-111111111111"}' '{"type":"result","subtype":"success","session_id":"11111111-1111-4111-8111-111111111111"}'
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	prompt := "cluster prompt secret"
	systemPrompt := "cluster system prompt secret"
	agentPrompt := "cluster agent prompt secret"
	agentsJSON := `{"reviewer":{"description":"Reviews changes","prompt":"` + agentPrompt + `","model":"sonnet"}}`
	mcpConfigPath := filepath.Join(dir, "mcp.json")
	d := NewProcessDriver(ProcessConfig{
		Executable: executable, ExpectedVersion: pinnedClaudeVersion, StrictVersion: true,
		Workspace: dir, Model: "claude-test", AppendSystemPrompt: systemPrompt,
		AgentsJSON: agentsJSON, MCPConfigPath: mcpConfigPath,
		Environment: []string{
			"CAPTURE=" + capture,
			"STDIN_CAPTURE=" + stdinCapture,
			"SYSTEM_CAPTURE=" + systemCapture,
			"AGENTS_CAPTURE=" + agentsCapture,
		},
		MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: time.Second,
	})
	if err := d.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	sink := &recordingSink{}
	turn := runtime.Turn{Prompt: prompt, ContinuationID: "11111111-1111-4111-8111-111111111111"}
	outcome, err := d.Run(t.Context(), turn, sink)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if outcome.Failure != nil {
		t.Fatalf("Run() outcome = %#v", outcome)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{prompt, systemPrompt, agentsJSON, agentPrompt} {
		if strings.Contains(string(args), secret) {
			t.Errorf("arguments expose cluster-controlled payload %q: %q", secret, args)
		}
	}
	for _, required := range []string{
		"-p\n", "--input-format\ntext\n", "--dangerously-skip-permissions\n",
		"--strict-mcp-config\n", "--append-system-prompt-file\n", "--plugin-dir\n",
	} {
		if !strings.Contains(string(args), required) {
			t.Errorf("arguments do not contain required fixed policy flag %q", strings.TrimSpace(required))
		}
	}
	if strings.Contains(string(args), "--agents\n") || strings.Contains(string(args), "--append-system-prompt\n") {
		t.Errorf("arguments use an inline payload flag: %q", args)
	}
	if !strings.Contains(string(args), "--mcp-config\n"+mcpConfigPath+"\n") {
		t.Error("arguments do not contain compiler-owned MCP configuration")
	}
	if strings.Contains(string(args), "--permission-prompt-tool\n") {
		t.Error("arguments unexpectedly configure Claude's native permission bridge")
	}
	if strings.Contains(string(args), "--bare\n") {
		t.Error("arguments unexpectedly disable normal Claude Code project/auth behavior with --bare")
	}
	stdinContents, err := os.ReadFile(stdinCapture)
	if err != nil {
		t.Fatal(err)
	}
	if string(stdinContents) != prompt {
		t.Errorf("stdin prompt = %q, want %q", stdinContents, prompt)
	}
	systemContents, err := os.ReadFile(systemCapture)
	if err != nil {
		t.Fatal(err)
	}
	if string(systemContents) != systemPrompt {
		t.Errorf("system prompt file = %q, want %q", systemContents, systemPrompt)
	}
	agentsContents, err := os.ReadFile(agentsCapture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(agentsContents), agentPrompt) || !strings.Contains(string(agentsContents), `name: "reviewer"`) {
		t.Errorf("generated agent definition = %q", agentsContents)
	}
	if len(sink.sessions) != 1 || sink.sessions[0].ContinuationID != turn.ContinuationID {
		t.Errorf("session events = %#v", sink.sessions)
	}
}

func TestProcessDriverDoesNotReflectStderr(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	secret := "provider stderr leaked /home/owner/.claude/.credentials.json token-secret"
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}' '{\"type\":\"result\",\"subtype\":\"success\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}'\nprintf '%s\\n' '" + secret + "' >&2\nexit 42\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, Workspace: dir,
		MaxEventBytes: 4096, MaxStderrBytes: 4096, InterruptGrace: time.Second,
	})
	_, err := driver.Run(t.Context(), runtime.Turn{Prompt: "safe prompt"}, &recordingSink{})
	if err == nil {
		t.Fatal("Run() succeeded, want process failure")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), ".credentials.json") || strings.Contains(err.Error(), "token-secret") {
		t.Fatalf("Run() reflected provider stderr across the boundary: %v", err)
	}
	if !strings.Contains(err.Error(), "claude exited with an error") {
		t.Fatalf("Run() error = %v, want sanitized process failure", err)
	}
}

func TestExternalHostArgumentsUseFixedSandboxPolicyWithoutPermissionBypass(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.236 (Claude Code)'; exit 0; fi\nif [ \"$1\" = \"auth\" ] && [ \"$2\" = \"status\" ]; then exit 0; fi\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(dir, "external-settings.json")
	if err := os.WriteFile(settings, []byte(`{"sandbox":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpConfig := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(mcpConfig, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	driver := NewProcessDriver(ProcessConfig{
		Executable: executable, ExpectedVersion: pinnedClaudeVersion, StrictVersion: true,
		Workspace: dir, ExternalSettingsPath: settings, MCPConfigPath: mcpConfig, RequireAuthStatus: true,
		Environment: []string{
			"PATH=/usr/bin:/bin",
			disableAutoMemoryEnvironment + "=0",
			strings.ToLower(disableAutoMemoryEnvironment) + "=case-variant",
			disableAutoMemoryEnvironment + "=duplicate",
			disableClaudeAIMCPEnvironment + "=true",
		},
		MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: time.Second,
	})
	if err := driver.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	args := driver.Args(runtime.Turn{Prompt: "hello"})
	joined := strings.Join(args, "\n")
	for _, required := range []string{"--setting-sources", "--settings", settings, "--permission-mode", "acceptEdits", "--strict-mcp-config", "--mcp-config", mcpConfig} {
		if !strings.Contains(joined, required) {
			t.Fatalf("external arguments do not contain %q: %s", required, joined)
		}
	}
	settingSources := -1
	for index, argument := range args {
		if argument == "--setting-sources" {
			settingSources = index
			break
		}
	}
	if settingSources < 0 || settingSources+1 >= len(args) || args[settingSources+1] != "" {
		t.Fatalf("external arguments enable user/project/local setting sources: %#v", args)
	}
	mcpConfigArgument := slices.Index(args, "--mcp-config")
	if mcpConfigArgument < 0 || mcpConfigArgument+1 >= len(args) || args[mcpConfigArgument+1] != mcpConfig {
		t.Fatalf("external arguments do not select the activation-scoped MCP configuration: %#v", args)
	}
	for _, forbidden := range []string{"--bare", "--dangerously-skip-permissions", "bypassPermissions", "user,project", "project", "local"} {
		if slices.Contains(args, forbidden) {
			t.Fatalf("external arguments contain forbidden configuration surface %q: %#v", forbidden, args)
		}
	}
	environment := driver.processEnvironment()
	for name, want := range map[string]string{
		disableAutoMemoryEnvironment:  disableAutoMemoryEnvironmentVal,
		disableClaudeAIMCPEnvironment: disableClaudeAIMCPEnvironmentVal,
	} {
		matches := 0
		for _, item := range environment {
			if item == name+"="+want {
				matches++
			} else if separator := strings.IndexByte(item, '='); separator >= 0 && strings.EqualFold(item[:separator], name) {
				t.Fatalf("external environment retains unsafe %s override: %#v", name, environment)
			}
		}
		if matches != 1 {
			t.Fatalf("external environment has %d canonical %s entries: %#v", matches, name, environment)
		}
	}
}

func TestExternalHostFailsClosedWithoutMCPConfiguration(t *testing.T) {
	driver := NewProcessDriver(ProcessConfig{
		Executable:           filepath.Join(t.TempDir(), "claude"),
		Workspace:            t.TempDir(),
		ExternalSettingsPath: filepath.Join(t.TempDir(), "external-settings.json"),
	})
	if args := driver.Args(runtime.Turn{Prompt: "hello"}); args != nil {
		t.Fatalf("Args() = %#v, want no runnable external command", args)
	}
	if err := driver.Validate(t.Context()); err == nil || !strings.Contains(err.Error(), "activation-scoped MCP configuration") {
		t.Fatalf("Validate() error = %v, want missing MCP configuration", err)
	}
	if _, err := driver.Run(t.Context(), runtime.Turn{Prompt: "hello"}, &recordingSink{}); err == nil || !strings.Contains(err.Error(), "activation-scoped MCP configuration") {
		t.Fatalf("Run() error = %v, want missing MCP configuration", err)
	}
}

func TestProcessDriverCancellation(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s\\n' '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"11111111-1111-4111-8111-111111111111\"}'\nwhile :; do :; done\n"
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	d := NewProcessDriver(ProcessConfig{Executable: executable, Workspace: dir, MaxEventBytes: 4096, MaxStderrBytes: 1024, InterruptGrace: 50 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err := d.Run(ctx, runtime.Turn{Prompt: "hello"}, &recordingSink{})
	if err != context.Canceled {
		t.Fatalf("Run() error = %v, want context canceled", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("cancellation took too long")
	}
}
