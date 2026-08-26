package externalruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"reflect"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
	"github.com/kagent-dev/kagent/go/core/v2/externalprofile"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
)

const maxAgentCardBytes int64 = 1 << 20

type revisionStore interface {
	GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error)
}

// Lifecycle validates and probes an explicitly placed external runtime without
// owning or terminating the shared client process.
type Lifecycle struct {
	revisions    revisionStore
	placement    placement
	broker       *externalgateway.Broker
	probeTimeout time.Duration
}

var _ runtimebackend.Lifecycle = (*Lifecycle)(nil)

// NewLifecycle constructs an online-only external runtime lifecycle.
func NewLifecycle(revisions revisionStore, placement placement, broker *externalgateway.Broker, probeTimeout time.Duration) (*Lifecycle, error) {
	if nilInterface(revisions) {
		return nil, fmt.Errorf("external runtime revision store is nil")
	}
	if nilInterface(placement) {
		return nil, fmt.Errorf("external runtime placement is nil")
	}
	if broker == nil {
		return nil, fmt.Errorf("external runtime broker is nil")
	}
	if probeTimeout <= 0 {
		return nil, fmt.Errorf("external runtime probe timeout must be positive")
	}
	return &Lifecycle{revisions: revisions, placement: placement, broker: broker, probeTimeout: probeTimeout}, nil
}

// Create validates the immutable revision and placement, probes the exact
// online slot once, and publishes its private authority.
func (l *Lifecycle) Create(ctx context.Context, instance *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	resolved, err := l.resolve(ctx, instance)
	if err != nil {
		return runtimebackend.Endpoint{}, err
	}
	if existing := instance.GetA2AAuthority(); existing != "" && existing != resolved.authority {
		return runtimebackend.Endpoint{}, newLifecycleError("create", instance, fmt.Errorf("persisted authority does not match runtime placement"))
	}
	if err := l.probe(ctx, instance, resolved.slot, resolved.runtime, resolved.profile); err != nil {
		return runtimebackend.Endpoint{}, err
	}
	return runtimebackend.Endpoint{A2AAuthority: resolved.authority}, nil
}

// Fork is explicitly unsupported until external clients implement durable
// checkpoint import.
func (*Lifecycle) Fork(context.Context, *apiv1alpha1.AgentInstance, *dbpkg.AgentInstanceCheckpoint) (runtimebackend.Endpoint, error) {
	return runtimebackend.Endpoint{}, fmt.Errorf("fork external runtime: %w", runtimebackend.ErrCheckpointUnsupported)
}

// Quiesce is explicitly unsupported until external clients can publish a
// durable task-boundary snapshot.
func (*Lifecycle) Quiesce(context.Context, *apiv1alpha1.AgentInstance) (*dbpkg.AgentInstanceTaskSnapshot, error) {
	return nil, fmt.Errorf("quiesce external runtime: %w", runtimebackend.ErrCheckpointUnsupported)
}

// Suspend is a logical idempotent operation. It validates persisted routing
// when present but never suspends the shared external client runtime.
func (l *Lifecycle) Suspend(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	return l.validateRoutingWhenPresent(ctx, instance, "suspend")
}

// Resume validates persisted routing and requires the exact placed runtime to
// be online and serving a compatible Agent Card.
func (l *Lifecycle) Resume(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	resolved, err := l.resolve(ctx, instance)
	if err != nil {
		return err
	}
	if err := validateAuthority(instance, resolved); err != nil {
		return newLifecycleError("resume", instance, err)
	}
	return l.probe(ctx, instance, resolved.slot, resolved.runtime, resolved.profile)
}

// Delete is a logical idempotent operation. It validates persisted routing
// when present but never terminates the shared external client runtime.
func (l *Lifecycle) Delete(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	return l.validateRoutingWhenPresent(ctx, instance, "delete")
}

type resolvedRuntime struct {
	runtime   dbpkg.ExternalRuntime
	slot      externalgateway.SlotKey
	authority string
	profile   externalprofile.Profile
}

func (l *Lifecycle) resolve(ctx context.Context, instance *apiv1alpha1.AgentInstance) (resolvedRuntime, error) {
	if ctx == nil {
		return resolvedRuntime{}, fmt.Errorf("external runtime lifecycle requires a context")
	}
	if instance == nil {
		return resolvedRuntime{}, fmt.Errorf("external runtime lifecycle requires an AgentInstance")
	}
	if instance.GetPreparedRevision() == "" {
		return resolvedRuntime{}, newLifecycleError("load prepared revision", instance, fmt.Errorf("prepared revision is empty"))
	}
	revision, err := l.revisions.GetRuntimeRevision(ctx, instance.GetPreparedRevision())
	if err != nil {
		return resolvedRuntime{}, newLifecycleError("load prepared revision", instance, err)
	}
	if revision == nil {
		return resolvedRuntime{}, newLifecycleError("load prepared revision", instance, fmt.Errorf("runtime revision store returned nil"))
	}
	if revision.Revision != instance.GetPreparedRevision() {
		return resolvedRuntime{}, newLifecycleError("load prepared revision", instance, fmt.Errorf("runtime revision identity does not match AgentInstance"))
	}
	if err := revision.ValidateBackendIdentity(); err != nil {
		return resolvedRuntime{}, newLifecycleError("validate prepared revision", instance, err)
	}
	if revision.BackendKind != dbpkg.RuntimeBackendKindExternal {
		return resolvedRuntime{}, newLifecycleError("validate prepared revision", instance, fmt.Errorf("runtime revision does not select the external backend"))
	}
	profile, err := externalprofile.Decode(revision.ExternalProfile)
	if err != nil {
		return resolvedRuntime{}, newLifecycleError("validate prepared revision", instance, err)
	}
	if _, err := externalprofile.NewEnvelope(revision.Revision, profile); err != nil {
		return resolvedRuntime{}, newLifecycleError("validate prepared revision", instance, err)
	}
	expectedRuntime, err := gatewayRuntime(revision.ExternalRuntime)
	if err != nil {
		return resolvedRuntime{}, newLifecycleError("validate prepared revision", instance, err)
	}
	slot, err := l.placement.Select(revision.ExternalRuntime)
	if err != nil {
		return resolvedRuntime{}, newLifecycleError("select placement", instance, err)
	}
	if slot.Runtime != expectedRuntime {
		return resolvedRuntime{}, newLifecycleError("select placement", instance, fmt.Errorf("placement runtime does not match prepared revision"))
	}
	authority, err := EncodeAuthority(slot)
	if err != nil {
		return resolvedRuntime{}, newLifecycleError("select placement", instance, err)
	}
	return resolvedRuntime{runtime: revision.ExternalRuntime, slot: slot, authority: authority, profile: profile}, nil
}

func (l *Lifecycle) validateRoutingWhenPresent(ctx context.Context, instance *apiv1alpha1.AgentInstance, operation string) error {
	if instance == nil {
		return fmt.Errorf("external runtime lifecycle requires an AgentInstance")
	}
	if instance.GetA2AAuthority() == "" {
		return nil
	}
	resolved, err := l.resolve(ctx, instance)
	if err != nil {
		return err
	}
	if err := validateAuthority(instance, resolved); err != nil {
		return newLifecycleError(operation, instance, err)
	}
	return nil
}

func validateAuthority(instance *apiv1alpha1.AgentInstance, resolved resolvedRuntime) error {
	if instance.GetA2AAuthority() == "" {
		return fmt.Errorf("persisted external runtime authority is empty")
	}
	slot, err := DecodeAuthority(instance.GetA2AAuthority())
	if err != nil {
		return err
	}
	if slot != resolved.slot || string(slot.Runtime) != string(resolved.runtime) {
		return fmt.Errorf("persisted external runtime authority does not match prepared revision and placement")
	}
	return nil
}

func (l *Lifecycle) probe(ctx context.Context, instance *apiv1alpha1.AgentInstance, slot externalgateway.SlotKey, runtime dbpkg.ExternalRuntime, profile externalprofile.Profile) error {
	path, err := agentCardPath(runtime)
	if err != nil {
		return newLifecycleError("probe Agent Card", instance, err)
	}
	probeCtx, cancel := context.WithTimeout(&transportContext{Context: ctx}, l.probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, path, nil)
	if err != nil {
		return newLifecycleError("probe Agent Card", instance, err)
	}
	request.Header.Set("Accept", "application/json")
	response, err := l.broker.RoundTrip(probeCtx, slot, request)
	if err != nil {
		return newLifecycleError("probe Agent Card", instance, err)
	}
	if err := validateAgentCardResponse(response, profile); err != nil {
		return newLifecycleError("probe Agent Card", instance, err)
	}
	return nil
}

func agentCardPath(runtime dbpkg.ExternalRuntime) (string, error) {
	switch runtime {
	case dbpkg.ExternalRuntimeCodex:
		return "/codex/v1/.well-known/agent-card.json", nil
	case dbpkg.ExternalRuntimeClaude:
		return "/claude/v1/.well-known/agent-card.json", nil
	default:
		return "", fmt.Errorf("runtime revision selects an unsupported external runtime")
	}
}

func validateAgentCardResponse(response *http.Response, profile externalprofile.Profile) error {
	if response == nil || response.Body == nil {
		return fmt.Errorf("external runtime returned an empty Agent Card response")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxAgentCardBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil {
		return errors.Join(readErr, closeErr)
	}
	if len(body) > int(maxAgentCardBytes) {
		return fmt.Errorf("external runtime Agent Card exceeds the size limit")
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("external runtime Agent Card returned HTTP status %d", response.StatusCode)
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return fmt.Errorf("external runtime Agent Card must use application/json")
	}
	var card a2atype.AgentCard
	if err := json.Unmarshal(body, &card); err != nil {
		return err
	}
	return validateAgentCard(&card, profile)
}

func validateAgentCard(card *a2atype.AgentCard, profile externalprofile.Profile) error {
	if card.Name == "" || card.Version == "" || len(card.DefaultInputModes) == 0 || len(card.DefaultOutputModes) == 0 {
		return fmt.Errorf("external runtime Agent Card is missing required A2A fields")
	}
	if card.Capabilities.Streaming {
		return fmt.Errorf("external runtime Agent Card must disable streaming")
	}
	compatibleInterface := false
	for _, endpoint := range card.SupportedInterfaces {
		if endpoint != nil && endpoint.URL != "" && endpoint.ProtocolBinding == a2atype.TransportProtocolJSONRPC && endpoint.ProtocolVersion == a2atype.Version {
			compatibleInterface = true
			break
		}
	}
	if !compatibleInterface {
		return fmt.Errorf("external runtime Agent Card has no A2A 1.0 JSON-RPC interface")
	}
	return validateProfileCapability(card.Capabilities.Extensions, profile)
}

func validateProfileCapability(extensions []a2atype.AgentExtension, profile externalprofile.Profile) error {
	var capability *a2atype.AgentExtension
	for i := range extensions {
		if extensions[i].URI != externalprofile.ExtensionURI {
			continue
		}
		if capability != nil {
			return fmt.Errorf("external runtime Agent Card declares the profile capability more than once")
		}
		capability = &extensions[i]
	}
	if capability == nil {
		return fmt.Errorf("external runtime Agent Card does not declare the profile capability")
	}
	params := capability.Params
	version, versionOK := params["version"].(string)
	instruction, instructionOK := params["instruction"].(bool)
	tools, toolsOK := params["tools"].(bool)
	if len(params) != 3 || !versionOK || version != externalprofile.Version || !instructionOK || !instruction || !toolsOK || tools {
		return fmt.Errorf("external runtime Agent Card declares an incompatible profile capability")
	}
	if profile.RequiresTools() {
		return fmt.Errorf("external runtime does not support the prepared profile tool allowlist")
	}
	return nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type lifecycleError struct {
	operation  string
	instanceID string
	cause      error
}

func newLifecycleError(operation string, instance *apiv1alpha1.AgentInstance, cause error) error {
	return &lifecycleError{operation: operation, instanceID: instance.GetId(), cause: cause}
}

func (e *lifecycleError) Error() string {
	return fmt.Sprintf("failed to %s for external AgentInstance %q", e.operation, e.instanceID)
}

func (e *lifecycleError) Unwrap() error {
	return e.cause
}
