package codingagent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Compiler renders the common, credential-free config contract for one
// coding-agent runtime. It intentionally remains unregistered until a pinned
// image implements the corresponding materializer and private A2A service.
type Compiler struct {
	runtime Runtime
	kube    v2translator.Reader
}

var _ v2translator.HarnessCompiler = (*Compiler)(nil)

// NewCompiler constructs a compiler. Compile rejects an unknown runtime before
// inspecting or serializing any input.
func NewCompiler(runtime Runtime, kube v2translator.Reader) *Compiler {
	return &Compiler{runtime: runtime, kube: kube}
}

type compiledAgent struct {
	config    AgentConfig
	templates []*v1alpha3.AgentTemplate
	models    []*v1alpha3.ModelConfig
	servers   []*v1alpha3.RemoteMCPServer
	egress    []string
}

// Compile produces an immutable Revision but performs no workload mutation.
func (c *Compiler) Compile(ctx context.Context, input *v2translator.HarnessInput) (*v2translator.Revision, error) {
	if err := c.validateInput(input); err != nil {
		return nil, err
	}
	compiled, err := c.compileAgent(input.Root, input.Harness.Namespace)
	if err != nil {
		return nil, err
	}
	config := Config{Version: ConfigVersion, Runtime: c.runtime, Root: compiled.config}
	if err := config.Validate(); err != nil {
		return nil, v2translator.NewValidationError("invalid %s runtime config: %v", c.runtime, err)
	}
	configJSON, err := json.Marshal(config)
	if err != nil {
		return nil, fmt.Errorf("marshal %s runtime config: %w", c.runtime, err)
	}
	if len(configJSON) > MaxConfigBytes {
		return nil, v2translator.NewValidationError("%s runtime config is %d bytes; maximum is %d", c.runtime, len(configJSON), MaxConfigBytes)
	}
	if _, err := Decode(configJSON); err != nil {
		return nil, fmt.Errorf("validate generated %s runtime config: %w", c.runtime, err)
	}

	cardJSON, err := json.Marshal(agentCard(input.Root.Template))
	if err != nil {
		return nil, fmt.Errorf("marshal %s Agent Card: %w", c.runtime, err)
	}
	provenance, err := c.buildProvenance(ctx, input.Harness, compiled)
	if err != nil {
		return nil, fmt.Errorf("build %s revision provenance: %w", c.runtime, err)
	}
	slices.Sort(compiled.egress)
	compiled.egress = slices.Compact(compiled.egress)

	return &v2translator.Revision{
		Namespace:          input.Root.Template.Namespace,
		AgentTemplateName:  input.Root.Template.Name,
		HarnessName:        input.Harness.Name,
		Image:              input.Harness.Spec.Workload.Image,
		Environment:        nil,
		ConfigJSON:         configJSON,
		AgentCardJSON:      cardJSON,
		WorkerPoolName:     input.Harness.Spec.Substrate.WorkerPoolRef.Name,
		SnapshotLocation:   input.Harness.Spec.Substrate.SnapshotPolicy.Location,
		Provenance:         provenance,
		EgressDestinations: compiled.egress,
	}, nil
}

func (c *Compiler) validateInput(input *v2translator.HarnessInput) error {
	if c == nil {
		return v2translator.NewValidationError("coding-agent Harness compiler is nil")
	}
	if c.runtime != RuntimeCodex && c.runtime != RuntimeClaude {
		return v2translator.NewValidationError("unsupported coding-agent runtime %q", c.runtime)
	}
	if input == nil || input.Harness == nil {
		return v2translator.NewValidationError("coding-agent Harness compiler requires a Harness")
	}
	if input.Harness.Namespace == "" || input.Harness.Name == "" {
		return v2translator.NewValidationError("coding-agent Harness requires a name and namespace")
	}
	if input.Root == nil || input.Root.Template == nil || input.Root.ModelConfig == nil {
		return v2translator.NewValidationError("coding-agent Harness compiler requires a resolved root AgentTemplate and ModelConfig")
	}
	if input.Root.Template.Namespace != input.Harness.Namespace {
		return v2translator.NewValidationError("Harness and AgentTemplate must be in the same namespace")
	}
	if (c.runtime == RuntimeCodex && (input.Harness.Spec.Codex == nil || input.Harness.Spec.Claude != nil || input.Harness.Spec.Kagent != nil)) ||
		(c.runtime == RuntimeClaude && (input.Harness.Spec.Claude == nil || input.Harness.Spec.Codex != nil || input.Harness.Spec.Kagent != nil)) {
		return v2translator.NewValidationError("%s compiler does not match selected Harness runtime", c.runtime)
	}
	if !ociDigestPattern.MatchString(input.Harness.Spec.Workload.Image) || input.Harness.Spec.Substrate.WorkerPoolRef.Name == "" || input.Harness.Spec.Substrate.SnapshotPolicy.Location == "" {
		return v2translator.NewValidationError("%s Harness requires pinned workload and Substrate policy", c.runtime)
	}
	if len(input.Harness.Spec.Env) != 0 {
		return v2translator.NewValidationError("coding-agent Harness v1 does not support environment variables")
	}
	return nil
}

func (c *Compiler) compileAgent(input *v2translator.AgentInput, namespace string) (*compiledAgent, error) {
	if input == nil || input.Template == nil || input.ModelConfig == nil {
		return nil, v2translator.NewValidationError("%s compiler requires a resolved AgentTemplate and ModelConfig", c.runtime)
	}
	if input.Template.Namespace != namespace || input.ModelConfig.Namespace != namespace {
		return nil, v2translator.NewValidationError("Harness, AgentTemplate, and ModelConfig must be in the same non-empty namespace")
	}
	model, modelEgress, err := c.compileModel(input.ModelConfig)
	if err != nil {
		return nil, fmt.Errorf("compile ModelConfig %q: %w", input.ModelConfig.Name, err)
	}
	mcp, mcpServers, mcpEgress, err := compileMCP(input)
	if err != nil {
		return nil, err
	}
	skills, plugins, artifactEgress, err := compileArtifacts(input.Template)
	if err != nil {
		return nil, err
	}
	result := &compiledAgent{
		config: AgentConfig{
			TemplateName: input.Template.Name,
			Description:  input.Template.Spec.Description,
			Instruction:  input.Instruction,
			Model:        model,
			MCPServers:   mcp,
			Skills:       skills,
			Plugins:      plugins,
		},
		templates: []*v1alpha3.AgentTemplate{input.Template},
		models:    []*v1alpha3.ModelConfig{input.ModelConfig},
		servers:   mcpServers,
		egress:    append(append(modelEgress, mcpEgress...), artifactEgress...),
	}
	for _, binding := range input.Shared {
		child, err := c.compileAgent(binding.Agent, namespace)
		if err != nil {
			return nil, err
		}
		result.config.SharedAgents = append(result.config.SharedAgents, SharedBinding{
			Name: binding.Name, Description: binding.Description, Agent: child.config,
		})
		result.templates = append(result.templates, child.templates...)
		result.models = append(result.models, child.models...)
		result.servers = append(result.servers, child.servers...)
		result.egress = append(result.egress, child.egress...)
	}
	slices.SortFunc(result.config.SharedAgents, func(left, right SharedBinding) int {
		return strings.Compare(left.Name, right.Name)
	})
	return result, nil
}

func (c *Compiler) compileModel(model *v1alpha3.ModelConfig) (ModelConfig, []string, error) {
	spec := &model.Spec
	if spec.Model == "" {
		return ModelConfig{}, nil, v2translator.NewValidationError("model name is required")
	}
	if spec.APIKeySecret != "" || spec.APIKeySecretKey != "" || spec.APIKeyPassthrough {
		return ModelConfig{}, nil, v2translator.NewValidationError("coding-agent adapters do not materialize ModelConfig credentials")
	}
	if len(spec.DefaultHeaders) != 0 {
		return ModelConfig{}, nil, v2translator.NewValidationError("coding-agent adapters do not serialize ModelConfig default headers")
	}
	if spec.TLS != nil && !spec.TLS.IsEmpty() {
		return ModelConfig{}, nil, v2translator.NewValidationError("coding-agent adapters do not support ModelConfig TLS yet")
	}

	provider := spec.Provider
	if provider == "" {
		provider = v1alpha3.ModelProviderOpenAI
	}
	switch c.runtime {
	case RuntimeCodex:
		if provider != v1alpha3.ModelProviderOpenAI {
			return ModelConfig{}, nil, v2translator.NewValidationError("Codex requires an OpenAI ModelConfig, got %q", provider)
		}
		if hasProviderConfigOtherThan(spec, "openAI") {
			return ModelConfig{}, nil, v2translator.NewValidationError("Codex ModelConfig contains configuration for another provider")
		}
		effort := ""
		if spec.OpenAI != nil {
			allowed := &v1alpha3.OpenAIConfig{ReasoningEffort: spec.OpenAI.ReasoningEffort}
			if spec.OpenAI.APIFormat != nil && *spec.OpenAI.APIFormat == v1alpha3.OpenAIAPIFormatChatCompletions {
				allowed.APIFormat = spec.OpenAI.APIFormat
			}
			if !reflect.DeepEqual(spec.OpenAI, allowed) {
				return ModelConfig{}, nil, v2translator.NewValidationError("Codex v1 supports only OpenAI reasoningEffort; provider request options are runtime-owned")
			}
			if spec.OpenAI.ReasoningEffort != nil {
				effort = string(*spec.OpenAI.ReasoningEffort)
			}
		}
		return ModelConfig{Provider: string(provider), Name: spec.Model, ReasoningEffort: effort}, []string{"api.openai.com"}, nil
	case RuntimeClaude:
		if provider != v1alpha3.ModelProviderAnthropic {
			return ModelConfig{}, nil, v2translator.NewValidationError("Claude requires an Anthropic ModelConfig, got %q", provider)
		}
		if hasProviderConfigOtherThan(spec, "anthropic") {
			return ModelConfig{}, nil, v2translator.NewValidationError("Claude ModelConfig contains configuration for another provider")
		}
		if spec.Anthropic != nil && !reflect.DeepEqual(*spec.Anthropic, v1alpha3.AnthropicConfig{}) {
			return ModelConfig{}, nil, v2translator.NewValidationError("Claude v1 supports only model selection; provider request options are runtime-owned")
		}
		return ModelConfig{Provider: string(provider), Name: spec.Model}, []string{"api.anthropic.com"}, nil
	default:
		return ModelConfig{}, nil, v2translator.NewValidationError("unsupported coding-agent runtime %q", c.runtime)
	}
}

func hasProviderConfigOtherThan(spec *v1alpha3.ModelConfigSpec, allowed string) bool {
	configs := map[string]any{
		"openAI": spec.OpenAI, "anthropic": spec.Anthropic, "azureOpenAI": spec.AzureOpenAI,
		"ollama": spec.Ollama, "gemini": spec.Gemini, "geminiVertexAI": spec.GeminiVertexAI,
		"anthropicVertexAI": spec.AnthropicVertexAI, "bedrock": spec.Bedrock,
		"sapAICore": spec.SAPAICore, "foundry": spec.Foundry,
	}
	for name, config := range configs {
		if name != allowed && !reflect.ValueOf(config).IsNil() {
			return true
		}
	}
	return false
}

func compileMCP(input *v2translator.AgentInput) ([]MCPBinding, []*v1alpha3.RemoteMCPServer, []string, error) {
	byServer := map[string]*MCPBinding{}
	servers := map[string]*v1alpha3.RemoteMCPServer{}
	var egress []string
	for _, resolved := range input.MCPTools {
		name := resolved.Binding.Server.Name
		if resolved.Binding.Server.Kind != "RemoteMCPServer" || name == "" || resolved.Server == nil || resolved.Server.Name != name {
			return nil, nil, nil, v2translator.NewValidationError("coding-agent config contains an invalid logical MCP server")
		}
		if resolved.Server.Namespace != input.Template.Namespace {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q must share the AgentTemplate namespace", name)
		}
		if len(resolved.Server.Spec.HeadersFrom) != 0 {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q headersFrom requires credential relay support", name)
		}
		if resolved.Server.Spec.TLS != nil && !resolved.Server.Spec.TLS.IsEmpty() {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q TLS requires runtime trust injection support", name)
		}
		protocol := resolved.Server.Spec.Protocol
		if protocol == "" {
			protocol = v1alpha3.RemoteMCPServerProtocolStreamableHttp
		}
		connection := MCPConnection{Transport: string(protocol), URL: resolved.Server.Spec.URL}
		if resolved.Server.Spec.Timeout != nil {
			connection.Timeout = resolved.Server.Spec.Timeout.Duration.String()
		}
		if resolved.Server.Spec.SseReadTimeout != nil {
			connection.SSEReadTimeout = resolved.Server.Spec.SseReadTimeout.Duration.String()
		}
		if resolved.Server.Spec.TerminateOnClose != nil {
			value := *resolved.Server.Spec.TerminateOnClose
			connection.TerminateOnClose = &value
		}
		if err := validateHTTPURL(connection.URL); err != nil {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q URL %v", name, err)
		}
		binding := byServer[name]
		if binding == nil {
			binding = &MCPBinding{Server: name, Connection: connection}
			byServer[name] = binding
			servers[name] = resolved.Server
		} else if !reflect.DeepEqual(binding.Connection, connection) {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q resolved to conflicting connections", name)
		}
		if len(resolved.Binding.Tools) == 0 {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q requires an explicit tool allowlist", name)
		}
		binding.Tools = append(binding.Tools, resolved.Binding.Tools...)
		parsed, _ := url.Parse(connection.URL)
		egress = append(egress, parsed.Hostname())
	}

	names := make([]string, 0, len(byServer))
	for name := range byServer {
		names = append(names, name)
	}
	slices.Sort(names)
	bindings := make([]MCPBinding, 0, len(names))
	serverObjects := make([]*v1alpha3.RemoteMCPServer, 0, len(names))
	for _, name := range names {
		binding := byServer[name]
		if slices.Contains(binding.Tools, "") {
			return nil, nil, nil, v2translator.NewValidationError("RemoteMCPServer %q contains an empty tool name", name)
		}
		slices.Sort(binding.Tools)
		binding.Tools = slices.Compact(binding.Tools)
		bindings = append(bindings, *binding)
		serverObjects = append(serverObjects, servers[name])
	}
	return bindings, serverObjects, egress, nil
}

func compileArtifacts(template *v1alpha3.AgentTemplate) ([]Skill, []Plugin, []string, error) {
	selected := map[string]struct{}{}
	skills := make([]Skill, 0, len(template.Spec.Skills))
	plugins := make([]Plugin, 0, len(template.Spec.Plugins))
	var egress []string
	for _, skill := range template.Spec.Skills {
		if _, exists := selected[skill.Name]; exists {
			return nil, nil, nil, v2translator.NewValidationError("duplicate skill name %q", skill.Name)
		}
		selected[skill.Name] = struct{}{}
		source := artifactSource(skill.Source)
		skills = append(skills, Skill{Name: skill.Name, Source: source})
		egress = append(egress, artifactDestinations(source)...)
	}
	for _, bundle := range template.Spec.Plugins {
		selection := append([]string(nil), bundle.Skills...)
		slices.Sort(selection)
		for i, name := range selection {
			if i > 0 && selection[i-1] == name {
				return nil, nil, nil, v2translator.NewValidationError("duplicate skill name %q", name)
			}
			if _, exists := selected[name]; exists {
				return nil, nil, nil, v2translator.NewValidationError("duplicate skill name %q", name)
			}
			selected[name] = struct{}{}
		}
		source := artifactSource(bundle.Source)
		plugins = append(plugins, Plugin{Source: source, Skills: selection})
		egress = append(egress, artifactDestinations(source)...)
	}
	slices.SortFunc(skills, func(left, right Skill) int { return strings.Compare(left.Name, right.Name) })
	slices.SortFunc(plugins, func(left, right Plugin) int {
		leftJSON, _ := json.Marshal(left)
		rightJSON, _ := json.Marshal(right)
		return strings.Compare(string(leftJSON), string(rightJSON))
	})
	return skills, plugins, egress, nil
}

func artifactSource(source v1alpha3.ArtifactSource) ArtifactSource {
	result := ArtifactSource{OCI: source.OCI, Path: source.Path}
	if source.Git != nil {
		result.Git = &GitArtifact{URL: source.Git.URL, Commit: source.Git.Commit}
	}
	if source.Bucket != nil {
		s3 := source.Bucket.S3
		result.Bucket = &BucketArtifact{S3: S3Object{
			Endpoint: s3.Endpoint, Bucket: s3.Bucket, Key: s3.Key, VersionID: s3.VersionID, Region: s3.Region,
		}}
	}
	return result
}

func artifactDestinations(source ArtifactSource) []string {
	switch {
	case source.Git != nil:
		return urlHostname(source.Git.URL)
	case source.Bucket != nil:
		return urlHostname(source.Bucket.S3.Endpoint)
	case source.OCI != "":
		repository := strings.SplitN(source.OCI, "@", 2)[0]
		first, _, found := strings.Cut(repository, "/")
		if found && (strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost") {
			return []string{first}
		}
		return []string{"registry-1.docker.io"}
	default:
		return nil
	}
}

func urlHostname(raw string) []string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	return []string{parsed.Hostname()}
}

type provenanceEntry struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid,omitempty"`
	Generation int64     `json:"generation,omitempty"`
	Hash       string    `json:"hash"`
}

func (c *Compiler) buildProvenance(ctx context.Context, harness *v1alpha3.Harness, compiled *compiledAgent) ([]byte, error) {
	entries := []provenanceEntry{objectProvenance(v1alpha3.GroupVersion.String(), "Harness", harness.Name, harness.UID, harness.Generation, harness.Spec)}
	configMaps := map[string]struct{}{}
	for _, template := range compiled.templates {
		entries = append(entries, objectProvenance(v1alpha3.GroupVersion.String(), "AgentTemplate", template.Name, template.UID, template.Generation, template.Spec))
		if template.Spec.SystemPromptFrom != nil {
			configMaps[template.Spec.SystemPromptFrom.Name] = struct{}{}
		}
		if template.Spec.PromptTemplate != nil {
			for _, source := range template.Spec.PromptTemplate.DataSources {
				configMaps[source.Name] = struct{}{}
			}
		}
	}
	for _, model := range compiled.models {
		entries = append(entries, objectProvenance(v1alpha3.GroupVersion.String(), "ModelConfig", model.Name, model.UID, model.Generation, model.Spec))
	}
	for _, server := range compiled.servers {
		entries = append(entries, objectProvenance(v1alpha3.GroupVersion.String(), "RemoteMCPServer", server.Name, server.UID, server.Generation, server.Spec))
	}
	for name := range configMaps {
		if c.kube == nil {
			return nil, fmt.Errorf("reader is required to resolve ConfigMap provenance")
		}
		configMap := &corev1.ConfigMap{}
		if err := c.kube.Get(ctx, types.NamespacedName{Namespace: harness.Namespace, Name: name}, configMap); err != nil {
			return nil, err
		}
		entries = append(entries, objectProvenance("v1", "ConfigMap", name, configMap.UID, configMap.Generation, configMap.Data))
	}
	slices.SortFunc(entries, func(left, right provenanceEntry) int {
		return strings.Compare(left.APIVersion+"\x00"+left.Kind+"\x00"+left.Name, right.APIVersion+"\x00"+right.Kind+"\x00"+right.Name)
	})
	entries = slices.CompactFunc(entries, func(left, right provenanceEntry) bool {
		return left.APIVersion == right.APIVersion && left.Kind == right.Kind && left.Name == right.Name && left.Hash == right.Hash
	})
	return json.Marshal(entries)
}

func objectProvenance(apiVersion, kind, name string, uid types.UID, generation int64, content any) provenanceEntry {
	raw, _ := json.Marshal(content)
	hash := sha256.Sum256(raw)
	return provenanceEntry{
		APIVersion: apiVersion, Kind: kind, Name: name, UID: uid, Generation: generation,
		Hash: fmt.Sprintf("%x", hash[:]),
	}
}

func agentCard(template *v1alpha3.AgentTemplate) *a2atype.AgentCard {
	return &a2atype.AgentCard{
		Name:        strings.ReplaceAll(template.Name, "-", "_"),
		Description: template.Spec.Description,
		Version:     ConfigVersion,
		SupportedInterfaces: []*a2atype.AgentInterface{a2atype.NewAgentInterface(
			"http://127.0.0.1:80", a2atype.TransportProtocolGRPC,
		)},
		Capabilities:       a2atype.AgentCapabilities{Streaming: false},
		Skills:             []a2atype.AgentSkill{},
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
	}
}
