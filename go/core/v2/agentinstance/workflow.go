package agentinstance

import (
	"context"
	"errors"
	"fmt"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type workflowStore interface {
	MarkAgentInstanceReady(context.Context, string, string) (*apiv1alpha1.AgentInstance, error)
	TransitionAgentInstance(context.Context, *apiv1alpha1.AgentInstance, apiv1alpha1.AgentInstanceState, apiv1alpha1.AgentInstanceOperation) (*apiv1alpha1.AgentInstance, error)
	DeleteAgentInstance(context.Context, string) error
}

// RuntimeWorkflow coordinates durable AgentInstance state with an injected
// runtime lifecycle. It returns only when the requested operation finishes or
// the RPC context is canceled.
type RuntimeWorkflow struct {
	store   workflowStore
	runtime runtimebackend.Lifecycle
}

func NewRuntimeWorkflow(store workflowStore, runtime runtimebackend.Lifecycle) *RuntimeWorkflow {
	return &RuntimeWorkflow{store: store, runtime: runtime}
}

// Create converges a persisted CREATING instance to READY. Retries discover
// the existing runtime before creating it. The runtime implementation owns
// identity validation and recovery from an ambiguous previous attempt.
func (w *RuntimeWorkflow) Create(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*apiv1alpha1.AgentInstance, error) {
	if instance.GetState() == apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY {
		return instance, nil
	}
	if instance.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING {
		return nil, fmt.Errorf("AgentInstance %s is not creating", instance.GetId())
	}

	endpoint, err := w.runtime.Create(ctx, instance)
	if err != nil {
		return nil, fmt.Errorf("create AgentInstance runtime: %w", err)
	}
	if endpoint.A2AAuthority == "" {
		return nil, fmt.Errorf("create AgentInstance runtime: A2A authority is empty")
	}

	instance, err = w.store.MarkAgentInstanceReady(ctx, instance.GetId(), endpoint.A2AAuthority)
	if err != nil {
		return nil, fmt.Errorf("mark AgentInstance ready: %w", err)
	}
	return instance, nil
}

// Suspend completes synchronously: success means both the runtime and the
// AgentInstance record are suspended.
func (w *RuntimeWorkflow) Suspend(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*apiv1alpha1.AgentInstance, error) {
	instance, claimed, err := w.claim(ctx, instance,
		apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
		apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_SUSPEND,
	)
	if err != nil {
		return nil, err
	}
	err = w.runtime.Suspend(ctx, instance)
	if err != nil {
		return nil, w.release(ctx, instance, apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY, claimed, err)
	}
	return w.finish(ctx, instance,
		apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
		apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED,
	)
}

// Resume completes synchronously: success means the runtime is available and
// the AgentInstance record is ready.
func (w *RuntimeWorkflow) Resume(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*apiv1alpha1.AgentInstance, error) {
	instance, claimed, err := w.claim(ctx, instance,
		apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED,
		apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_RESUME,
	)
	if err != nil {
		return nil, err
	}
	err = w.runtime.Resume(ctx, instance)
	if err != nil {
		return nil, w.release(ctx, instance, apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED, claimed, err)
	}
	return w.finish(ctx, instance,
		apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED,
		apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	)
}

func (w *RuntimeWorkflow) claim(
	ctx context.Context,
	instance *apiv1alpha1.AgentInstance,
	expectedState apiv1alpha1.AgentInstanceState,
	operation apiv1alpha1.AgentInstanceOperation,
) (*apiv1alpha1.AgentInstance, bool, error) {
	// The operation is persisted with a compare-and-set before touching the
	// runtime. This lets every API replica reject a different mutation while
	// still allowing the same mutation to finish after a lost response. The
	// returned bool reports whether this call installed the marker; a retry
	// which finds the same operation joins it but must not later clear it.
	if instance.GetState() != expectedState {
		return nil, false, dbpkg.ErrAgentInstanceConflict
	}
	if instance.GetOperation() == operation {
		return instance, false, nil
	}
	if instance.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		return nil, false, dbpkg.ErrAgentInstanceConflict
	}
	next := proto.Clone(instance).(*apiv1alpha1.AgentInstance)
	next.Operation = operation
	next.UpdatedAt = timestamppb.Now()
	claimed, err := w.store.TransitionAgentInstance(ctx, next, expectedState, apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED)
	if errors.Is(err, dbpkg.ErrAgentInstanceConflict) && claimed.GetState() == expectedState && claimed.GetOperation() == operation {
		return claimed, false, nil
	}
	return claimed, err == nil, err
}

func (w *RuntimeWorkflow) finish(
	ctx context.Context,
	instance *apiv1alpha1.AgentInstance,
	expectedState, nextState apiv1alpha1.AgentInstanceState,
) (*apiv1alpha1.AgentInstance, error) {
	// Completing the operation uses a second compare-and-set so only the owner
	// of the active operation can publish the new stable state. If another retry
	// already committed that state, its result makes this retry successful too.
	next := proto.Clone(instance).(*apiv1alpha1.AgentInstance)
	next.State = nextState
	next.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	next.UpdatedAt = timestamppb.Now()
	current, err := w.store.TransitionAgentInstance(ctx, next, expectedState, instance.GetOperation())
	if errors.Is(err, dbpkg.ErrAgentInstanceConflict) && current.GetState() == nextState && current.GetOperation() == apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		return current, nil
	}
	return current, err
}

func (w *RuntimeWorkflow) release(ctx context.Context, instance *apiv1alpha1.AgentInstance, state apiv1alpha1.AgentInstanceState, claimed bool, operationErr error) error {
	// Only the request that installed the marker may clear it. A concurrent
	// retry must not release an operation that it merely joined. Clearing the
	// marker restores the last stable database state after the runtime call
	// failed, allowing a later request to retry the workflow.
	if !claimed {
		return operationErr
	}
	next := proto.Clone(instance).(*apiv1alpha1.AgentInstance)
	next.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	next.UpdatedAt = timestamppb.Now()
	_, err := w.store.TransitionAgentInstance(ctx, next, state, instance.GetOperation())
	if errors.Is(err, dbpkg.ErrAgentInstanceConflict) {
		return operationErr
	}
	return errors.Join(operationErr, err)
}

// Delete fences other lifecycle mutations with the same persisted operation
// marker used by Suspend and Resume. The runtime implementation owns recovery
// from an ambiguous previous deletion.
func (w *RuntimeWorkflow) Delete(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*apiv1alpha1.AgentInstance, error) {
	originalState := instance.GetState()
	instance, claimed, err := w.claim(ctx, instance, originalState, apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_DELETE)
	if err != nil {
		return nil, err
	}
	if err := w.runtime.Delete(ctx, instance); err != nil {
		return nil, w.release(ctx, instance, originalState, claimed, err)
	}
	return w.finishDelete(ctx, instance)
}

// finishDelete removes the durable AgentInstance row only after its runtime is
// gone. The returned message is detached from storage and scrubbed of runtime
// routing details so the synchronous Delete RPC can describe the completed
// operation without leaving a tombstone in the database.
func (w *RuntimeWorkflow) finishDelete(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*apiv1alpha1.AgentInstance, error) {
	if err := w.store.DeleteAgentInstance(ctx, instance.GetId()); err != nil {
		return nil, fmt.Errorf("delete AgentInstance: %w", err)
	}
	instance.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED
	instance.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	instance.PreparedRevision = ""
	instance.A2AAuthority = ""
	// TODO: Trigger runtime revision garbage collection outside the AgentInstance delete workflow.
	return instance, nil
}
