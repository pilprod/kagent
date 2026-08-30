// Package config defines the versioned, non-secret Codex Harness runtime
// configuration shared by its compiler and Actor entrypoint.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/kagent-dev/kagent/go/api/agentplugin"
)

const (
	Version               = 1
	PinnedCodexVersion    = "0.151.0"
	NetworkRestricted     = "restricted"
	NetworkEnabled        = "enabled"
	MCPTypeHTTP           = "http"
	MCPTypeStreamable     = "streamable-http"
	DefaultModelProvider  = "kagent-openai"
	DefaultAPIKeyEnv      = "OPENAI_API_KEY"
	DefaultAccessTokenEnv = "CODEX_ACCESS_TOKEN"
	DefaultOpenAIBaseURL  = "https://api.openai.com/v1"
	defaultMaxEvent       = 1 << 20
	defaultMaxStderr      = 64 << 10
	defaultHandshakeMS    = 10_000
	defaultShutdownMS     = 2_000
)

var (
	namePattern              = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	environmentNamePattern   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	headerEnvironmentPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)
)

// Config is compiler output. It deliberately contains no credentials: secret
// MCP headers are represented by exact ${ENVIRONMENT_VARIABLE} references.
type Config struct {
	Version               int                    `json:"version"`
	CodexExecutable       string                 `json:"codex_executable"`
	ExpectedCodexVersion  string                 `json:"expected_codex_version"`
	StrictVersion         bool                   `json:"strict_version"`
	Model                 string                 `json:"model"`
	ModelProvider         string                 `json:"model_provider"`
	Provider              *Provider              `json:"provider,omitempty"`
	ReasoningEffort       string                 `json:"reasoning_effort,omitempty"`
	ServiceTier           string                 `json:"service_tier,omitempty"`
	DeveloperInstructions string                 `json:"developer_instructions,omitempty"`
	SkillResources        *agentplugin.Resources `json:"skill_resources,omitempty"`
	MCPServers            map[string]MCPServer   `json:"mcp_servers,omitempty"`
	NetworkAccess         string                 `json:"network_access"`
	MaxEventBytes         int                    `json:"max_event_bytes"`
	MaxStderrBytes        int                    `json:"max_stderr_bytes"`
	HandshakeTimeoutMS    int                    `json:"handshake_timeout_millis"`
	ShutdownGraceMS       int                    `json:"shutdown_grace_millis"`
}

// MCPServer is one compiler-owned streamable HTTP MCP server. Headers whose
// value is exactly ${NAME} are emitted as Codex env_http_headers entries.
type MCPServer struct {
	Type               string            `json:"type"`
	URL                string            `json:"url"`
	Headers            map[string]string `json:"headers,omitempty"`
	Tools              []string          `json:"tools,omitempty"`
	StartupTimeoutMS   int               `json:"startup_timeout_millis,omitempty"`
	ToolTimeoutSeconds int               `json:"tool_timeout_seconds,omitempty"`
}

// Provider defines a compiler-owned Responses-compatible model provider. The
// API key itself remains in the named environment variable and never enters
// ConfigJSON or Codex's durable home. A nil Provider selects a built-in provider
// such as "openai", which permits a deliberately pre-authenticated host home.
type Provider struct {
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	APIKeyEnv string `json:"api_key_env"`
}

// Production constructs the pinned in-cluster defaults. The compiler may add
// immutable skills and direct MCP servers before validating and serializing it.
func Production(model, effort, instructions string) Config {
	return Config{
		Version:              Version,
		CodexExecutable:      "codex",
		ExpectedCodexVersion: PinnedCodexVersion,
		StrictVersion:        true,
		Model:                model,
		ModelProvider:        DefaultModelProvider,
		Provider: &Provider{
			Name: "OpenAI", BaseURL: DefaultOpenAIBaseURL, APIKeyEnv: DefaultAPIKeyEnv,
		},
		ReasoningEffort:       effort,
		DeveloperInstructions: instructions,
		NetworkAccess:         NetworkRestricted,
		MaxEventBytes:         defaultMaxEvent,
		MaxStderrBytes:        defaultMaxStderr,
		HandshakeTimeoutMS:    defaultHandshakeMS,
		ShutdownGraceMS:       defaultShutdownMS,
	}
}

// Preauthenticated constructs configuration for an explicitly selected,
// host-managed CODEX_HOME. It uses Codex's built-in OpenAI provider and never
// places subscription credentials in ConfigJSON.
func Preauthenticated(model, effort, instructions string) Config {
	cfg := Production(model, effort, instructions)
	cfg.ModelProvider = "openai"
	cfg.Provider = nil
	return cfg
}

// Parse decodes exactly one strict JSON value and validates its runtime
// contract. Unknown fields are rejected so old Actors cannot silently ignore a
// newly compiled security or resource setting.
func Parse(contents []byte) (Config, error) {
	var cfg Config
	decoder := json.NewDecoder(strings.NewReader(string(contents)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Config{}, fmt.Errorf("decode config: trailing JSON value")
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("unsupported config version %d (want %d)", c.Version, Version)
	}
	if strings.TrimSpace(c.CodexExecutable) == "" {
		return fmt.Errorf("codex_executable is required")
	}
	if c.StrictVersion && strings.TrimSpace(c.ExpectedCodexVersion) == "" {
		return fmt.Errorf("expected_codex_version is required when strict_version is enabled")
	}
	if strings.TrimSpace(c.Model) == "" {
		return fmt.Errorf("model is required")
	}
	if !namePattern.MatchString(c.ModelProvider) {
		return fmt.Errorf("model_provider %q must contain only letters, numbers, underscores, or hyphens", c.ModelProvider)
	}
	if c.Provider != nil {
		if c.ModelProvider == "openai" || c.ModelProvider == "ollama" || c.ModelProvider == "lmstudio" {
			return fmt.Errorf("custom provider cannot override reserved model_provider %q", c.ModelProvider)
		}
		if strings.TrimSpace(c.Provider.Name) == "" {
			return fmt.Errorf("provider name is required")
		}
		parsed, err := url.Parse(c.Provider.BaseURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return fmt.Errorf("provider base_url must be an absolute HTTP(S) URL without credentials, query, or fragment")
		}
		if !environmentNamePattern.MatchString(c.Provider.APIKeyEnv) {
			return fmt.Errorf("provider api_key_env %q is not an environment variable name", c.Provider.APIKeyEnv)
		}
	} else if c.ModelProvider != "openai" {
		return fmt.Errorf("pre-authenticated configuration requires built-in model_provider %q", "openai")
	}
	if c.ReasoningEffort != "" && !validReasoningEffort(c.ReasoningEffort) {
		return fmt.Errorf("unsupported reasoning_effort %q", c.ReasoningEffort)
	}
	if c.ServiceTier != "" && c.ServiceTier != "fast" {
		return fmt.Errorf("unsupported service_tier %q", c.ServiceTier)
	}
	if c.NetworkAccess != NetworkRestricted && c.NetworkAccess != NetworkEnabled {
		return fmt.Errorf("network_access must be %q or %q", NetworkRestricted, NetworkEnabled)
	}
	if c.MaxEventBytes <= 0 || c.MaxStderrBytes <= 0 || c.HandshakeTimeoutMS <= 0 || c.ShutdownGraceMS <= 0 {
		return fmt.Errorf("event, stderr, handshake, and shutdown limits must be positive")
	}
	for name, server := range c.MCPServers {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("codex MCP server name %q must contain only letters, numbers, underscores, or hyphens", name)
		}
		if server.Type != MCPTypeHTTP && server.Type != MCPTypeStreamable {
			return fmt.Errorf("codex MCP server %q has unsupported transport %q", name, server.Type)
		}
		parsed, err := url.Parse(server.URL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
			return fmt.Errorf("codex MCP server %q URL must be an absolute HTTP(S) URL without credentials, query, or fragment", name)
		}
		for header, value := range server.Headers {
			if strings.TrimSpace(header) == "" || strings.TrimSpace(value) == "" {
				return fmt.Errorf("codex MCP server %q headers require non-empty names and values", name)
			}
			if SensitiveHeader(header) {
				if _, ok := HeaderEnvironment(value); !ok {
					return fmt.Errorf("codex MCP server %q credential header %q must use an environment reference", name, header)
				}
			}
			if strings.Contains(value, "${") {
				if _, ok := HeaderEnvironment(value); !ok {
					return fmt.Errorf("codex MCP server %q header %q has an invalid environment reference", name, header)
				}
			}
		}
		seenTools := make(map[string]struct{}, len(server.Tools))
		for _, tool := range server.Tools {
			if strings.TrimSpace(tool) == "" {
				return fmt.Errorf("codex MCP server %q tool names must be non-empty", name)
			}
			if _, exists := seenTools[tool]; exists {
				return fmt.Errorf("codex MCP server %q selects tool %q more than once", name, tool)
			}
			seenTools[tool] = struct{}{}
		}
		if server.StartupTimeoutMS < 0 || server.ToolTimeoutSeconds < 0 {
			return fmt.Errorf("codex MCP server %q timeouts must not be negative", name)
		}
	}
	return nil
}

// HeaderEnvironment returns the environment variable selected by an exact
// ${NAME} header value. Mixed literal/variable interpolation is intentionally
// unsupported because Codex's env_http_headers contract names a whole value.
func HeaderEnvironment(value string) (string, bool) {
	match := headerEnvironmentPattern.FindStringSubmatch(value)
	if len(match) != 2 {
		return "", false
	}
	return match[1], true
}

// SensitiveHeader reports header names whose values may carry authentication
// material. Such values must remain environment-backed instead of entering the
// compiler-owned ConfigJSON or the generated Codex configuration.
func SensitiveHeader(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch normalized {
	case "authorization", "proxy-authorization", "cookie", "set-cookie", "api-key", "x-api-key":
		return true
	}
	for _, part := range strings.FieldsFunc(normalized, func(r rune) bool { return r == '-' || r == '_' }) {
		switch part {
		case "auth", "authentication", "key", "password", "signature", "token", "secret", "credential", "credentials":
			return true
		}
	}
	compact := strings.NewReplacer("-", "", "_", "").Replace(normalized)
	for _, marker := range []string{"authorization", "authentication", "apikey", "accesstoken", "clientsecret"} {
		if strings.Contains(compact, marker) {
			return true
		}
	}
	return false
}

func (c Config) HandshakeTimeout() time.Duration {
	return time.Duration(c.HandshakeTimeoutMS) * time.Millisecond
}

func (c Config) ShutdownGrace() time.Duration {
	return time.Duration(c.ShutdownGraceMS) * time.Millisecond
}

func validReasoningEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
		return true
	default:
		return false
	}
}
