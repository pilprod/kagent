// Package adapter constructs the Codex runtime from compiler-owned
// configuration and explicitly selected Actor or external-host paths.
package adapter

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kagent-dev/kagent/go/core/v2/agentplugins"
	"github.com/kagent-dev/kagent/go/harness/codex/config"
	"github.com/kagent-dev/kagent/go/harness/codex/internal/driver"
)

const (
	codexHomeEnvironment      = "CODEX_HOME"
	externalHomeMarker        = ".yourown-chat-managed-codex-home"
	externalPermissionProfile = "yourown_chat_local"
)

// ErrExternalHostCredentialIsolation reports a failed runtime proof that the
// selected permission profile keeps host-managed credentials unreadable to
// Codex-spawned workspace commands.
var ErrExternalHostCredentialIsolation = driver.ErrCredentialIsolation

var harnessControlEnvironment = map[string]struct{}{
	"KAGENT_CONFIG_JSON":              {},
	"KAGENT_AGENT_CARD_JSON":          {},
	"YOUROWN_CHAT_READINESS_TOKEN":    {},
	"YOUROWN_CHAT_TRANSPORT_TOKEN":    {},
	"YOUROWN_CHAT_MANAGED_SLOT_ID":    {},
	"YOUROWN_CHAT_MANAGED_GENERATION": {},
	"YOUROWN_CHAT_MANAGED_ACTIVATION": {},
	"CODEX_BIN":                       {},
}

type Mode string

const (
	// ModeInCluster uses a private CODEX_HOME and workspace below DurableDir.
	// Authentication is an environment-only API key selected by Config.Provider.
	ModeInCluster Mode = "in-cluster"
	// ModeExternalHost uses an existing, isolated, pre-authenticated CODEX_HOME
	// outside DurableDir. Kagent never copies its credentials into snapshot data.
	ModeExternalHost Mode = "external-host"
)

// Input contains compiler output and runtime-owner locations. ExternalHost mode
// requires the caller to deliberately select an isolated CODEX_HOME; the
// process environment's inherited CODEX_HOME is always ignored.
type Input struct {
	ConfigJSON []byte
	Mode       Mode
	Workspace  string
	DurableDir string
	CodexHome  string
	// SkillRepositoryPrefix is the exact OCI repository namespace authorized by
	// local owner policy and derived from the pinned runtime image identity. It
	// never comes from portable compiler input.
	SkillRepositoryPrefix string
	// CodexExecutable is an owner-selected absolute executable override. It is
	// required in external-host mode and forbidden in in-cluster mode.
	CodexExecutable string
	Environment     []string
}

// New validates and materializes Codex-owned state, then proves the selected
// runtime contract before returning a driver. External-host mode starts a
// private app-server probe, but it never performs authentication or exposes
// the app-server protocol to the cluster.
func New(ctx context.Context, input Input) (*driver.ProcessDriver, error) {
	processConfig, err := prepare(ctx, input)
	if err != nil {
		return nil, err
	}
	runner := driver.NewProcessDriver(processConfig)
	if err := runner.Validate(ctx); err != nil {
		return nil, err
	}
	return runner, nil
}

func prepare(ctx context.Context, input Input) (driver.ProcessConfig, error) {
	var cfg config.Config
	var err error
	switch input.Mode {
	case ModeInCluster:
		cfg, err = config.Parse(input.ConfigJSON)
	case ModeExternalHost:
		cfg, err = config.ParsePortableExternalForRepositoryPrefix(input.ConfigJSON, input.SkillRepositoryPrefix)
	default:
		return driver.ProcessConfig{}, fmt.Errorf("unsupported adapter mode %q", input.Mode)
	}
	if err != nil {
		return driver.ProcessConfig{}, err
	}
	if err := validateLocations(input, cfg); err != nil {
		return driver.ProcessConfig{}, err
	}

	if input.Mode == ModeInCluster {
		if err := requireEnvironment(input.Environment, cfg.Provider.APIKeyEnv); err != nil {
			return driver.ProcessConfig{}, err
		}
	} else {
		for _, name := range []string{
			config.DefaultAPIKeyEnv, config.DefaultAccessTokenEnv,
			"OPENAI_BASE_URL", "OPENAI_ORGANIZATION", "OPENAI_ORG_ID", "OPENAI_PROJECT", "OPENAI_PROJECT_ID",
		} {
			if _, found, duplicate := environmentValue(input.Environment, name); found || duplicate {
				return driver.ProcessConfig{}, fmt.Errorf("external-host mode uses its pre-authenticated CODEX_HOME; %s must not be injected", name)
			}
		}
	}
	for serverName, server := range cfg.MCPServers {
		for header, value := range server.Headers {
			environmentName, ok := config.HeaderEnvironment(value)
			if !ok {
				continue
			}
			if err := requireEnvironment(input.Environment, environmentName); err != nil {
				return driver.ProcessConfig{}, fmt.Errorf("codex MCP server %q header %q: %w", serverName, header, err)
			}
		}
	}

	if err := ensurePrivateDir(input.DurableDir); err != nil {
		return driver.ProcessConfig{}, fmt.Errorf("prepare durable directory: %w", err)
	}
	if input.Mode == ModeInCluster {
		if err := ensurePrivateDir(input.Workspace); err != nil {
			return driver.ProcessConfig{}, fmt.Errorf("prepare durable workspace: %w", err)
		}
		if err := ensurePrivateDir(input.CodexHome); err != nil {
			return driver.ProcessConfig{}, fmt.Errorf("prepare private CODEX_HOME: %w", err)
		}
	} else {
		if err := requireDirectory(input.Workspace, false); err != nil {
			return driver.ProcessConfig{}, fmt.Errorf("validate external workspace: %w", err)
		}
		if err := requireDirectory(input.CodexHome, true); err != nil {
			return driver.ProcessConfig{}, fmt.Errorf("validate pre-authenticated CODEX_HOME: %w", err)
		}
		if err := requireExternalHomeMarker(input.CodexHome); err != nil {
			return driver.ProcessConfig{}, err
		}
		if err := requirePrivateRegularFile(filepath.Join(input.CodexHome, "auth.json"), false); err != nil {
			return driver.ProcessConfig{}, fmt.Errorf("validate pre-authenticated Codex credential file: %w", err)
		}
		if pathsOverlap(input.DurableDir, input.CodexHome) {
			return driver.ProcessConfig{}, fmt.Errorf("external durable directory and CODEX_HOME must not overlap")
		}
		if pathsOverlap(input.Workspace, input.CodexHome) {
			return driver.ProcessConfig{}, fmt.Errorf("external workspace and CODEX_HOME must not overlap")
		}
		if pathsOverlap(input.Workspace, input.DurableDir) {
			return driver.ProcessConfig{}, fmt.Errorf("external workspace and activation-scoped durable directory must not overlap")
		}
	}

	generatedRoot := filepath.Join(input.DurableDir, "generated-codex")
	if err := resetGeneratedRoot(input.DurableDir, generatedRoot); err != nil {
		return driver.ProcessConfig{}, fmt.Errorf("reset compiler-owned Codex resources: %w", err)
	}
	packagesRoot := filepath.Join(generatedRoot, "packages")
	skillsRoot := filepath.Join(generatedRoot, "skills")
	if cfg.SkillResources != nil {
		materializeSkills := agentplugins.MaterializeSkills
		if input.Mode == ModeExternalHost {
			materializeSkills = agentplugins.MaterializeExternalSkills
		}
		if err := materializeSkills(ctx, *cfg.SkillResources, agentplugins.SkillPaths{
			Plugins: packagesRoot, Skills: skillsRoot,
		}); err != nil {
			return driver.ProcessConfig{}, fmt.Errorf("materialize Codex skills: %w", err)
		}
	} else if err := ensurePrivateDir(skillsRoot); err != nil {
		return driver.ProcessConfig{}, fmt.Errorf("prepare generated Codex skills: %w", err)
	}

	contents, err := renderCodexConfig(cfg, input.Workspace, input.CodexHome, skillsRoot)
	if err != nil {
		return driver.ProcessConfig{}, fmt.Errorf("render Codex configuration: %w", err)
	}
	if err := replacePrivateFile(filepath.Join(input.CodexHome, "config.toml"), contents); err != nil {
		return driver.ProcessConfig{}, fmt.Errorf("materialize compiler-owned Codex configuration: %w", err)
	}

	environment := stripEnvironment(input.Environment, harnessControlEnvironment)
	environment = setEnvironment(environment, codexHomeEnvironment, input.CodexHome)
	executable := cfg.CodexExecutable
	if input.CodexExecutable != "" {
		executable = input.CodexExecutable
	}
	sandbox := driver.SandboxPolicy{ExternalSandbox: &driver.ExternalSandboxPolicy{NetworkAccess: cfg.NetworkAccess}}
	credentialReadProbe := ""
	if input.Mode == ModeExternalHost {
		sandbox = driver.SandboxPolicy{PermissionProfile: &driver.PermissionProfilePolicy{ID: externalPermissionProfile}}
		credentialReadProbe = filepath.Join(input.CodexHome, "auth.json")
	}
	return driver.ProcessConfig{
		Executable: executable, ExpectedVersion: cfg.ExpectedCodexVersion,
		StrictVersion: cfg.StrictVersion, Workspace: input.Workspace,
		Model: cfg.Model, ModelProvider: cfg.ModelProvider,
		ReasoningEffort: cfg.ReasoningEffort, ServiceTier: cfg.ServiceTier,
		DeveloperInstructions: cfg.DeveloperInstructions,
		SandboxPolicy:         sandbox, CredentialReadProbe: credentialReadProbe, Environment: environment,
		MaxEventBytes: cfg.MaxEventBytes, MaxStderrBytes: cfg.MaxStderrBytes,
		HandshakeTimeout: cfg.HandshakeTimeout(), ShutdownGrace: cfg.ShutdownGrace(),
	}, nil
}

func validateLocations(input Input, cfg config.Config) error {
	for name, path := range map[string]string{
		"workspace": input.Workspace, "durable directory": input.DurableDir, "CODEX_HOME": input.CodexHome,
	} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	switch input.Mode {
	case ModeInCluster:
		if input.CodexExecutable != "" {
			return fmt.Errorf("in-cluster mode does not accept a Codex executable override")
		}
		if cfg.Provider == nil {
			return fmt.Errorf("in-cluster mode requires an environment-backed custom model provider")
		}
		if !samePath(input.Workspace, filepath.Join(input.DurableDir, "workspace")) {
			return fmt.Errorf("in-cluster workspace must be %q", filepath.Join(input.DurableDir, "workspace"))
		}
		if !samePath(input.CodexHome, filepath.Join(input.DurableDir, "codex")) {
			return fmt.Errorf("in-cluster CODEX_HOME must be %q", filepath.Join(input.DurableDir, "codex"))
		}
	case ModeExternalHost:
		if cfg.Provider != nil || cfg.ModelProvider != "openai" {
			return fmt.Errorf("external-host mode requires pre-authenticated built-in OpenAI provider configuration")
		}
		if cfg.NetworkAccess != config.NetworkRestricted {
			return fmt.Errorf("external-host mode requires restricted command network access")
		}
		if pathWithin(input.DurableDir, input.CodexHome) {
			return fmt.Errorf("pre-authenticated CODEX_HOME must be outside the snapshotted durable directory")
		}
		if !filepath.IsAbs(input.CodexExecutable) || filepath.Clean(input.CodexExecutable) != input.CodexExecutable {
			return fmt.Errorf("external-host Codex executable must be an absolute normalized path selected by local owner policy")
		}
	default:
		return fmt.Errorf("unsupported adapter mode %q", input.Mode)
	}
	return nil
}

func requireExternalHomeMarker(codexHome string) error {
	path := filepath.Join(codexHome, externalHomeMarker)
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("validate managed CODEX_HOME marker: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != 0 {
		return fmt.Errorf("managed CODEX_HOME marker must be an empty private regular file")
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

func requireDirectory(path string, private bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	if private && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%q must not be accessible by group or other users", path)
	}
	return nil
}

func resetGeneratedRoot(durableRoot, generatedRoot string) error {
	if !pathWithin(durableRoot, generatedRoot) || samePath(durableRoot, generatedRoot) {
		return fmt.Errorf("generated resource root %q escapes activation durable directory", generatedRoot)
	}
	if err := os.RemoveAll(generatedRoot); err != nil {
		return err
	}
	return ensurePrivateDir(generatedRoot)
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
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func requireEnvironment(environment []string, name string) error {
	value, found, duplicate := environmentValue(environment, name)
	if duplicate {
		return fmt.Errorf("%s is configured more than once", name)
	}
	if !found || strings.TrimSpace(value) == "" {
		return fmt.Errorf("in-cluster model provider requires non-empty %s", name)
	}
	return nil
}

func environmentValue(environment []string, name string) (value string, found, duplicate bool) {
	prefix := name + "="
	for _, item := range environment {
		if strings.HasPrefix(item, prefix) {
			if found {
				duplicate = true
			}
			found = true
			value = strings.TrimPrefix(item, prefix)
		}
	}
	return value, found, duplicate
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

func stripEnvironment(environment []string, names map[string]struct{}) []string {
	result := make([]string, 0, len(environment))
	for _, item := range environment {
		name, _, found := strings.Cut(item, "=")
		if _, blocked := names[name]; found && blocked {
			continue
		}
		result = append(result, item)
	}
	return result
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathsOverlap(left, right string) bool {
	canonicalLeft, leftErr := filepath.EvalSymlinks(left)
	canonicalRight, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		left, right = canonicalLeft, canonicalRight
	}
	return pathWithin(left, right) || pathWithin(right, left)
}
