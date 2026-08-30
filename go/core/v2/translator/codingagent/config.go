package codingagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// ConfigVersion identifies the JSON contract shared by the controller and
	// pinned coding-agent runtime images.
	ConfigVersion = "v2"
	// MaxConfigBytes stays below Linux's per-environment-entry limit because
	// Substrate currently injects ConfigJSON through one environment variable.
	MaxConfigBytes = 96 << 10
	maxAgentDepth  = 1
	maxJSONDepth   = 32
)

// Runtime selects the materializer used by a pinned coding-agent image.
type Runtime string

const (
	RuntimeCodex  Runtime = "codex"
	RuntimeClaude Runtime = "claude"
)

// Config is the credential-free, immutable input to a coding-agent runtime.
type Config struct {
	Version string      `json:"version"`
	Runtime Runtime     `json:"runtime"`
	Root    AgentConfig `json:"root"`
}

// AgentConfig preserves one AgentTemplate independently of the parent-specific
// name and description used for a Shared binding.
type AgentConfig struct {
	TemplateName string          `json:"templateName"`
	Description  string          `json:"description,omitempty"`
	Instruction  string          `json:"instruction,omitempty"`
	Model        ModelConfig     `json:"model"`
	MCPGrants    []MCPGrant      `json:"mcpGrants,omitempty"`
	Skills       []Skill         `json:"skills,omitempty"`
	Plugins      []Plugin        `json:"plugins,omitempty"`
	SharedAgents []SharedBinding `json:"sharedAgents,omitempty"`
}

// ModelConfig contains only CLI-portable model selection. Authentication and
// provider-specific request tuning are runtime-owned and never serialized.
type ModelConfig struct {
	Provider        string `json:"provider"`
	Name            string `json:"name"`
	ReasoningEffort string `json:"reasoningEffort,omitempty"`
	ServiceTier     string `json:"serviceTier,omitempty"`
}

// MCPGrant is a logical, content-addressed grant reference exposed through the
// local host's loopback MCP proxy. It contains neither an upstream URL nor a
// credential. The host receives a short-lived bearer capability separately
// over the authenticated Substrate assignment channel.
type MCPGrant struct {
	ID    string   `json:"id"`
	Tools []string `json:"tools"`
}

// Skill selects one standalone skill from an immutable artifact.
type Skill struct {
	Name   string         `json:"name"`
	Source ArtifactSource `json:"source"`
}

// Plugin selects skills from one immutable Agent Plugins package.
type Plugin struct {
	Source ArtifactSource `json:"source"`
	Skills []string       `json:"skills,omitempty"`
}

// SharedBinding preserves the parent-specific native sub-agent identity.
type SharedBinding struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Agent       AgentConfig `json:"agent"`
}

// ArtifactSource is a frozen copy of the v1 immutable artifact union. Keeping
// this wire type separate prevents future CRD additions from silently changing
// an existing runtime config version.
type ArtifactSource struct {
	OCI    string          `json:"oci,omitempty"`
	Git    *GitArtifact    `json:"git,omitempty"`
	Bucket *BucketArtifact `json:"bucket,omitempty"`
	Path   string          `json:"path,omitempty"`
}

// GitArtifact identifies immutable content at a full Git commit.
type GitArtifact struct {
	URL    string `json:"url"`
	Commit string `json:"commit"`
}

// BucketArtifact selects one versioned S3 object.
type BucketArtifact struct {
	S3 S3Object `json:"s3"`
}

// S3Object identifies an immutable object-store version.
type S3Object struct {
	Endpoint  string `json:"endpoint"`
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"versionId"`
	Region    string `json:"region,omitempty"`
}

var (
	gitCommitPattern    = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
	ociDigestPattern    = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
	templateNamePattern = regexp.MustCompile(`^[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?$`)
	runtimeNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	mcpGrantIDPattern   = regexp.MustCompile(`^mcp-[0-9a-f]{64}$`)
)

// Decode strictly decodes and validates one runtime config document.
func Decode(raw []byte) (*Config, error) {
	if len(raw) == 0 || len(raw) > MaxConfigBytes {
		return nil, fmt.Errorf("coding-agent config size must be between 1 and %d bytes", MaxConfigBytes)
	}
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("coding-agent config must be valid UTF-8")
	}
	if err := rejectDuplicateKeys(raw); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode coding-agent config: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("encode canonical coding-agent config: %w", err)
	}
	if !bytes.Equal(raw, canonical) {
		return nil, fmt.Errorf("coding-agent config is not canonical JSON")
	}
	return &config, nil
}

// Validate checks the complete portable contract without consulting external
// state. Runtime consumers must call it before materializing files or argv.
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("coding-agent config is required")
	}
	if c.Version != ConfigVersion {
		return fmt.Errorf("unsupported coding-agent config version %q", c.Version)
	}
	if c.Runtime != RuntimeCodex && c.Runtime != RuntimeClaude {
		return fmt.Errorf("unsupported coding-agent runtime %q", c.Runtime)
	}
	if err := validateAgent(c.Runtime, &c.Root, 0, make(map[string]string)); err != nil {
		return err
	}
	return nil
}

func validateAgent(runtime Runtime, agent *AgentConfig, depth int, seenGrantIDs map[string]string) error {
	if depth > maxAgentDepth {
		return fmt.Errorf("shared agent depth exceeds %d", maxAgentDepth)
	}
	if agent.TemplateName == "" {
		return fmt.Errorf("agent templateName is required")
	}
	if len(agent.TemplateName) > 253 || !templateNamePattern.MatchString(agent.TemplateName) {
		return fmt.Errorf("agent templateName %q is not a valid Kubernetes object name", agent.TemplateName)
	}
	if agent.Model.Name == "" {
		return fmt.Errorf("agent %q model name is required", agent.TemplateName)
	}
	if err := validateIdentifier(agent.Model.Name, 256); err != nil {
		return fmt.Errorf("agent %q model name: %w", agent.TemplateName, err)
	}
	wantProvider := "OpenAI"
	if runtime == RuntimeClaude {
		wantProvider = "Anthropic"
	}
	if agent.Model.Provider != wantProvider {
		return fmt.Errorf("agent %q provider %q is incompatible with %s", agent.TemplateName, agent.Model.Provider, runtime)
	}
	if runtime == RuntimeClaude && (agent.Model.ReasoningEffort != "" || agent.Model.ServiceTier != "") {
		return fmt.Errorf("agent %q reasoningEffort and serviceTier are not supported by claude", agent.TemplateName)
	}
	if runtime == RuntimeCodex && agent.Model.ReasoningEffort != "" &&
		!slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, agent.Model.ReasoningEffort) {
		return fmt.Errorf("agent %q has invalid reasoningEffort %q", agent.TemplateName, agent.Model.ReasoningEffort)
	}
	if runtime == RuntimeCodex && agent.Model.ServiceTier != "" && agent.Model.ServiceTier != "fast" {
		return fmt.Errorf("agent %q has invalid serviceTier %q", agent.TemplateName, agent.Model.ServiceTier)
	}

	for i := range agent.MCPGrants {
		grant := &agent.MCPGrants[i]
		if !mcpGrantIDPattern.MatchString(grant.ID) {
			return fmt.Errorf("agent %q MCP grant ID %q is invalid", agent.TemplateName, grant.ID)
		}
		if i > 0 && agent.MCPGrants[i-1].ID >= grant.ID {
			return fmt.Errorf("agent %q MCP grants must be sorted and unique", agent.TemplateName)
		}
		if owner, duplicate := seenGrantIDs[grant.ID]; duplicate {
			return fmt.Errorf("agent %q MCP grant %q duplicates a grant owned by agent %q", agent.TemplateName, grant.ID, owner)
		}
		seenGrantIDs[grant.ID] = agent.TemplateName
		if len(grant.Tools) == 0 {
			return fmt.Errorf("agent %q MCP grant %q requires an explicit tool allowlist", agent.TemplateName, grant.ID)
		}
		for toolIndex, tool := range grant.Tools {
			if err := validateMCPToolName(tool); err != nil {
				return fmt.Errorf("agent %q MCP grant %q tool name: %w", agent.TemplateName, grant.ID, err)
			}
			if toolIndex > 0 && grant.Tools[toolIndex-1] >= tool {
				return fmt.Errorf("agent %q MCP grant %q tools must be sorted and unique", agent.TemplateName, grant.ID)
			}
		}
	}

	selectedSkills := map[string]struct{}{}
	for i := range agent.Skills {
		skill := &agent.Skills[i]
		if i > 0 && agent.Skills[i-1].Name >= skill.Name {
			return fmt.Errorf("agent %q standalone skills must be sorted and unique", agent.TemplateName)
		}
		if err := validateSkillName(skill.Name); err != nil {
			return fmt.Errorf("agent %q: %w", agent.TemplateName, err)
		}
		if _, exists := selectedSkills[skill.Name]; exists {
			return fmt.Errorf("agent %q has duplicate skill %q", agent.TemplateName, skill.Name)
		}
		selectedSkills[skill.Name] = struct{}{}
		if err := skill.Source.validate(); err != nil {
			return fmt.Errorf("agent %q skill %q source: %w", agent.TemplateName, skill.Name, err)
		}
	}
	var previousPlugin []byte
	for i := range agent.Plugins {
		plugin := &agent.Plugins[i]
		pluginJSON, err := json.Marshal(plugin)
		if err != nil {
			return fmt.Errorf("agent %q plugin %d: %w", agent.TemplateName, i, err)
		}
		if i > 0 && bytes.Compare(previousPlugin, pluginJSON) >= 0 {
			return fmt.Errorf("agent %q plugins must be sorted and unique", agent.TemplateName)
		}
		previousPlugin = pluginJSON
		if err := plugin.Source.validate(); err != nil {
			return fmt.Errorf("agent %q plugin %d source: %w", agent.TemplateName, i, err)
		}
		for skillIndex, name := range plugin.Skills {
			if skillIndex > 0 && plugin.Skills[skillIndex-1] >= name {
				return fmt.Errorf("agent %q plugin %d skill selection must be sorted and unique", agent.TemplateName, i)
			}
			if err := validateSkillName(name); err != nil {
				return fmt.Errorf("agent %q: %w", agent.TemplateName, err)
			}
			if _, exists := selectedSkills[name]; exists {
				return fmt.Errorf("agent %q has duplicate skill %q", agent.TemplateName, name)
			}
			selectedSkills[name] = struct{}{}
		}
	}

	bindings := map[string]struct{}{}
	for i := range agent.SharedAgents {
		binding := &agent.SharedAgents[i]
		if i > 0 && agent.SharedAgents[i-1].Name >= binding.Name {
			return fmt.Errorf("agent %q Shared bindings must be sorted and unique", agent.TemplateName)
		}
		if binding.Name == "" || binding.Description == "" {
			return fmt.Errorf("agent %q Shared binding name and description are required", agent.TemplateName)
		}
		if !runtimeNamePattern.MatchString(binding.Name) {
			return fmt.Errorf("agent %q Shared binding %q is not a safe runtime name", agent.TemplateName, binding.Name)
		}
		if _, exists := bindings[binding.Name]; exists {
			return fmt.Errorf("agent %q has duplicate Shared binding %q", agent.TemplateName, binding.Name)
		}
		bindings[binding.Name] = struct{}{}
		if err := validateAgent(runtime, &binding.Agent, depth+1, seenGrantIDs); err != nil {
			return err
		}
	}
	return nil
}

func (s ArtifactSource) validate() error {
	selected := 0
	if s.OCI != "" {
		selected++
		if !ociDigestPattern.MatchString(s.OCI) {
			return fmt.Errorf("OCI source is not digest-pinned")
		}
	}
	if s.Git != nil {
		selected++
		if err := validateHTTPURL(s.Git.URL); err != nil {
			return fmt.Errorf("git URL: %w", err)
		}
		if !gitCommitPattern.MatchString(s.Git.Commit) {
			return fmt.Errorf("git source is not pinned to a full commit")
		}
	}
	if s.Bucket != nil {
		selected++
		object := s.Bucket.S3
		if err := validateHTTPURL(object.Endpoint); err != nil {
			return fmt.Errorf("S3 endpoint: %w", err)
		}
		if object.Bucket == "" || object.Key == "" || object.VersionID == "" {
			return fmt.Errorf("S3 bucket, key, and versionId are required")
		}
	}
	if selected != 1 {
		return fmt.Errorf("exactly one immutable artifact source is required")
	}
	if len(s.Path) > 1024 {
		return fmt.Errorf("path exceeds 1024 bytes")
	}
	if s.Path != "" && (s.Path == "." || path.Clean(s.Path) != s.Path || path.IsAbs(s.Path) || strings.Contains(s.Path, `\`) || slices.Contains(strings.Split(s.Path, "/"), "..")) {
		return fmt.Errorf("path must be canonical, relative, and must not contain '..' segments")
	}
	return nil
}

func validateHTTPURL(raw string) error {
	if len(raw) == 0 || len(raw) > 4096 || strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return fmt.Errorf("must be between 1 and 4096 bytes without control characters")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("must not contain userinfo")
	}
	return nil
}

func validateIdentifier(value string, maximum int) error {
	if value == "" || len(value) > maximum {
		return fmt.Errorf("must be between 1 and %d bytes", maximum)
	}
	if strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) >= 0 {
		return fmt.Errorf("must not contain control or whitespace characters")
	}
	return nil
}

func validateMCPToolName(value string) error {
	if value == "" || len(value) > 128 {
		return fmt.Errorf("must be between 1 and 128 bytes")
	}
	for _, character := range value {
		valid := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.'
		if !valid {
			return fmt.Errorf("contains invalid characters")
		}
	}
	return nil
}

func validateSkillName(name string) error {
	if name == "" || len(name) > 128 || name == "." || name == ".." || strings.ContainsAny(name, `/\\`) || strings.IndexFunc(name, func(value rune) bool { return value < 0x20 || value == 0x7f }) >= 0 {
		return fmt.Errorf("skill name %q must be one relative path component", name)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode coding-agent config: %w", err)
	}
	return fmt.Errorf("coding-agent config contains more than one JSON value")
}

func rejectDuplicateKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) error
	walk = func(depth int) error {
		token, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("decode coding-agent config: %w", err)
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		if depth >= maxJSONDepth {
			return fmt.Errorf("coding-agent config exceeds maximum JSON depth %d", maxJSONDepth)
		}
		switch delimiter {
		case '{':
			seen := map[string]struct{}{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return fmt.Errorf("decode coding-agent config: %w", err)
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("decode coding-agent config: object key is not a string")
				}
				if _, exists := seen[key]; exists {
					return fmt.Errorf("coding-agent config contains duplicate key %q", key)
				}
				seen[key] = struct{}{}
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(depth + 1); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("decode coding-agent config: unexpected delimiter %q", delimiter)
		}
	}
	if err := walk(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("coding-agent config contains more than one JSON value")
		}
		return fmt.Errorf("decode coding-agent config: %w", err)
	}
	return nil
}
