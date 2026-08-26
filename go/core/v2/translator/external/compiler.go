package external

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/api/v1alpha3"
	v2translator "github.com/kagent-dev/kagent/go/core/v2/translator"
)

const profileVersion = "v1"

// Compiler translates one external Harness runtime into a sanitized profile.
// Model selection remains authoritative at the external runtime and therefore
// is deliberately absent from the persisted profile and revision digest.
type Compiler struct {
	runtime dbpkg.ExternalRuntime
}

var _ v2translator.HarnessCompiler = (*Compiler)(nil)

type profile struct {
	Version     string        `json:"version"`
	Instruction string        `json:"instruction"`
	Tools       []profileTool `json:"tools"`
}

type profileTool struct {
	Server string   `json:"server"`
	Allow  []string `json:"allow"`
}

type provenance struct {
	Version       string        `json:"version"`
	Harness       resourceRef   `json:"harness"`
	AgentTemplate resourceRef   `json:"agentTemplate"`
	MCPServers    []resourceRef `json:"mcpServers"`
}

type resourceRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
}

// NewCompiler constructs a compiler for one supported external runtime.
func NewCompiler(runtime dbpkg.ExternalRuntime) (*Compiler, error) {
	if runtime != dbpkg.ExternalRuntimeCodex && runtime != dbpkg.ExternalRuntimeClaude {
		return nil, fmt.Errorf("external Harness compiler runtime is not supported")
	}
	return &Compiler{runtime: runtime}, nil
}

// Compile emits only the resolved instruction and logical MCP tool allowlist.
// Local runtime capabilities, credentials, endpoints, and execution profile
// choices are owned and validated by the connected client.
func (c *Compiler) Compile(_ context.Context, input *v2translator.HarnessInput) (*v2translator.Revision, error) {
	if err := c.validateInput(input); err != nil {
		return nil, err
	}
	tools, err := compileTools(input.Root.MCPTools)
	if err != nil {
		return nil, err
	}
	externalProfile, err := json.Marshal(profile{Version: profileVersion, Instruction: input.Root.Instruction, Tools: tools})
	if err != nil {
		return nil, fmt.Errorf("marshal external runtime profile: %w", err)
	}
	agentCard, err := json.Marshal(externalAgentCard(input.Root.Template))
	if err != nil {
		return nil, fmt.Errorf("marshal external runtime Agent Card: %w", err)
	}
	sourceSnapshot, err := json.Marshal(externalProvenance(input))
	if err != nil {
		return nil, fmt.Errorf("marshal external runtime provenance: %w", err)
	}

	return &v2translator.Revision{
		Namespace:          input.Root.Template.Namespace,
		AgentTemplateName:  input.Root.Template.Name,
		HarnessName:        input.Harness.Name,
		BackendKind:        dbpkg.RuntimeBackendKindExternal,
		ExternalRuntime:    c.runtime,
		ExternalProfile:    externalProfile,
		AgentCardJSON:      agentCard,
		Provenance:         sourceSnapshot,
		EgressDestinations: []string{},
	}, nil
}

func (c *Compiler) validateInput(input *v2translator.HarnessInput) error {
	if c == nil {
		return v2translator.NewValidationError("external Harness compiler is nil")
	}
	if input == nil || input.Harness == nil {
		return v2translator.NewValidationError("external Harness compiler requires a Harness")
	}
	if input.Root == nil || input.Root.Template == nil {
		return v2translator.NewValidationError("external Harness compiler requires a resolved root AgentTemplate")
	}
	if !harnessSelectsRuntime(input.Harness, c.runtime) {
		return v2translator.NewValidationError("external Harness compiler runtime does not match the Harness")
	}
	if input.Harness.Spec.Workload != nil || input.Harness.Spec.Substrate != nil || input.Harness.Spec.Env != nil {
		return v2translator.NewValidationError("external Harnesses do not support workload, substrate, or env")
	}
	if len(input.Root.Shared) != 0 {
		return v2translator.NewValidationError("external Harness profiles do not support Shared AgentTemplate tools yet")
	}
	if len(input.Root.Template.Spec.Skills) != 0 {
		return v2translator.NewValidationError("external Harness profiles do not support AgentTemplate skills yet")
	}
	if len(input.Root.Template.Spec.Plugins) != 0 {
		return v2translator.NewValidationError("external Harness profiles do not support AgentTemplate plugins yet")
	}
	return nil
}

func harnessSelectsRuntime(harness *v1alpha3.Harness, runtime dbpkg.ExternalRuntime) bool {
	if harness.Spec.Kagent != nil {
		return false
	}
	switch runtime {
	case dbpkg.ExternalRuntimeCodex:
		return harness.Spec.Codex != nil && harness.Spec.Claude == nil
	case dbpkg.ExternalRuntimeClaude:
		return harness.Spec.Claude != nil && harness.Spec.Codex == nil
	default:
		return false
	}
}

func compileTools(resolved []v2translator.ResolvedMCPTool) ([]profileTool, error) {
	allowByServer := make(map[string]map[string]struct{}, len(resolved))
	for _, tool := range resolved {
		server := tool.Binding.Server.Name
		if tool.Binding.Server.Kind != "RemoteMCPServer" || server == "" || tool.Server == nil || tool.Server.Name != server {
			return nil, v2translator.NewValidationError("external Harness profile contains an invalid logical MCP server")
		}
		if len(tool.Binding.Tools) == 0 {
			return nil, v2translator.NewValidationError("external Harness profile requires a non-empty MCP tool allowlist")
		}
		if len(tool.Server.Spec.HeadersFrom) != 0 || (tool.Server.Spec.TLS != nil && !tool.Server.Spec.TLS.IsEmpty()) {
			return nil, v2translator.NewValidationError("external Harness profiles cannot use RemoteMCPServer headers or TLS configuration")
		}
		allow := allowByServer[server]
		if allow == nil {
			allow = make(map[string]struct{}, len(tool.Binding.Tools))
			allowByServer[server] = allow
		}
		for _, name := range tool.Binding.Tools {
			if name == "" {
				return nil, v2translator.NewValidationError("external Harness profile contains an empty MCP tool name")
			}
			allow[name] = struct{}{}
		}
	}

	servers := make([]string, 0, len(allowByServer))
	for server := range allowByServer {
		servers = append(servers, server)
	}
	slices.Sort(servers)
	tools := make([]profileTool, 0, len(servers))
	for _, server := range servers {
		allowSet := allowByServer[server]
		allow := make([]string, 0, len(allowSet))
		for name := range allowSet {
			allow = append(allow, name)
		}
		slices.Sort(allow)
		tools = append(tools, profileTool{Server: server, Allow: allow})
	}
	return tools, nil
}

func externalAgentCard(template *v1alpha3.AgentTemplate) *a2atype.AgentCard {
	return &a2atype.AgentCard{
		Name:               strings.ReplaceAll(template.Name, "-", "_"),
		Description:        template.Spec.Description,
		Version:            profileVersion,
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []a2atype.AgentSkill{},
		Capabilities:       a2atype.AgentCapabilities{Streaming: false},
		// The public gateway replaces this private placeholder with its own
		// authenticated gRPC interface before returning the card to callers.
		SupportedInterfaces: []*a2atype.AgentInterface{a2atype.NewAgentInterface(
			"http://127.0.0.1", a2atype.TransportProtocolJSONRPC,
		)},
	}
}

func externalProvenance(input *v2translator.HarnessInput) provenance {
	servers := make([]resourceRef, 0, len(input.Root.MCPTools))
	for _, tool := range input.Root.MCPTools {
		server := tool.Server
		if server == nil {
			continue
		}
		servers = append(servers, resourceRef{
			Kind: "RemoteMCPServer", Namespace: server.Namespace, Name: server.Name, UID: string(server.UID),
		})
	}
	slices.SortFunc(servers, func(left, right resourceRef) int {
		leftKey := left.Namespace + "\x00" + left.Name + "\x00" + left.UID
		rightKey := right.Namespace + "\x00" + right.Name + "\x00" + right.UID
		return strings.Compare(leftKey, rightKey)
	})
	servers = slices.Compact(servers)

	template := input.Root.Template
	harness := input.Harness
	return provenance{
		Version: profileVersion,
		Harness: resourceRef{Kind: "Harness", Namespace: harness.Namespace, Name: harness.Name, UID: string(harness.UID)},
		AgentTemplate: resourceRef{
			Kind: "AgentTemplate", Namespace: template.Namespace, Name: template.Name, UID: string(template.UID),
		},
		MCPServers: servers,
	}
}
