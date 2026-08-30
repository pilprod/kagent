package runtimebackend

import (
	"context"
	"fmt"
	"reflect"

	"github.com/a2aproject/a2a-go/v2/a2aclient"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// Kind identifies a private runtime implementation. It is intentionally not a
// public AgentInstance choice until the control plane can persist the selection.
type Kind string

const (
	// KindSubstrate routes an AgentInstance to the in-cluster Substrate runtime.
	KindSubstrate Kind = "substrate"
	// KindExternal routes an AgentInstance to a connected external runtime.
	KindExternal Kind = "external"
)

// Backend keeps lifecycle operations and A2A connectivity inseparable for one
// runtime implementation.
type Backend struct {
	Lifecycle Lifecycle
	Connector Connector
}

// Registration associates one supported kind with its complete backend.
type Registration struct {
	Kind    Kind
	Backend Backend
}

// Selector resolves the persisted runtime choice for an AgentInstance.
type Selector interface {
	Select(context.Context, *apiv1alpha1.AgentInstance) (Kind, error)
}

// Router dispatches lifecycle and A2A operations through the same immutable
// backend registry.
type Router struct {
	selector Selector
	backends map[Kind]Backend
}

var (
	_ Lifecycle = (*Router)(nil)
	_ Connector = (*Router)(nil)
)

// NewRouter validates and constructs a router with at least one explicitly
// registered backend.
func NewRouter(selector Selector, registrations ...Registration) (*Router, error) {
	if isNil(selector) {
		return nil, fmt.Errorf("runtime backend selector is nil")
	}
	if len(registrations) == 0 {
		return nil, fmt.Errorf("at least one runtime backend must be registered")
	}

	backends := make(map[Kind]Backend, len(registrations))
	for _, registration := range registrations {
		if !supported(registration.Kind) {
			return nil, fmt.Errorf("runtime backend kind %q is not supported", registration.Kind)
		}
		if _, exists := backends[registration.Kind]; exists {
			return nil, fmt.Errorf("runtime backend %q is registered more than once", registration.Kind)
		}
		if isNil(registration.Backend.Lifecycle) {
			return nil, fmt.Errorf("runtime backend %q lifecycle is nil", registration.Kind)
		}
		if isNil(registration.Backend.Connector) {
			return nil, fmt.Errorf("runtime backend %q connector is nil", registration.Kind)
		}
		backends[registration.Kind] = registration.Backend
	}

	return &Router{selector: selector, backends: backends}, nil
}

// Create dispatches runtime creation to the selected backend.
func (r *Router) Create(ctx context.Context, instance *apiv1alpha1.AgentInstance) (Endpoint, error) {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return Endpoint{}, err
	}
	endpoint, err := backend.Lifecycle.Create(ctx, instance)
	if err != nil {
		return Endpoint{}, newOperationError("create", kind, instance, err)
	}
	return endpoint, nil
}

// Fork dispatches checkpoint restoration without degrading it to a fresh
// runtime creation.
func (r *Router) Fork(ctx context.Context, instance *apiv1alpha1.AgentInstance, checkpoint *dbpkg.AgentInstanceCheckpoint) (Endpoint, error) {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return Endpoint{}, err
	}
	endpoint, err := backend.Lifecycle.Fork(ctx, instance, checkpoint)
	if err != nil {
		return Endpoint{}, newOperationError("fork", kind, instance, err)
	}
	return endpoint, nil
}

// Quiesce returns the selected backend's exact durable task boundary.
func (r *Router) Quiesce(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*dbpkg.AgentInstanceTaskSnapshot, error) {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return nil, err
	}
	snapshot, err := backend.Lifecycle.Quiesce(ctx, instance)
	if err != nil {
		return nil, newOperationError("quiesce", kind, instance, err)
	}
	return snapshot, nil
}

// Suspend dispatches runtime suspension to the selected backend.
func (r *Router) Suspend(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return err
	}
	if err := backend.Lifecycle.Suspend(ctx, instance); err != nil {
		return newOperationError("suspend", kind, instance, err)
	}
	return nil
}

// Resume dispatches runtime resumption to the selected backend.
func (r *Router) Resume(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return err
	}
	if err := backend.Lifecycle.Resume(ctx, instance); err != nil {
		return newOperationError("resume", kind, instance, err)
	}
	return nil
}

// Delete dispatches runtime deletion to the selected backend.
func (r *Router) Delete(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return err
	}
	if err := backend.Lifecycle.Delete(ctx, instance); err != nil {
		return newOperationError("delete", kind, instance, err)
	}
	return nil
}

// Dial creates an A2A client through the same backend selected for lifecycle
// operations.
func (r *Router) Dial(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*a2aclient.Client, error) {
	kind, backend, err := r.selectBackend(ctx, instance)
	if err != nil {
		return nil, err
	}
	client, err := backend.Connector.Dial(ctx, instance)
	if err != nil {
		return nil, newOperationError("dial", kind, instance, err)
	}
	return client, nil
}

func (r *Router) selectBackend(ctx context.Context, instance *apiv1alpha1.AgentInstance) (Kind, Backend, error) {
	if instance == nil {
		return "", Backend{}, fmt.Errorf("runtime backend selection requires an AgentInstance")
	}
	kind, err := r.selector.Select(ctx, instance)
	if err != nil {
		return "", Backend{}, newOperationError("select", "", instance, err)
	}
	backend, exists := r.backends[kind]
	if !exists {
		return "", Backend{}, fmt.Errorf("runtime backend %q selected for AgentInstance %q is not registered", kind, instance.GetId())
	}
	return kind, backend, nil
}

func supported(kind Kind) bool {
	return kind == KindSubstrate || kind == KindExternal
}

func isNil(value any) bool {
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

// operationError preserves error identity for control-plane recovery while its
// printable form excludes backend errors that may contain private endpoints or
// credentials.
type operationError struct {
	operation  string
	kind       Kind
	instanceID string
	cause      error
}

func newOperationError(operation string, kind Kind, instance *apiv1alpha1.AgentInstance, cause error) error {
	return &operationError{operation: operation, kind: kind, instanceID: instance.GetId(), cause: cause}
}

func (e *operationError) Error() string {
	if e.kind == "" {
		return fmt.Sprintf("failed to %s runtime backend for AgentInstance %q", e.operation, e.instanceID)
	}
	return fmt.Sprintf("failed to %s with runtime backend %q for AgentInstance %q", e.operation, e.kind, e.instanceID)
}

func (e *operationError) Unwrap() error {
	return e.cause
}
