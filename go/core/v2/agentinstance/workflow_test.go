package agentinstance

import (
	"context"
	"errors"
	"strings"
	"testing"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"google.golang.org/protobuf/proto"
)

func TestRuntimeWorkflowLifecycle(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a",
		PreparedRevision: "revision-1", State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING,
	}
	store := &lifecycleTestStore{
		instance: instance,
	}
	runtime := &lifecycleTestRuntime{}
	workflow := NewRuntimeWorkflow(store, runtime)

	created, err := workflow.Create(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || created.GetA2AAuthority() == "" {
		t.Fatalf("created instance = %+v", created)
	}
	suspended, err := workflow.Suspend(context.Background(), created)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED || suspended.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		t.Fatalf("suspended instance = %+v", suspended)
	}
	resumed, err := workflow.Resume(context.Background(), suspended)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || resumed.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		t.Fatalf("resumed instance = %+v", resumed)
	}

	deleted, err := workflow.Delete(context.Background(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED || store.instance != nil {
		t.Fatalf("deleted instance = %+v", deleted)
	}
	if got := strings.Join(runtime.calls, ","); got != "create,suspend,resume,delete" {
		t.Fatalf("runtime calls = %q", got)
	}
}

func TestRuntimeWorkflowRetriesJoinedOperationAfterDatabaseFailure(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a",
		PreparedRevision: "revision-1", A2AAuthority: "private-runtime-authority",
		State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	}
	store := &lifecycleTestStore{instance: instance, failFinishOnce: true}
	runtime := &lifecycleTestRuntime{}
	workflow := NewRuntimeWorkflow(store, runtime)

	if _, err := workflow.Suspend(t.Context(), instance); err == nil {
		t.Fatal("Suspend() succeeded despite the injected database failure")
	}
	if store.instance.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || store.instance.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_SUSPEND {
		t.Fatalf("failed Suspend() instance = %+v", store.instance)
	}

	suspended, err := workflow.Suspend(t.Context(), store.instance)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED || suspended.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		t.Fatalf("retried Suspend() instance = %+v", suspended)
	}
	if got := strings.Join(runtime.calls, ","); got != "suspend,suspend" {
		t.Fatalf("runtime calls = %q, want the joined operation to converge on retry", got)
	}
}

type lifecycleTestStore struct {
	instance       *apiv1alpha1.AgentInstance
	failFinishOnce bool
}

func (s *lifecycleTestStore) MarkAgentInstanceReady(_ context.Context, _ string, authority string) (*apiv1alpha1.AgentInstance, error) {
	s.instance.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY
	s.instance.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	s.instance.A2AAuthority = authority
	return s.instance, nil
}

func (s *lifecycleTestStore) TransitionAgentInstance(_ context.Context, instance *apiv1alpha1.AgentInstance, expectedState apiv1alpha1.AgentInstanceState, expectedOperation apiv1alpha1.AgentInstanceOperation) (*apiv1alpha1.AgentInstance, error) {
	if s.instance.GetState() != expectedState || s.instance.GetOperation() != expectedOperation {
		return s.instance, dbpkg.ErrAgentInstanceConflict
	}
	if s.failFinishOnce && expectedOperation == apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_SUSPEND && instance.GetOperation() == apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		s.failFinishOnce = false
		return s.instance, errors.New("injected database failure")
	}
	s.instance = proto.Clone(instance).(*apiv1alpha1.AgentInstance)
	return s.instance, nil
}

func (s *lifecycleTestStore) DeleteAgentInstance(context.Context, string) error {
	s.instance = nil
	return nil
}

type lifecycleTestRuntime struct {
	calls []string
}

func (r *lifecycleTestRuntime) Create(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	r.calls = append(r.calls, "create")
	return runtimebackend.Endpoint{A2AAuthority: "private-runtime-authority"}, nil
}

func (r *lifecycleTestRuntime) Suspend(context.Context, *apiv1alpha1.AgentInstance) error {
	r.calls = append(r.calls, "suspend")
	return nil
}

func (r *lifecycleTestRuntime) Resume(context.Context, *apiv1alpha1.AgentInstance) error {
	r.calls = append(r.calls, "resume")
	return nil
}

func (r *lifecycleTestRuntime) Delete(context.Context, *apiv1alpha1.AgentInstance) error {
	r.calls = append(r.calls, "delete")
	return nil
}
