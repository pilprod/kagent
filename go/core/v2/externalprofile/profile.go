// Package externalprofile defines the private, immutable configuration sent
// from the authenticated kagent A2A gateway to a connected external runtime.
package externalprofile

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

const (
	// ExtensionURI is reserved for the top-level A2A request metadata carrying
	// the prepared external runtime profile. It is never valid in Message.Metadata.
	ExtensionURI = "https://yourown.chat/a2a/extensions/external-agent-profile/v1"
	// Version is the only profile and dispatch envelope version understood by
	// this release.
	Version = "v1"
)

// tool is one logical MCP server and its immutable allowlist. Endpoints and
// credentials intentionally do not cross the external runtime boundary.
type tool struct {
	Server string
	Allow  []string
}

// Profile is the sanitized compiler output persisted with a RuntimeRevision.
type Profile struct {
	instruction string
	tools       []tool
}

// Envelope is the value stored under ExtensionURI in top-level A2A metadata.
type Envelope struct {
	revision    string
	instruction string
	tools       []tool
}

type wireProfile struct {
	Version     *string     `json:"version"`
	Instruction *string     `json:"instruction"`
	Tools       *[]wireTool `json:"tools"`
}

type wireTool struct {
	Server *string   `json:"server"`
	Allow  *[]string `json:"allow"`
}

// Decode validates the exact persisted v1 schema. The compiler produces
// sorted, duplicate-free server and allow lists, so accepting anything else
// would make a corrupted database value acquire new runtime semantics.
func Decode(raw json.RawMessage) (Profile, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var wire wireProfile
	if err := decoder.Decode(&wire); err != nil {
		return Profile{}, fmt.Errorf("decode external runtime profile: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Profile{}, err
	}
	if wire.Version == nil || *wire.Version != Version || wire.Instruction == nil || wire.Tools == nil {
		return Profile{}, fmt.Errorf("external runtime profile does not match version v1")
	}

	profile := Profile{instruction: *wire.Instruction, tools: make([]tool, len(*wire.Tools))}
	previousServer := ""
	for i, candidate := range *wire.Tools {
		if candidate.Server == nil || *candidate.Server == "" || candidate.Allow == nil || len(*candidate.Allow) == 0 {
			return Profile{}, fmt.Errorf("external runtime profile contains an invalid tool entry")
		}
		if i > 0 && *candidate.Server <= previousServer {
			return Profile{}, fmt.Errorf("external runtime profile tool servers must be sorted and unique")
		}
		if !strictlySortedNonEmpty(*candidate.Allow) {
			return Profile{}, fmt.Errorf("external runtime profile tool allowlists must be sorted, unique, and non-empty")
		}
		profile.tools[i] = tool{Server: *candidate.Server, Allow: slices.Clone(*candidate.Allow)}
		previousServer = *candidate.Server
	}
	return profile, nil
}

// RequiresTools reports whether the prepared profile needs logical MCP tool
// mapping support from the connected local runtime.
func (p Profile) RequiresTools() bool { return len(p.tools) != 0 }

func requireEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("external runtime profile contains trailing JSON data")
		}
		return fmt.Errorf("decode external runtime profile: %w", err)
	}
	return nil
}

func strictlySortedNonEmpty(values []string) bool {
	for i, value := range values {
		if value == "" || (i > 0 && value <= values[i-1]) {
			return false
		}
	}
	return true
}

// NewEnvelope binds a decoded profile to the full prepared SHA-256 identity.
func NewEnvelope(revision string, profile Profile) (Envelope, error) {
	if !validRevision(revision) {
		return Envelope{}, fmt.Errorf("external runtime revision is not a lowercase SHA-256 identity")
	}
	return Envelope{revision: revision, instruction: profile.instruction, tools: cloneTools(profile.tools)}, nil
}

func validRevision(revision string) bool {
	if len(revision) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(revision)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == revision
}

// Metadata returns only protobuf-Struct-compatible JSON values and allocates a
// fresh object so caller-provided metadata can never alias the trusted value.
func (e Envelope) Metadata() map[string]any {
	tools := make([]any, len(e.tools))
	for i, profileTool := range e.tools {
		allow := make([]any, len(profileTool.Allow))
		for j, name := range profileTool.Allow {
			allow[j] = name
		}
		tools[i] = map[string]any{"server": profileTool.Server, "allow": allow}
	}
	return map[string]any{
		"version":     Version,
		"revision":    e.revision,
		"instruction": e.instruction,
		"tools":       tools,
	}
}

func cloneTools(tools []tool) []tool {
	cloned := make([]tool, len(tools))
	for i, profileTool := range tools {
		cloned[i] = tool{Server: profileTool.Server, Allow: slices.Clone(profileTool.Allow)}
	}
	return cloned
}
