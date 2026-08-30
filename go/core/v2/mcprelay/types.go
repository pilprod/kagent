// Package mcprelay contains the transport-independent authorization and MCP
// filtering core for revision-scoped runtime workers.
package mcprelay

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	// ErrInvalidRequest reports malformed caller input before dependency access.
	ErrInvalidRequest = errors.New("invalid mcp relay request")
	// ErrUnauthenticated reports a missing or invalid relay capability.
	ErrUnauthenticated = errors.New("invalid mcp relay capability")
	// ErrPermissionDenied reports a valid capability used outside its exact scope.
	ErrPermissionDenied = errors.New("mcp relay operation is not permitted")
	// ErrUnavailable reports unavailable or inconsistent control-plane state.
	ErrUnavailable = errors.New("mcp relay authorization state is unavailable")
	// ErrUpstream reports a failed or malformed upstream MCP operation.
	ErrUpstream = errors.New("mcp relay upstream failed")
)

// CapabilityDigest is the only representation of a worker capability passed
// to a verifier. The plaintext capability remains outside persistence APIs.
type CapabilityDigest [sha256.Size]byte

// Grant is the verified scope of one short-lived capability. BindingID
// content-addresses only the non-secret subject/server UID/tool selection; the
// revision's private policy separately pins the server specification hash.
type Grant struct {
	AgentInstanceID string
	Revision        string
	BindingID       string
	ExpiresAt       time.Time
	RevokedAt       *time.Time
}

// InstanceState is the lifecycle state relevant to relay authorization.
type InstanceState string

const (
	InstanceStateCreating  InstanceState = "CREATING"
	InstanceStateReady     InstanceState = "READY"
	InstanceStateSuspended InstanceState = "SUSPENDED"
	InstanceStateFailed    InstanceState = "FAILED"
)

// InstanceLifecycle is authoritative current state, loaded for every relay
// operation so suspend/delete/revision changes revoke access immediately.
type InstanceLifecycle struct {
	AgentInstanceID  string
	PreparedRevision string
	State            InstanceState
	OperationPending bool
}

// PolicyStore loads immutable private policy by prepared revision.
type PolicyStore interface {
	MCPPolicy(context.Context, string) (translator.MCPPolicyV1, error)
}

// GrantVerifier resolves only a capability digest, never its plaintext value.
type GrantVerifier interface {
	VerifyMCPGrant(context.Context, CapabilityDigest) (Grant, error)
}

// LifecycleStore loads current AgentInstance lifecycle state.
type LifecycleStore interface {
	MCPInstanceLifecycle(context.Context, string) (InstanceLifecycle, error)
}

// ToolPage is one page returned by an upstream MCP server.
type ToolPage struct {
	Tools      []*mcp.Tool
	NextCursor string
}

// UpstreamTarget is the complete immutable authorization scope supplied to an
// in-cluster upstream resolver. The Engine derives every field from verified
// grant, lifecycle, and prepared-revision policy state; relay callers cannot
// provide or override any part of this target.
type UpstreamTarget struct {
	AgentInstanceID string
	Revision        string
	BindingID       string
	Server          translator.MCPServerIdentity
}

// UpstreamClient resolves the pinned server identity behind the cluster trust
// boundary. Callers can never supply a URL or arbitrary server reference.
// ListTools invokes its callback synchronously for every page while one scoped
// upstream session remains open; the Engine owns page and cursor validation.
type UpstreamClient interface {
	ListTools(context.Context, UpstreamTarget, func(ToolPage) error) error
	CallTool(context.Context, UpstreamTarget, string, json.RawMessage) (*mcp.CallToolResult, error)
}

// Config contains all dependencies of the transport-independent relay core.
type Config struct {
	Policies   PolicyStore
	Grants     GrantVerifier
	Lifecycles LifecycleStore
	Upstream   UpstreamClient
	Now        func() time.Time
}

// Engine authorizes and filters MCP operations. It deliberately exposes no
// HTTP, gRPC, Kubernetes, database, or Secret-resolution behavior.
type Engine struct {
	policies   PolicyStore
	grants     GrantVerifier
	lifecycles LifecycleStore
	upstream   UpstreamClient
	now        func() time.Time
}

// New constructs a relay core with explicit authorization dependencies.
func New(config Config) (*Engine, error) {
	if config.Policies == nil || config.Grants == nil || config.Lifecycles == nil || config.Upstream == nil {
		return nil, errors.New("mcp relay policy, grant, lifecycle, and upstream dependencies are required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Engine{
		policies: config.Policies, grants: config.Grants, lifecycles: config.Lifecycles,
		upstream: config.Upstream, now: config.Now,
	}, nil
}

func failed(kind error, operation string) error {
	return fmt.Errorf("%w: %s", kind, operation)
}
