// Package adapter constructs the Claude runtime from compiler-owned
// configuration and Actor-owned paths.
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/kagent-dev/kagent/go/core/v2/agentplugins"
	"github.com/kagent-dev/kagent/go/harness/claude/config"
	"github.com/kagent-dev/kagent/go/harness/claude/internal/driver"
)

const (
	claudeConfigDirEnv              = "CLAUDE_CONFIG_DIR"
	disableUpdatesEnv               = "DISABLE_UPDATES"
	googleApplicationCredentialsEnv = "GOOGLE_APPLICATION_CREDENTIALS"
	externalHomeMarker              = ".yourown-chat-managed-claude-home"
)

var harnessControlEnvironment = map[string]struct{}{
	"KAGENT_CONFIG_JSON":              {},
	"KAGENT_AGENT_CARD_JSON":          {},
	"YOUROWN_CHAT_READINESS_TOKEN":    {},
	"YOUROWN_CHAT_TRANSPORT_TOKEN":    {},
	"YOUROWN_CHAT_MANAGED_SLOT_ID":    {},
	"YOUROWN_CHAT_MANAGED_GENERATION": {},
	"YOUROWN_CHAT_MANAGED_ACTIVATION": {},
	"CLAUDE_BIN":                      {},
}

type Mode string

const (
	ModeInCluster    Mode = "in-cluster"
	ModeExternalHost Mode = "external-host"
)

// Input contains compiler output and Actor-owned locations used to construct
// the Claude driver.
type Input struct {
	ConfigJSON   []byte
	Mode         Mode
	Workspace    string
	DurableDir   string
	EphemeralDir string
	// ClaudeConfigDir and ClaudeExecutable are local-owner selections. They are
	// required in external-host mode and cannot be sourced from ConfigJSON.
	ClaudeConfigDir  string
	ClaudeExecutable string
	// SkillRepositoryPrefix is the exact OCI repository namespace authorized by
	// local owner policy and derived from the pinned runtime image identity. It
	// never comes from portable compiler input.
	SkillRepositoryPrefix string
	Environment           []string
}

// New validates and materializes Claude-owned state, then constructs its driver.
func New(ctx context.Context, input Input) (*driver.ProcessDriver, error) {
	var cfg config.Config
	var err error
	if input.Mode == ModeExternalHost {
		cfg, err = config.ParsePortableExternal(input.ConfigJSON, input.SkillRepositoryPrefix)
	} else {
		cfg, err = config.Parse(input.ConfigJSON)
	}
	if err != nil {
		return nil, err
	}
	agentsJSON, err := cfg.AgentsJSON()
	if err != nil {
		return nil, err
	}
	mcpJSON, err := cfg.MCPConfigJSON()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(input.Workspace) || !filepath.IsAbs(input.DurableDir) || !filepath.IsAbs(input.EphemeralDir) {
		return nil, fmt.Errorf("workspace, durable, and ephemeral directories must be absolute paths")
	}
	if input.Mode == "" {
		input.Mode = ModeInCluster
	}
	claudeDir := filepath.Join(input.DurableDir, "claude")
	external := input.Mode == ModeExternalHost
	if external {
		if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
			return nil, fmt.Errorf("Claude external-host mode requires Linux or macOS")
		}
		if len(cfg.Agents) != 0 || len(cfg.MCPServers) != 0 {
			return nil, fmt.Errorf("Claude external-host MCP and Shared resources are not materialized by this ABI version")
		}
		if !filepath.IsAbs(input.ClaudeExecutable) || filepath.Clean(input.ClaudeExecutable) != input.ClaudeExecutable {
			return nil, fmt.Errorf("external-host Claude executable must be an absolute normalized path selected by local owner policy")
		}
		if !filepath.IsAbs(input.ClaudeConfigDir) || filepath.Clean(input.ClaudeConfigDir) != input.ClaudeConfigDir {
			return nil, fmt.Errorf("external-host CLAUDE_CONFIG_DIR must be an absolute normalized path selected by local owner policy")
		}
		if pathsOverlap(input.DurableDir, input.ClaudeConfigDir) || pathsOverlap(input.Workspace, input.ClaudeConfigDir) {
			return nil, fmt.Errorf("external workspace, durable directory, and CLAUDE_CONFIG_DIR must not overlap")
		}
		if err := requireDirectory(input.Workspace, false); err != nil {
			return nil, fmt.Errorf("validate external workspace: %w", err)
		}
		if err := requireDirectory(input.ClaudeConfigDir, true); err != nil {
			return nil, fmt.Errorf("validate pre-authenticated CLAUDE_CONFIG_DIR: %w", err)
		}
		if err := requirePrivateRegularFile(filepath.Join(input.ClaudeConfigDir, externalHomeMarker), true); err != nil {
			return nil, fmt.Errorf("validate managed CLAUDE_CONFIG_DIR marker: %w", err)
		}
		if runtime.GOOS == "linux" {
			if err := requirePrivateRegularFile(filepath.Join(input.ClaudeConfigDir, ".credentials.json"), false); err != nil {
				return nil, fmt.Errorf("validate pre-authenticated Claude credential file: %w", err)
			}
		}
		for _, name := range []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_TOKEN",
			config.GoogleCredentialsJSONEnvName, googleApplicationCredentialsEnv,
		} {
			if _, found, duplicate := environmentValue(input.Environment, name); found || duplicate {
				return nil, fmt.Errorf("external-host mode uses its pre-authenticated CLAUDE_CONFIG_DIR; %s must not be injected", name)
			}
		}
		claudeDir = input.ClaudeConfigDir
		if err := ensurePrivateDir(input.DurableDir); err != nil {
			return nil, fmt.Errorf("prepare external durable directory: %w", err)
		}
	} else if input.Mode == ModeInCluster {
		if input.ClaudeConfigDir != "" || input.ClaudeExecutable != "" {
			return nil, fmt.Errorf("in-cluster mode does not accept Claude owner path overrides")
		}
		for _, directory := range []struct{ name, path string }{
			{name: "workspace", path: input.Workspace},
			{name: "Claude state", path: claudeDir},
		} {
			if err := ensurePrivateDir(directory.path); err != nil {
				return nil, fmt.Errorf("prepare %s directory: %w", directory.name, err)
			}
		}
	} else {
		return nil, fmt.Errorf("unsupported Claude adapter mode %q", input.Mode)
	}
	if external && pathsOverlap(input.Workspace, input.DurableDir) {
		return nil, fmt.Errorf("external workspace and activation-scoped durable directory must not overlap")
	}
	generatedRoot := filepath.Join(input.DurableDir, "generated-claude")
	if err := resetGeneratedRoot(input.DurableDir, generatedRoot); err != nil {
		return nil, fmt.Errorf("reset activation-scoped Claude resources: %w", err)
	}
	pluginDir := ""
	if cfg.SkillResources != nil {
		pluginDir = filepath.Join(generatedRoot, "portable-skills-plugin")
		materializeSkills := agentplugins.MaterializeSkills
		if external {
			materializeSkills = agentplugins.MaterializeExternalSkills
		}
		if err := materializeSkills(ctx, *cfg.SkillResources, agentplugins.SkillPaths{
			Plugins: filepath.Join(generatedRoot, "packages"),
			Skills:  filepath.Join(pluginDir, "skills"),
		}); err != nil {
			return nil, fmt.Errorf("materialize Claude skills: %w", err)
		}
		manifestDirectory := filepath.Join(pluginDir, ".claude-plugin")
		if err := ensurePrivateDir(manifestDirectory); err != nil {
			return nil, fmt.Errorf("prepare Claude skill plugin manifest directory: %w", err)
		}
		manifest, err := json.Marshal(struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Version     string `json:"version"`
		}{Name: "kagent-portable-v2", Description: "Activation-scoped skills selected by kagent", Version: "1.0.0"})
		if err != nil {
			return nil, fmt.Errorf("encode Claude skill plugin manifest: %w", err)
		}
		if err := replacePrivateFile(filepath.Join(manifestDirectory, "plugin.json"), manifest); err != nil {
			return nil, fmt.Errorf("materialize Claude skill plugin manifest: %w", err)
		}
	}
	environment := stripEnvironment(input.Environment, harnessControlEnvironment)
	environment = setEnvironment(environment, claudeConfigDirEnv, claudeDir)
	// The image and compiler pin an exact Claude version. Prevent both automatic
	// and manual update paths from changing that runtime after validation.
	environment = setEnvironment(environment, disableUpdatesEnv, "1")
	if !external {
		environment, err = materializeGoogleCredentials(environment, input.EphemeralDir)
		if err != nil {
			return nil, err
		}
	}
	var mcpConfigPath string
	if external {
		// --strict-mcp-config still consults its explicit file. Select an exact
		// activation-scoped empty document so ambient MCP discovery can never be
		// substituted when the portable ABI (correctly) contains no MCP grants.
		mcpConfigPath = filepath.Join(generatedRoot, "mcp.json")
		if err := replacePrivateFile(mcpConfigPath, []byte(`{"mcpServers":{}}`)); err != nil {
			return nil, fmt.Errorf("materialize Claude external-host empty MCP configuration: %w", err)
		}
	} else if len(mcpJSON) != 0 {
		if err := ensurePrivateDir(input.EphemeralDir); err != nil {
			return nil, fmt.Errorf("prepare ephemeral MCP directory: %w", err)
		}
		mcpConfigPath = filepath.Join(input.EphemeralDir, "mcp.json")
		if err := replacePrivateFile(mcpConfigPath, mcpJSON); err != nil {
			return nil, fmt.Errorf("materialize Claude MCP configuration: %w", err)
		}
	}
	executable := cfg.ClaudeExecutable
	settingsPath := ""
	if external {
		executable = input.ClaudeExecutable
		settingsPath = filepath.Join(input.DurableDir, "external-settings.json")
		settings, settingsErr := externalSettings(input.ClaudeConfigDir)
		if settingsErr != nil {
			return nil, settingsErr
		}
		if err := replacePrivateFile(settingsPath, settings); err != nil {
			return nil, fmt.Errorf("materialize Claude external-host settings: %w", err)
		}
	}
	return driver.NewProcessDriver(driver.ProcessConfig{
		Executable: executable, ExpectedVersion: cfg.ExpectedClaudeVersion,
		StrictVersion: cfg.StrictVersion, Workspace: input.Workspace, Model: cfg.Model,
		AppendSystemPrompt: cfg.AppendSystemPrompt, AgentsJSON: agentsJSON, MCPConfigPath: mcpConfigPath, Environment: environment,
		ExternalSettingsPath: settingsPath, RequireAuthStatus: external,
		PluginDirs:    optionalPath(pluginDir),
		MaxEventBytes: cfg.MaxEventBytes, MaxStderrBytes: cfg.MaxStderrBytes,
		InterruptGrace: cfg.InterruptGrace(),
	}), nil
}

func optionalPath(path string) []string {
	if path == "" {
		return nil
	}
	return []string{path}
}

func resetGeneratedRoot(parent, generatedRoot string) error {
	if !pathWithin(parent, generatedRoot) || filepath.Clean(parent) == filepath.Clean(generatedRoot) {
		return fmt.Errorf("generated resource root %q escapes its activation directory", generatedRoot)
	}
	if err := os.RemoveAll(generatedRoot); err != nil {
		return err
	}
	return ensurePrivateDir(generatedRoot)
}

func externalSettings(claudeConfigDir string) ([]byte, error) {
	permissionPath := "//" + strings.TrimPrefix(claudeConfigDir, string(filepath.Separator)) + "/**"
	settings := struct {
		Permissions struct {
			DefaultMode                  string   `json:"defaultMode"`
			DisableBypassPermissionsMode string   `json:"disableBypassPermissionsMode"`
			Deny                         []string `json:"deny"`
		} `json:"permissions"`
		DisableAllHooks bool `json:"disableAllHooks"`
		Sandbox         struct {
			Enabled                  bool `json:"enabled"`
			AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
			FailIfUnavailable        bool `json:"failIfUnavailable"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
			Filesystem               struct {
				DenyRead  []string `json:"denyRead"`
				DenyWrite []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}{}
	settings.Permissions.DefaultMode = "acceptEdits"
	settings.Permissions.DisableBypassPermissionsMode = "disable"
	settings.Permissions.Deny = []string{"Read(" + permissionPath + ")", "Edit(" + permissionPath + ")"}
	settings.DisableAllHooks = true
	settings.Sandbox.Enabled = true
	settings.Sandbox.AutoAllowBashIfSandboxed = true
	settings.Sandbox.FailIfUnavailable = true
	settings.Sandbox.AllowUnsandboxedCommands = false
	settings.Sandbox.Filesystem.DenyRead = []string{claudeConfigDir}
	settings.Sandbox.Filesystem.DenyWrite = []string{claudeConfigDir}
	raw, err := json.Marshal(settings)
	if err != nil {
		return nil, fmt.Errorf("encode Claude external-host settings: %w", err)
	}
	return raw, nil
}

func requireDirectory(path string, private bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q must be a directory and not a symlink", path)
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%q must be owner-only", path)
	}
	return nil
}

func requirePrivateRegularFile(path string, empty bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%q must be a private regular file", path)
	}
	if empty && info.Size() != 0 {
		return fmt.Errorf("%q must be empty", path)
	}
	return nil
}

func pathsOverlap(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func environmentValue(environment []string, name string) (string, bool, bool) {
	prefix := name + "="
	value := ""
	found := false
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			if found {
				return "", true, true
			}
			value = strings.TrimPrefix(item, prefix)
			found = true
		}
	}
	return value, found, false
}

func stripEnvironment(environment []string, blocked map[string]struct{}) []string {
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		name, _, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		if _, deny := blocked[name]; !deny {
			result = append(result, item)
		}
	}
	return result
}

func replacePrivateFile(path string, contents []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func materializeGoogleCredentials(environment []string, directory string) ([]string, error) {
	// The compiler injects the Secret value as JSON, while Google ADC expects a
	// file path. Keep the credential in ephemeral Actor storage rather than the
	// well-known path under /data, which is durable and may be snapshotted.
	prefix := config.GoogleCredentialsJSONEnvName + "="
	var credentials string
	filtered := make([]string, 0, len(environment))
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			if credentials != "" {
				return nil, fmt.Errorf("%s is configured more than once", config.GoogleCredentialsJSONEnvName)
			}
			credentials = strings.TrimPrefix(item, prefix)
			continue
		}
		filtered = append(filtered, item)
	}
	if credentials == "" {
		return filtered, nil
	}
	if !json.Valid([]byte(credentials)) {
		return nil, fmt.Errorf("%s must contain valid JSON", config.GoogleCredentialsJSONEnvName)
	}
	if err := ensurePrivateDir(directory); err != nil {
		return nil, fmt.Errorf("prepare ephemeral credentials directory: %w", err)
	}
	path := filepath.Join(directory, "google-credentials.json")
	temporary, err := os.CreateTemp(directory, ".google-credentials-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("create temporary Google credentials: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("secure temporary Google credentials: %w", err)
	}
	if _, err := temporary.WriteString(credentials); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("materialize Google credentials: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close Google credentials: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return nil, fmt.Errorf("replace Google credentials: %w", err)
	}
	return setEnvironment(filtered, googleApplicationCredentialsEnv, path), nil
}

func ensurePrivateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	return os.Chmod(path, 0o700)
}

func setEnvironment(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}
