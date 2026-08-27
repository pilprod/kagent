package mcprelay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxCapabilityBytes   = 512
	minCapabilityBytes   = 32
	maxBindingIDBytes    = 80
	maxArgumentsBytes    = 1 << 20
	maxSchemaBytes       = 256 << 10
	maxDescriptionBytes  = 16 << 10
	maxCallResultBytes   = 4 << 20
	maxUpstreamPages     = 100
	maxToolsPerPage      = 1000
	maxPaginationCursor  = 1024
	maxJSONDepth         = 64
	stableBindingIDBytes = len("mcp-") + sha256.Size*2
)

// ListTools returns only the tools selected by the authorized revision
// binding. It consumes all upstream pages and fails closed when a selected
// tool is missing, duplicated, or malformed.
func (e *Engine) ListTools(ctx context.Context, capability, bindingID string) ([]*mcp.Tool, error) {
	authorization, err := e.authorize(ctx, capability, bindingID)
	if err != nil {
		return nil, err
	}
	binding := authorization.binding

	allowed := make(map[string]struct{}, len(binding.Tools))
	for _, name := range binding.Tools {
		allowed[name] = struct{}{}
	}
	found := make(map[string]*mcp.Tool, len(binding.Tools))
	seenCursors := map[string]struct{}{"": {}}
	cursor := ""
	for pageNumber := 0; ; pageNumber++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if pageNumber >= maxUpstreamPages {
			return nil, failed(ErrUpstream, "upstream tools pagination exceeded its page limit")
		}
		page, err := e.upstream.ListTools(ctx, authorization.target, cursor)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, failed(ErrUpstream, "list selected tools")
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if len(page.Tools) > maxToolsPerPage {
			return nil, failed(ErrUpstream, "upstream tools page exceeds its item limit")
		}
		for _, candidate := range page.Tools {
			if candidate == nil {
				continue
			}
			if _, selected := allowed[candidate.Name]; !selected {
				continue
			}
			if _, duplicate := found[candidate.Name]; duplicate {
				return nil, failed(ErrUpstream, "upstream returned a selected tool more than once")
			}
			tool, err := sanitizeTool(candidate)
			if err != nil {
				return nil, failed(ErrUpstream, "upstream returned an invalid selected tool")
			}
			found[candidate.Name] = tool
		}

		if page.NextCursor == "" {
			break
		}
		if err := validateCursor(page.NextCursor); err != nil {
			return nil, failed(ErrUpstream, "upstream returned an invalid pagination cursor")
		}
		if _, duplicate := seenCursors[page.NextCursor]; duplicate {
			return nil, failed(ErrUpstream, "upstream tools pagination repeated a cursor")
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}

	tools := make([]*mcp.Tool, 0, len(binding.Tools))
	for _, name := range binding.Tools {
		tool := found[name]
		if tool == nil {
			return nil, failed(ErrUpstream, "upstream omitted a selected tool")
		}
		tools = append(tools, tool)
	}
	return tools, nil
}

// CallTool invokes exactly one tool selected by the authorized binding. Tool
// authorization completes before the upstream client is touched.
func (e *Engine) CallTool(
	ctx context.Context,
	capability, bindingID, toolName string,
	arguments json.RawMessage,
) (*mcp.CallToolResult, error) {
	if err := validateToolName(toolName); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	arguments, err := validateArguments(arguments)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	authorization, err := e.authorize(ctx, capability, bindingID)
	if err != nil {
		return nil, err
	}
	binding := authorization.binding
	if _, allowed := slices.BinarySearch(binding.Tools, toolName); !allowed {
		return nil, ErrPermissionDenied
	}

	result, err := e.upstream.CallTool(ctx, authorization.target, toolName, arguments)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, failed(ErrUpstream, "call selected tool")
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	result, err = sanitizeCallToolResult(result)
	if err != nil {
		return nil, failed(ErrUpstream, "upstream returned an invalid tool result")
	}
	return result, nil
}

type authorization struct {
	binding translator.MCPPolicyBinding
	target  UpstreamTarget
}

func (e *Engine) authorize(ctx context.Context, capability, bindingID string) (authorization, error) {
	if err := validateOpaqueValue("capability", capability, minCapabilityBytes, maxCapabilityBytes); err != nil {
		return authorization{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := validateBindingID(bindingID); err != nil {
		return authorization{}, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	if err := ctx.Err(); err != nil {
		return authorization{}, err
	}

	digest := CapabilityDigest(sha256.Sum256([]byte(capability)))
	grant, err := e.grants.VerifyMCPGrant(ctx, digest)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return authorization{}, ctxErr
	}
	if err != nil {
		return authorization{}, failed(ErrUnauthenticated, "verify capability")
	}
	if grant.AgentInstanceID == "" || grant.Revision == "" || grant.BindingID == "" || grant.ExpiresAt.IsZero() {
		return authorization{}, failed(ErrUnavailable, "verified grant is incomplete")
	}
	if grant.RevokedAt != nil || !e.now().Before(grant.ExpiresAt) {
		return authorization{}, ErrUnauthenticated
	}
	if grant.BindingID != bindingID {
		return authorization{}, ErrPermissionDenied
	}

	lifecycle, err := e.lifecycles.MCPInstanceLifecycle(ctx, grant.AgentInstanceID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return authorization{}, ctxErr
	}
	if err != nil {
		return authorization{}, failed(ErrUnavailable, "load AgentInstance lifecycle")
	}
	if lifecycle.AgentInstanceID != grant.AgentInstanceID || lifecycle.PreparedRevision == "" {
		return authorization{}, failed(ErrUnavailable, "AgentInstance lifecycle is inconsistent")
	}
	if lifecycle.State != InstanceStateReady || lifecycle.OperationPending {
		return authorization{}, ErrPermissionDenied
	}
	if lifecycle.PreparedRevision != grant.Revision {
		return authorization{}, ErrPermissionDenied
	}

	policy, err := e.policies.MCPPolicy(ctx, grant.Revision)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return authorization{}, ctxErr
	}
	if err != nil {
		return authorization{}, failed(ErrUnavailable, "load prepared revision MCP policy")
	}
	if err := policy.Validate(); err != nil {
		return authorization{}, failed(ErrUnavailable, "prepared revision MCP policy is invalid")
	}
	binding, found := policy.Binding(bindingID)
	if !found {
		return authorization{}, ErrPermissionDenied
	}
	return authorization{
		binding: binding,
		target: UpstreamTarget{
			AgentInstanceID: grant.AgentInstanceID,
			Revision:        lifecycle.PreparedRevision,
			BindingID:       binding.ID,
			Server:          binding.Server,
		},
	}, nil
}
