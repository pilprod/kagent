package agentinstance

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/google/uuid"
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
	store := &workflowTestStore{instance: instance}
	snapshot := &dbpkg.AgentInstanceTaskSnapshot{
		Atespace: "team-a", Name: "snapshot-1", UID: "snapshot-uid", ContentScope: "DATA",
	}
	runtime := &workflowTestRuntime{
		endpoint: runtimebackend.Endpoint{A2AAuthority: "runtime.internal"}, snapshot: snapshot,
	}
	workflow := NewRuntimeWorkflow(store, runtime)

	created, err := workflow.Create(t.Context(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || created.GetA2AAuthority() != "runtime.internal" {
		t.Fatalf("created instance = %+v", created)
	}
	boundary, err := workflow.Quiesce(t.Context(), created)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || boundary != snapshot {
		t.Fatalf("quiesced instance = %+v, boundary = %+v", created, boundary)
	}

	suspended, err := workflow.Suspend(t.Context(), created)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED || suspended.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		t.Fatalf("suspended instance = %+v", suspended)
	}

	resumed, err := workflow.Resume(t.Context(), suspended)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || resumed.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		t.Fatalf("resumed instance = %+v", resumed)
	}

	deleted, err := workflow.Delete(t.Context(), resumed)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED || store.instance != nil {
		t.Fatalf("deleted instance = %+v", deleted)
	}
	wantCalls := []string{"create", "quiesce", "suspend", "resume", "delete"}
	if !slices.Equal(runtime.calls, wantCalls) {
		t.Fatalf("runtime calls = %v, want %v", runtime.calls, wantCalls)
	}
}

func TestRuntimeWorkflowForkPublishesBackendAuthority(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "fork-1", Namespace: "team-a", PreparedRevision: "revision-1",
		State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING,
	}
	store := &workflowTestStore{instance: instance}
	checkpoint := &dbpkg.AgentInstanceCheckpoint{
		ID: uuid.MustParse("018f47a2-4efb-7c21-a848-123456789abc"), SnapshotAtespace: "team-a", SnapshotName: "snapshot-1", SnapshotUID: "snapshot-uid",
	}
	runtime := &workflowTestRuntime{endpoint: runtimebackend.Endpoint{A2AAuthority: "fork.internal"}}

	fork, err := NewRuntimeWorkflow(store, runtime).Fork(t.Context(), instance, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if fork.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || fork.GetA2AAuthority() != "fork.internal" {
		t.Fatalf("fork = %+v", fork)
	}
	if runtime.checkpoint != checkpoint {
		t.Fatalf("runtime checkpoint = %+v, want %+v", runtime.checkpoint, checkpoint)
	}
}

func TestRuntimeWorkflowRetriesJoinedOperationAfterDatabaseFailure(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a",
		PreparedRevision: "revision-1", A2AAuthority: "runtime.internal",
		State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	}
	store := &workflowTestStore{instance: instance, failFinishOnce: true}
	runtime := &workflowTestRuntime{}
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
	if want := []string{"suspend", "suspend"}; !slices.Equal(runtime.calls, want) {
		t.Fatalf("runtime calls = %v, want %v", runtime.calls, want)
	}
}

func TestRuntimeWorkflowRejectsEmptyBackendAuthority(t *testing.T) {
	tests := []struct {
		name string
		run  func(*RuntimeWorkflow, *apiv1alpha1.AgentInstance) error
	}{
		{
			name: "create",
			run: func(workflow *RuntimeWorkflow, instance *apiv1alpha1.AgentInstance) error {
				_, err := workflow.Create(t.Context(), instance)
				return err
			},
		},
		{
			name: "fork",
			run: func(workflow *RuntimeWorkflow, instance *apiv1alpha1.AgentInstance) error {
				_, err := workflow.Fork(t.Context(), instance, &dbpkg.AgentInstanceCheckpoint{})
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := &apiv1alpha1.AgentInstance{
				Id: "instance-1", State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING,
			}
			if err := test.run(NewRuntimeWorkflow(&workflowTestStore{instance: instance}, &workflowTestRuntime{}), instance); err == nil {
				t.Fatal("operation accepted an empty runtime authority")
			}
		})
	}
}

type workflowTestStore struct {
	instance       *apiv1alpha1.AgentInstance
	failFinishOnce bool
}

func (s *workflowTestStore) MarkAgentInstanceReady(_ context.Context, _ string, authority string) (*apiv1alpha1.AgentInstance, error) {
	s.instance.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY
	s.instance.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	s.instance.A2AAuthority = authority
	return s.instance, nil
}

func (s *workflowTestStore) TransitionAgentInstance(_ context.Context, instance *apiv1alpha1.AgentInstance, expectedState apiv1alpha1.AgentInstanceState, expectedOperation apiv1alpha1.AgentInstanceOperation) (*apiv1alpha1.AgentInstance, error) {
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

func (s *workflowTestStore) DeleteAgentInstance(context.Context, string) error {
	s.instance = nil
	return nil
}

type workflowTestRuntime struct {
	endpoint   runtimebackend.Endpoint
	snapshot   *dbpkg.AgentInstanceTaskSnapshot
	checkpoint *dbpkg.AgentInstanceCheckpoint
	calls      []string
}

var _ runtimebackend.Lifecycle = (*workflowTestRuntime)(nil)

func (r *workflowTestRuntime) Create(context.Context, *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	r.calls = append(r.calls, "create")
	return r.endpoint, nil
}

func (r *workflowTestRuntime) Fork(_ context.Context, _ *apiv1alpha1.AgentInstance, checkpoint *dbpkg.AgentInstanceCheckpoint) (runtimebackend.Endpoint, error) {
	r.calls = append(r.calls, "fork")
	r.checkpoint = checkpoint
	return r.endpoint, nil
}

func (r *workflowTestRuntime) Quiesce(context.Context, *apiv1alpha1.AgentInstance) (*dbpkg.AgentInstanceTaskSnapshot, error) {
	r.calls = append(r.calls, "quiesce")
	return r.snapshot, nil
}

func (r *workflowTestRuntime) Suspend(context.Context, *apiv1alpha1.AgentInstance) error {
	r.calls = append(r.calls, "suspend")
	return nil
}

func (r *workflowTestRuntime) Resume(context.Context, *apiv1alpha1.AgentInstance) error {
	r.calls = append(r.calls, "resume")
	return nil
}

func (r *workflowTestRuntime) Delete(context.Context, *apiv1alpha1.AgentInstance) error {
	r.calls = append(r.calls, "delete")
	return nil
}
