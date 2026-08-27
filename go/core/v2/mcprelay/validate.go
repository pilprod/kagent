package mcprelay

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func validateBindingID(value string) error {
	if len(value) != stableBindingIDBytes || len(value) > maxBindingIDBytes || value[:len("mcp-")] != "mcp-" {
		return fmt.Errorf("binding ID has invalid format")
	}
	digest := value[len("mcp-"):]
	if _, err := hex.DecodeString(digest); err != nil || strings.ToLower(digest) != digest {
		return fmt.Errorf("binding ID has invalid format")
	}
	return nil
}

func validateOpaqueValue(field, value string, minimum, maximum int) error {
	if len(value) < minimum || len(value) > maximum {
		return fmt.Errorf("%s must contain between %d and %d bytes", field, minimum, maximum)
	}
	for _, character := range value {
		if character < '!' || character > '~' || unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("%s contains invalid characters", field)
		}
	}
	return nil
}

// validateCursor treats pagination cursors as opaque UTF-8 values. Unlike
// capabilities, cursors may legitimately contain Unicode and printable
// whitespace. Control characters are rejected so the value stays safe at
// logging and transport boundaries.
func validateCursor(value string) error {
	if value == "" || len(value) > maxPaginationCursor {
		return fmt.Errorf("pagination cursor must contain between 1 and %d bytes", maxPaginationCursor)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("pagination cursor must be valid UTF-8")
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return fmt.Errorf("pagination cursor contains a control character")
		}
	}
	return nil
}

func validateToolName(value string) error {
	if value == "" {
		return fmt.Errorf("tool name is required")
	}
	if len(value) > 128 {
		return fmt.Errorf("tool name exceeds 128 bytes")
	}
	for _, character := range value {
		valid := (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '-' || character == '_' || character == '.'
		if !valid {
			return fmt.Errorf("tool name contains invalid characters")
		}
	}
	return nil
}

func validateArguments(arguments json.RawMessage) (json.RawMessage, error) {
	if len(arguments) == 0 {
		return json.RawMessage("{}"), nil
	}
	if len(arguments) > maxArgumentsBytes {
		return nil, fmt.Errorf("tool arguments exceed %d bytes", maxArgumentsBytes)
	}
	if err := validateJSON(arguments, maxJSONDepth); err != nil {
		return nil, fmt.Errorf("tool arguments are invalid: %w", err)
	}
	trimmed := bytes.TrimSpace(arguments)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("tool arguments must be a JSON object")
	}
	return bytes.Clone(trimmed), nil
}

func sanitizeTool(candidate *mcp.Tool) (*mcp.Tool, error) {
	if err := validateToolName(candidate.Name); err != nil {
		return nil, err
	}
	if len(candidate.Description) > maxDescriptionBytes {
		return nil, fmt.Errorf("tool description exceeds %d bytes", maxDescriptionBytes)
	}
	for _, character := range candidate.Description {
		if character == 0 || (unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t') {
			return nil, fmt.Errorf("tool description contains control characters")
		}
	}
	input, err := sanitizeSchema(candidate.InputSchema, true)
	if err != nil {
		return nil, fmt.Errorf("invalid input schema: %w", err)
	}
	output, err := sanitizeSchema(candidate.OutputSchema, false)
	if err != nil {
		return nil, fmt.Errorf("invalid output schema: %w", err)
	}
	return &mcp.Tool{Name: candidate.Name, Description: candidate.Description, InputSchema: input, OutputSchema: output}, nil
}

// sanitizeCallToolResult validates the upstream wire representation and
// reconstructs it into a detached result. This strips server-only hidden state
// (including CallToolResult.err), deep-copies maps and slices, and limits tool
// results to MCP content types valid in a tools/call response.
func sanitizeCallToolResult(result *mcp.CallToolResult) (*mcp.CallToolResult, error) {
	if result == nil {
		return nil, fmt.Errorf("tool result is required")
	}
	for _, content := range result.Content {
		if !allowedCallToolContent(content) {
			return nil, fmt.Errorf("tool result contains a content type not allowed in tools/call")
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal tool result: %w", err)
	}
	if len(raw) > maxCallResultBytes {
		return nil, fmt.Errorf("tool result exceeds %d bytes", maxCallResultBytes)
	}
	if err := validateJSON(raw, maxJSONDepth); err != nil {
		return nil, fmt.Errorf("invalid tool result JSON: %w", err)
	}

	var detached mcp.CallToolResult
	if err := json.Unmarshal(raw, &detached); err != nil {
		return nil, fmt.Errorf("decode tool result: %w", err)
	}
	for _, content := range detached.Content {
		if !allowedCallToolContent(content) {
			return nil, fmt.Errorf("tool result contains a content type not allowed in tools/call")
		}
	}
	return &detached, nil
}

func allowedCallToolContent(content mcp.Content) bool {
	switch value := content.(type) {
	case *mcp.TextContent:
		return value != nil
	case *mcp.ImageContent:
		return value != nil
	case *mcp.AudioContent:
		return value != nil
	case *mcp.ResourceLink:
		return value != nil
	case *mcp.EmbeddedResource:
		return value != nil
	default:
		return false
	}
}

func sanitizeSchema(schema any, input bool) (json.RawMessage, error) {
	if schema == nil {
		if input {
			return nil, fmt.Errorf("schema is required")
		}
		return nil, nil
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	if len(raw) > maxSchemaBytes {
		return nil, fmt.Errorf("schema exceeds %d bytes", maxSchemaBytes)
	}
	if err := validateJSON(raw, maxJSONDepth); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if input {
		var object map[string]json.RawMessage
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return nil, fmt.Errorf("input schema must be a JSON object")
		}
		if err := json.Unmarshal(raw, &object); err != nil || object == nil {
			return nil, fmt.Errorf("input schema must be a JSON object")
		}
		var schemaType string
		if err := json.Unmarshal(object["type"], &schemaType); err != nil || schemaType != "object" {
			return nil, fmt.Errorf("input schema root type must be object")
		}
	} else if len(trimmed) == 0 || (trimmed[0] != '{' && string(trimmed) != "true" && string(trimmed) != "false") {
		return nil, fmt.Errorf("output schema must be a JSON object or boolean")
	}
	return bytes.Clone(raw), nil
}

func validateJSON(raw []byte, maxDepth int) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, 0, maxDepth); err != nil {
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

func consumeJSONValue(decoder *json.Decoder, depth, maxDepth int) error {
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
			if err := consumeJSONValue(decoder, depth+1, maxDepth); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1, maxDepth); err != nil {
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
