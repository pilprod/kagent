package translator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode"

	"github.com/kagent-dev/kagent/go/api/v1alpha3"
)

const (
	// MCPPolicyVersionV1 identifies the first private relay policy format. It is
	// an internal runtime contract, not a public Kubernetes API.
	MCPPolicyVersionV1 = "v1"

	mcpBindingIDPrefix    = "mcp-"
	maxMCPToolNameBytes   = 128
	maxMCPPathPartBytes   = 128
	maxMCPObjectIDBytes   = 256
	maxMCPServerSpec      = 256 << 10
	maxMCPPolicyBindings  = 2550
	maxMCPToolsPerBinding = 50
	maxMCPSubjectDepth    = 2
	maxMCPPolicyJSONBytes = 32 << 20
	maxMCPPolicyJSONDepth = 16
)

// MCPPolicyV1 is the private, immutable MCP authorization policy compiled for
// one runtime revision. It deliberately contains only server identity and a
// hash of the connection specification: URLs, headers, TLS material, and
// credential values are resolved behind the relay boundary in later slices.
type MCPPolicyV1 struct {
	Version  string             `json:"version"`
	Bindings []MCPPolicyBinding `json:"bindings"`
}

// MCPPolicyBinding grants one runtime subject an exact tool set on one
// RemoteMCPServer. ID is content-addressed so callers cannot substitute a
// server while retaining a valid binding identifier.
type MCPPolicyBinding struct {
	ID          string            `json:"id"`
	SubjectPath []string          `json:"subjectPath"`
	Server      MCPServerIdentity `json:"server"`
	Tools       []string          `json:"tools"`
}

// MCPServerIdentity pins a RemoteMCPServer without disclosing its connection
// configuration to a runtime worker.
type MCPServerIdentity struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	SpecHash  string `json:"specHash"`
}

type mcpBindingIdentity struct {
	SubjectPath []string          `json:"subjectPath"`
	Server      MCPServerIdentity `json:"server"`
	Tools       []string          `json:"tools"`
}

// DecodeMCPPolicyV1 strictly decodes persisted private policy. It rejects
// duplicate and unknown fields, trailing values, excessive size/depth, and
// every non-canonical semantic form rejected by Validate.
func DecodeMCPPolicyV1(raw []byte) (MCPPolicyV1, error) {
	if len(raw) == 0 {
		return MCPPolicyV1{}, fmt.Errorf("MCP policy is empty")
	}
	if len(raw) > maxMCPPolicyJSONBytes {
		return MCPPolicyV1{}, fmt.Errorf("MCP policy exceeds %d bytes", maxMCPPolicyJSONBytes)
	}
	if err := validateMCPPolicyJSON(raw, maxMCPPolicyJSONDepth); err != nil {
		return MCPPolicyV1{}, fmt.Errorf("validate MCP policy JSON: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var policy MCPPolicyV1
	if err := decoder.Decode(&policy); err != nil {
		return MCPPolicyV1{}, fmt.Errorf("decode MCP policy: %w", err)
	}
	if err := policy.Validate(); err != nil {
		return MCPPolicyV1{}, err
	}
	return policy, nil
}

// CanonicalMCPPolicyJSON returns the unique JSON representation used at the
// database boundary after strict decoding and semantic validation.
func CanonicalMCPPolicyJSON(raw []byte) ([]byte, error) {
	policy, err := DecodeMCPPolicyV1(raw)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("marshal canonical MCP policy: %w", err)
	}
	return canonical, nil
}

func validateMCPPolicyJSON(raw []byte, maxDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeMCPPolicyJSONValue(decoder, 0, maxDepth); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains trailing data")
		}
		return fmt.Errorf("read trailing JSON data: %w", err)
	}
	return nil
}

func consumeMCPPolicyJSONValue(decoder *json.Decoder, depth, maxDepth int) error {
	if depth > maxDepth {
		return fmt.Errorf("JSON nesting exceeds %d levels", maxDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("decode JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("JSON object contains a duplicate key")
			}
			seen[key] = struct{}{}
			if err := consumeMCPPolicyJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeMCPPolicyJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	closing, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("decode JSON closing delimiter: %w", err)
	}
	want := json.Delim('}')
	if delimiter == '[' {
		want = ']'
	}
	if closing != want {
		return fmt.Errorf("unexpected JSON closing delimiter %q", closing)
	}
	return nil
}

func buildMCPPolicy(input *HarnessInput) (MCPPolicyV1, error) {
	policy := MCPPolicyV1{Version: MCPPolicyVersionV1, Bindings: []MCPPolicyBinding{}}
	if input == nil || input.Root == nil {
		return MCPPolicyV1{}, NewValidationError("resolved Harness input is missing its root AgentTemplate")
	}

	var add func(*AgentInput, []string) error
	add = func(agent *AgentInput, subjectPath []string) error {
		if agent == nil || agent.Template == nil {
			return NewValidationError("resolved AgentTemplate input is incomplete")
		}
		for _, resolved := range agent.MCPTools {
			binding, err := newMCPPolicyBinding(subjectPath, agent.Template.Namespace, resolved)
			if err != nil {
				return err
			}
			policy.Bindings = append(policy.Bindings, binding)
		}
		for _, shared := range agent.Shared {
			if err := validateMCPIdentifier("Shared binding name", shared.Name, maxMCPPathPartBytes); err != nil {
				return err
			}
			path := append(slices.Clone(subjectPath), shared.Name)
			if err := add(shared.Agent, path); err != nil {
				return err
			}
		}
		return nil
	}
	if err := add(input.Root, []string{"root"}); err != nil {
		return MCPPolicyV1{}, err
	}

	slices.SortFunc(policy.Bindings, func(a, b MCPPolicyBinding) int { return strings.Compare(a.ID, b.ID) })
	canonical := policy.Bindings[:0]
	for _, binding := range policy.Bindings {
		if len(canonical) > 0 && canonical[len(canonical)-1].ID == binding.ID {
			previous := canonical[len(canonical)-1]
			if previous.Server != binding.Server || !slices.Equal(previous.SubjectPath, binding.SubjectPath) || !slices.Equal(previous.Tools, binding.Tools) {
				return MCPPolicyV1{}, NewValidationError("MCP binding identity collision for %q", binding.ID)
			}
			continue
		}
		canonical = append(canonical, binding)
	}
	policy.Bindings = canonical
	if err := policy.Validate(); err != nil {
		return MCPPolicyV1{}, err
	}
	return policy, nil
}

func newMCPPolicyBinding(subjectPath []string, agentNamespace string, resolved ResolvedMCPTool) (MCPPolicyBinding, error) {
	if resolved.Server == nil {
		return MCPPolicyBinding{}, NewValidationError("resolved RemoteMCPServer is missing")
	}
	server := resolved.Server
	if resolved.Binding.Server.Kind != "RemoteMCPServer" || resolved.Binding.Server.Name != server.Name || server.Namespace != agentNamespace {
		return MCPPolicyBinding{}, NewValidationError("resolved RemoteMCPServer identity does not match its AgentTemplate binding")
	}
	if err := validateMCPIdentifier("RemoteMCPServer namespace", server.Namespace, maxMCPObjectIDBytes); err != nil {
		return MCPPolicyBinding{}, err
	}
	if err := validateMCPIdentifier("RemoteMCPServer name", server.Name, maxMCPObjectIDBytes); err != nil {
		return MCPPolicyBinding{}, err
	}
	if err := validateMCPIdentifier("RemoteMCPServer UID", string(server.UID), maxMCPObjectIDBytes); err != nil {
		return MCPPolicyBinding{}, err
	}
	specHash, err := MCPServerSpecHash(server.Spec)
	if err != nil {
		return MCPPolicyBinding{}, NewValidationError("hash RemoteMCPServer %q specification: %v", server.Name, err)
	}

	tools := slices.Clone(resolved.Binding.Tools)
	if len(tools) > maxMCPToolsPerBinding {
		return MCPPolicyBinding{}, NewValidationError("RemoteMCPServer %q binding exceeds %d tools", server.Name, maxMCPToolsPerBinding)
	}
	for _, tool := range tools {
		if err := validateMCPToolName(tool); err != nil {
			return MCPPolicyBinding{}, err
		}
	}
	slices.Sort(tools)
	tools = slices.Compact(tools)
	if len(tools) == 0 {
		return MCPPolicyBinding{}, NewValidationError("RemoteMCPServer %q binding selects no tools", server.Name)
	}

	binding := MCPPolicyBinding{
		SubjectPath: slices.Clone(subjectPath),
		Server: MCPServerIdentity{
			Namespace: server.Namespace,
			Name:      server.Name,
			UID:       string(server.UID),
			SpecHash:  specHash,
		},
		Tools: tools,
	}
	binding.ID, err = mcpBindingID(binding)
	if err != nil {
		return MCPPolicyBinding{}, err
	}
	return binding, nil
}

// MCPServerSpecHash returns the private policy identity for one
// RemoteMCPServer specification. Relay resolution must use this function so
// compile-time and invocation-time pinning cannot drift apart.
func MCPServerSpecHash(spec v1alpha3.RemoteMCPServerSpec) (string, error) {
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal RemoteMCPServer specification: %w", err)
	}
	if len(raw) > maxMCPServerSpec {
		return "", fmt.Errorf("RemoteMCPServer specification exceeds %d bytes", maxMCPServerSpec)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}

func mcpBindingID(binding MCPPolicyBinding) (string, error) {
	raw, err := json.Marshal(mcpBindingIdentity{SubjectPath: binding.SubjectPath, Server: binding.Server, Tools: binding.Tools})
	if err != nil {
		return "", fmt.Errorf("marshal MCP binding identity: %w", err)
	}
	digest := sha256.Sum256(raw)
	return mcpBindingIDPrefix + hex.EncodeToString(digest[:]), nil
}

// Validate rejects non-canonical or tampered persisted policies before the
// relay uses them for authorization.
func (p MCPPolicyV1) Validate() error {
	if p.Version != MCPPolicyVersionV1 {
		return fmt.Errorf("unsupported MCP policy version %q", p.Version)
	}
	if p.Bindings == nil {
		return fmt.Errorf("MCP policy bindings must be an array")
	}
	if len(p.Bindings) > maxMCPPolicyBindings {
		return fmt.Errorf("MCP policy exceeds %d bindings", maxMCPPolicyBindings)
	}
	previous := ""
	for i, binding := range p.Bindings {
		if binding.ID <= previous {
			return fmt.Errorf("MCP policy bindings are not sorted and unique at index %d", i)
		}
		previous = binding.ID
		if len(binding.SubjectPath) == 0 || len(binding.SubjectPath) > maxMCPSubjectDepth || binding.SubjectPath[0] != "root" {
			return fmt.Errorf("MCP binding %q has invalid subject path", binding.ID)
		}
		for _, part := range binding.SubjectPath {
			if err := validateMCPIdentifier("MCP subject path component", part, maxMCPPathPartBytes); err != nil {
				return err
			}
		}
		if err := validateMCPIdentifier("RemoteMCPServer namespace", binding.Server.Namespace, maxMCPObjectIDBytes); err != nil {
			return err
		}
		if err := validateMCPIdentifier("RemoteMCPServer name", binding.Server.Name, maxMCPObjectIDBytes); err != nil {
			return err
		}
		if err := validateMCPIdentifier("RemoteMCPServer UID", binding.Server.UID, maxMCPObjectIDBytes); err != nil {
			return err
		}
		if len(binding.Server.SpecHash) != sha256.Size*2 {
			return fmt.Errorf("MCP binding %q has invalid server specification hash", binding.ID)
		}
		if _, err := hex.DecodeString(binding.Server.SpecHash); err != nil || strings.ToLower(binding.Server.SpecHash) != binding.Server.SpecHash {
			return fmt.Errorf("MCP binding %q has invalid server specification hash", binding.ID)
		}
		if len(binding.Tools) == 0 {
			return fmt.Errorf("MCP binding %q selects no tools", binding.ID)
		}
		if len(binding.Tools) > maxMCPToolsPerBinding {
			return fmt.Errorf("MCP binding %q exceeds %d tools", binding.ID, maxMCPToolsPerBinding)
		}
		for toolIndex, tool := range binding.Tools {
			if err := validateMCPToolNameValue(tool); err != nil {
				return err
			}
			if toolIndex > 0 && binding.Tools[toolIndex-1] >= tool {
				return fmt.Errorf("MCP binding %q tools are not sorted and unique", binding.ID)
			}
		}
		expected, err := mcpBindingID(binding)
		if err != nil {
			return err
		}
		if binding.ID != expected {
			return fmt.Errorf("MCP binding %q does not match its content identity", binding.ID)
		}
	}
	return nil
}

// Binding returns the immutable policy binding identified by id.
func (p MCPPolicyV1) Binding(id string) (MCPPolicyBinding, bool) {
	index, found := slices.BinarySearchFunc(p.Bindings, id, func(binding MCPPolicyBinding, id string) int {
		return strings.Compare(binding.ID, id)
	})
	if !found {
		return MCPPolicyBinding{}, false
	}
	binding := p.Bindings[index]
	binding.SubjectPath = slices.Clone(binding.SubjectPath)
	binding.Tools = slices.Clone(binding.Tools)
	return binding, true
}

func validateMCPToolName(value string) error {
	if err := validateMCPToolNameValue(value); err != nil {
		return NewValidationError("%v", err)
	}
	return nil
}

func validateMCPToolNameValue(value string) error {
	if value == "" {
		return fmt.Errorf("MCP tool name is required")
	}
	if len(value) > maxMCPToolNameBytes {
		return fmt.Errorf("MCP tool name exceeds %d bytes", maxMCPToolNameBytes)
	}
	for _, character := range value {
		valid := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.'
		if !valid {
			return fmt.Errorf("MCP tool name %q contains invalid characters", value)
		}
	}
	return nil
}

func validateMCPIdentifier(field, value string, limit int) error {
	if value == "" {
		return NewValidationError("%s is required", field)
	}
	if len(value) > limit {
		return NewValidationError("%s exceeds %d bytes", field, limit)
	}
	for _, character := range value {
		if character == 0 || unicode.IsControl(character) {
			return NewValidationError("%s contains control characters", field)
		}
	}
	return nil
}
