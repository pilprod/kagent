package driver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kagent-dev/kagent/go/harness/runtime"
)

type ProcessConfig struct {
	Executable         string
	ExpectedVersion    string
	StrictVersion      bool
	Workspace          string
	Model              string
	AppendSystemPrompt string
	AgentsJSON         string
	MCPConfigPath      string
	// ExternalSettingsPath selects the immutable, adapter-generated settings
	// policy for an external-host process. When set, project and user settings
	// are not loaded and Claude's built-in sandbox must start successfully.
	ExternalSettingsPath string
	// PluginDirs are adapter-generated, activation-scoped local plugin roots.
	// Cluster config can select skill content but cannot supply these paths.
	PluginDirs        []string
	RequireAuthStatus bool
	Environment       []string
	MaxEventBytes     int
	MaxStderrBytes    int
	InterruptGrace    time.Duration
}

type ProcessDriver struct {
	config ProcessConfig
}

const (
	maxStdinPromptBytes              = 10 << 20
	maxSystemPromptBytes             = 1 << 20
	maxGeneratedAgentsJSON           = 1 << 20
	generatedAgentsPluginName        = "kagent-generated-agents"
	disableAutoMemoryEnvironment     = "CLAUDE_CODE_DISABLE_AUTO_MEMORY"
	disableClaudeAIMCPEnvironment    = "ENABLE_CLAUDEAI_MCP_SERVERS"
	disableAutoMemoryEnvironmentVal  = "1"
	disableClaudeAIMCPEnvironmentVal = "false"
)

var generatedAgentNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func NewProcessDriver(config ProcessConfig) *ProcessDriver {
	return &ProcessDriver{config: config}
}

func (d *ProcessDriver) Validate(ctx context.Context) error {
	if err := d.validateExternalConfiguration(); err != nil {
		return err
	}
	path, err := exec.LookPath(d.config.Executable)
	if err != nil {
		return fmt.Errorf("find Claude executable %q: %w", d.config.Executable, err)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Dir = d.config.Workspace
	cmd.Env = d.processEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("read Claude version: %w", err)
	}
	version := strings.TrimSpace(string(output))
	if d.config.StrictVersion && !strings.Contains(version, d.config.ExpectedVersion) {
		return fmt.Errorf("claude version mismatch: got %q, expected %q", version, d.config.ExpectedVersion)
	}
	if d.config.RequireAuthStatus {
		cmd := exec.CommandContext(ctx, path, "auth", "status")
		cmd.Dir = d.config.Workspace
		cmd.Env = d.processEnvironment()
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("Claude external-host authentication is unavailable")
		}
	}
	return nil
}

func (d *ProcessDriver) Args(turn runtime.Turn) []string {
	// Args is an inspection helper and cannot return an error. Return no runnable
	// command when the external adapter omitted a required fail-closed input;
	// Validate and Run return the concrete configuration error.
	if d.validateExternalConfiguration() != nil {
		return nil
	}
	return d.args(turn, invocationResources{})
}

func (d *ProcessDriver) args(turn runtime.Turn, resources invocationResources) []string {
	args := []string{
		"-p",
		"--input-format", "text",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--strict-mcp-config",
		"--no-chrome",
	}
	if d.config.ExternalSettingsPath == "" {
		// In-cluster Claude runs inside the Harness sandbox. Native external-host
		// mode must never inherit this bypass.
		args = append(args, "--dangerously-skip-permissions")
	} else {
		// Do not use --bare here: current Claude Code releases deliberately skip
		// subscription OAuth and keychain reads in bare mode. An empty setting-source
		// list excludes user/project/local settings and their CLAUDE.md context while
		// preserving authentication from the owner-selected CLAUDE_CONFIG_DIR.
		// Explicit settings, strict MCP config, and explicit plugin directories are
		// the only non-managed configuration surfaces enabled by this invocation.
		args = append(args,
			"--setting-sources", "",
			"--settings", d.config.ExternalSettingsPath,
			"--permission-mode", "acceptEdits",
		)
	}
	if d.config.Model != "" {
		args = append(args, "--model", d.config.Model)
	}
	if resources.systemPromptPath != "" {
		args = append(args, "--append-system-prompt-file", resources.systemPromptPath)
	}
	if resources.agentsPluginPath != "" {
		args = append(args, "--plugin-dir", resources.agentsPluginPath)
	}
	if d.config.MCPConfigPath != "" {
		args = append(args, "--mcp-config", d.config.MCPConfigPath)
	}
	for _, pluginDir := range d.config.PluginDirs {
		args = append(args, "--plugin-dir", pluginDir)
	}
	if turn.ContinuationID != "" {
		// Resume the Actor's exact root conversation. --continue selects Claude's
		// latest session and can be redirected by subagents or interrupted attempts.
		args = append(args, "--resume", turn.ContinuationID)
	}
	return args
}

func (d *ProcessDriver) Run(ctx context.Context, turn runtime.Turn, sink runtime.EventSink) (runtime.Outcome, error) {
	if err := d.validateExternalConfiguration(); err != nil {
		return runtime.Outcome{}, err
	}
	resources, err := prepareInvocationResources(d.config.AppendSystemPrompt, d.config.AgentsJSON)
	if err != nil {
		return runtime.Outcome{}, err
	}
	defer resources.Close()

	cmd := exec.Command(d.config.Executable, d.args(turn, resources)...)
	configureProcessGroup(cmd)
	cmd.Dir = d.config.Workspace
	cmd.Env = d.processEnvironment()
	// Claude print mode accepts the user prompt on stdin. Keep the complete
	// cluster-controlled prompt out of the process table and close stdin at EOF.
	if len(turn.Prompt) > maxStdinPromptBytes {
		return runtime.Outcome{}, fmt.Errorf("Claude prompt exceeds the supported stdin limit")
	}
	cmd.Stdin = strings.NewReader(turn.Prompt)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return runtime.Outcome{}, fmt.Errorf("open Claude stdout: %w", err)
	}
	stderr := &boundedBuffer{max: d.config.MaxStderrBytes}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return runtime.Outcome{}, fmt.Errorf("start Claude: %w", err)
	}
	type parseItem struct {
		event *Event
		err   error
	}
	items := make(chan parseItem)
	stopEmit := make(chan struct{})
	go func() {
		defer close(items)
		parseErr := ParseJSONL(stdout, d.config.MaxEventBytes, func(event Event) error {
			select {
			case items <- parseItem{event: &event}:
				return nil
			case <-stopEmit:
				return context.Canceled
			}
		})
		select {
		case items <- parseItem{err: parseErr}:
		case <-stopEmit:
		}
	}()
	waitDone := make(chan error, 1)
	var waitOnce sync.Once
	waitForExit := func() <-chan error {
		waitOnce.Do(func() {
			go func() { waitDone <- cmd.Wait() }()
		})
		return waitDone
	}
	var terminal *runtime.Outcome

	for {
		select {
		case item, ok := <-items:
			if !ok {
				return runtime.Outcome{}, fmt.Errorf("claude parser stopped without a result")
			}
			if item.event != nil {
				outcome, err := emitEvent(*item.event, sink, terminal != nil)
				if err == nil {
					if outcome != nil {
						terminal = outcome
					}
					continue
				}
				close(stopEmit)
				d.terminate(cmd, waitForExit())
				for range items {
				}
				return runtime.Outcome{}, err
			}
			if item.err != nil {
				close(stopEmit)
				d.terminate(cmd, waitForExit())
				return runtime.Outcome{}, item.err
			}
			// StdoutPipe requires all reads to complete before Wait closes the pipe.
			// The parser's nil result is the EOF boundary, so start Wait only now.
			if waitErr := <-waitForExit(); waitErr != nil {
				// Stderr is untrusted provider output and can contain prompt fragments,
				// local paths, or credentials. Drain it into a bounded buffer, but never
				// reflect it across the A2A boundary.
				return runtime.Outcome{}, fmt.Errorf("claude exited with an error: %w", waitErr)
			}
			if terminal == nil {
				return runtime.Outcome{}, fmt.Errorf("claude process exited without a terminal result")
			}
			return *terminal, nil
		case <-ctx.Done():
			close(stopEmit)
			d.terminate(cmd, waitForExit())
			for range items {
			}
			return runtime.Outcome{}, ctx.Err()
		}
	}
}

func (d *ProcessDriver) validateExternalConfiguration() error {
	if d.config.ExternalSettingsPath != "" && strings.TrimSpace(d.config.MCPConfigPath) == "" {
		return fmt.Errorf("external-host Claude requires an activation-scoped MCP configuration")
	}
	return nil
}

func (d *ProcessDriver) processEnvironment() []string {
	environment := append([]string(nil), d.config.Environment...)
	if d.config.ExternalSettingsPath == "" {
		return environment
	}
	// Subscription OAuth remains in the owner-selected CLAUDE_CONFIG_DIR, but
	// automatic memory and first-party claude.ai MCP discovery are independent
	// ambient surfaces and must stay disabled for a cluster-driven invocation.
	environment = replaceEnvironment(environment, disableAutoMemoryEnvironment, disableAutoMemoryEnvironmentVal)
	environment = replaceEnvironment(environment, disableClaudeAIMCPEnvironment, disableClaudeAIMCPEnvironmentVal)
	return environment
}

func replaceEnvironment(environment []string, name, value string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		separator := strings.IndexByte(item, '=')
		if separator >= 0 && strings.EqualFold(item[:separator], name) {
			continue
		}
		result = append(result, item)
	}
	return append(result, name+"="+value)
}

type invocationResources struct {
	root             string
	systemPromptPath string
	agentsPluginPath string
}

func (r invocationResources) Close() {
	if r.root != "" {
		_ = os.RemoveAll(r.root)
	}
}

type generatedAgent struct {
	Description string `json:"description"`
	Prompt      string `json:"prompt"`
	Model       string `json:"model,omitempty"`
}

func prepareInvocationResources(systemPrompt, agentsJSON string) (resources invocationResources, resultErr error) {
	if len(systemPrompt) > maxSystemPromptBytes {
		return invocationResources{}, fmt.Errorf("Claude system prompt exceeds the supported file limit")
	}
	if len(agentsJSON) > maxGeneratedAgentsJSON {
		return invocationResources{}, fmt.Errorf("Claude agents configuration exceeds the supported file limit")
	}
	if systemPrompt == "" && agentsJSON == "" {
		return invocationResources{}, nil
	}

	root, err := os.MkdirTemp("", "kagent-claude-invocation-")
	if err != nil {
		return invocationResources{}, fmt.Errorf("prepare private Claude invocation resources: %w", err)
	}
	resources.root = root
	defer func() {
		if resultErr != nil {
			resources.Close()
			resources = invocationResources{}
		}
	}()
	if err := os.Chmod(root, 0o700); err != nil {
		return resources, fmt.Errorf("protect private Claude invocation resources: %w", err)
	}

	if systemPrompt != "" {
		resources.systemPromptPath = filepath.Join(root, "append-system-prompt.txt")
		if err := os.WriteFile(resources.systemPromptPath, []byte(systemPrompt), 0o600); err != nil {
			return resources, fmt.Errorf("write private Claude system prompt: %w", err)
		}
	}
	if agentsJSON != "" {
		resources.agentsPluginPath = filepath.Join(root, "agents-plugin")
		if err := materializeAgentsPlugin(resources.agentsPluginPath, agentsJSON); err != nil {
			return resources, err
		}
	}
	return resources, nil
}

func materializeAgentsPlugin(root, agentsJSON string) error {
	var agents map[string]generatedAgent
	decoder := json.NewDecoder(strings.NewReader(agentsJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&agents); err != nil {
		return fmt.Errorf("decode Claude agents configuration: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("decode Claude agents configuration: trailing JSON value")
	}
	if len(agents) == 0 {
		return fmt.Errorf("Claude agents configuration must not be empty")
	}

	manifestRoot := filepath.Join(root, ".claude-plugin")
	agentsRoot := filepath.Join(root, "agents")
	for _, directory := range []string{root, manifestRoot, agentsRoot} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("prepare private Claude agents plugin: %w", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("protect private Claude agents plugin: %w", err)
		}
	}
	manifest := []byte(`{"name":"` + generatedAgentsPluginName + `","version":"1.0.0","description":"Compiler-owned invocation-scoped Claude agents"}`)
	if err := os.WriteFile(filepath.Join(manifestRoot, "plugin.json"), manifest, 0o600); err != nil {
		return fmt.Errorf("write private Claude agents plugin manifest: %w", err)
	}

	names := make([]string, 0, len(agents))
	for name := range agents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		agent := agents[name]
		if !generatedAgentNamePattern.MatchString(name) || strings.TrimSpace(agent.Description) == "" || strings.TrimSpace(agent.Prompt) == "" {
			return fmt.Errorf("Claude agents configuration contains an invalid agent")
		}
		contents := strings.Builder{}
		contents.WriteString("---\nname: ")
		contents.WriteString(yamlQuoted(name))
		contents.WriteString("\ndescription: ")
		contents.WriteString(yamlQuoted(agent.Description))
		if agent.Model != "" {
			contents.WriteString("\nmodel: ")
			contents.WriteString(yamlQuoted(agent.Model))
		}
		contents.WriteString("\n---\n\n")
		contents.WriteString(agent.Prompt)
		contents.WriteByte('\n')
		if err := os.WriteFile(filepath.Join(agentsRoot, name+".md"), []byte(contents.String()), 0o600); err != nil {
			return fmt.Errorf("write private Claude agent definition: %w", err)
		}
	}
	return nil
}

func yamlQuoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func emitEvent(event Event, sink runtime.EventSink, terminal bool) (*runtime.Outcome, error) {
	if terminal {
		return nil, fmt.Errorf("claude emitted activity after its terminal result")
	}
	switch event.Kind {
	case EventSessionStarted:
		return nil, sink.SessionStarted(runtime.SessionStarted{ContinuationID: event.SessionID})
	case EventTextDelta:
		return nil, sink.TextDelta(runtime.TextDelta{Text: event.Text})
	case EventToolActivity:
		switch event.ToolPhase {
		case "started":
			return nil, sink.ToolCall(runtime.ToolCall{
				ID: event.ToolID, Name: event.ToolName, Arguments: event.Metadata,
			})
		case "completed":
			return nil, sink.ToolResult(runtime.ToolResult{
				ID: event.ToolID, Name: event.ToolName, Result: event.ToolResult, IsError: event.ToolError,
			})
		default:
			return nil, fmt.Errorf("claude tool activity has unsupported phase %q", event.ToolPhase)
		}
	case EventCompleted:
		return &runtime.Outcome{}, nil
	case EventFailed:
		return &runtime.Outcome{Failure: &runtime.Failure{Message: event.SafeMessage}}, nil
	default:
		return nil, fmt.Errorf("unsupported Claude event kind %q", event.Kind)
	}
}

func (d *ProcessDriver) terminate(cmd *exec.Cmd, waitDone <-chan error) {
	_ = interruptProcessGroup(cmd.Process)
	timer := time.NewTimer(d.config.InterruptGrace)
	defer timer.Stop()
	select {
	case <-waitDone:
		// The group leader can exit on the interrupt while a descendant that
		// ignores it remains alive. Kill any processes still in the group.
		_ = killProcessGroup(cmd.Process)
	case <-timer.C:
		_ = killProcessGroup(cmd.Process)
		<-waitDone
	}
}

type boundedBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.max - b.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return original, nil
}
