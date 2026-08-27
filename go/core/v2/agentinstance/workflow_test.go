package agentinstance

import (
	"context"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/google/uuid"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestActorWorkflowLifecycle(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a",
		PreparedRevision: "revision-1", State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING,
	}
	store := &lifecycleTestStore{
		instance: instance,
		revision: &dbpkg.RuntimeRevision{
			Revision: "revision-1", ActorTemplateNamespace: "team-a", ActorTemplateName: "assistant-kagent-revision",
		},
	}
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	workflow := NewActorWorkflow(store, actors)

	created, err := workflow.Create(context.Background(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || created.GetA2AAuthority() == "" {
		t.Fatalf("created instance = %+v", created)
	}
	if len(actors.actors) != 1 {
		t.Fatalf("actors = %v", actors.actors)
	}
	if actor := actors.actors[actorKey("team-a", actorName(instance.GetId()))]; actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("created Actor status = %s", actor.GetStatus().GetState())
	}
	boundary, err := workflow.Quiesce(context.Background(), created)
	if err != nil {
		t.Fatal(err)
	}
	if created.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY || boundary.Name != "snapshot-1" || boundary.UID != "snapshot-uid" {
		t.Fatalf("quiesced instance = %+v, boundary = %+v", created, boundary)
	}

	suspended, err := workflow.Suspend(context.Background(), created)
	if err != nil {
		t.Fatal(err)
	}
	if suspended.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED || suspended.GetOperation() != apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED {
		t.Fatalf("suspended instance = %+v", suspended)
	}
	if actor := actors.actors[actorKey("team-a", actorName(instance.GetId()))]; actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("suspended Actor status = %s", actor.GetStatus().GetState())
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
	if deleted.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED || store.instance != nil || len(actors.actors) != 0 {
		t.Fatalf("deleted instance = %+v, actors = %v", deleted, actors.actors)
	}
	if actors.suspendCalls != 2 || actors.deleteCalls != 1 || actors.deleteAnyState {
		t.Fatalf("KubernetesPod lifecycle suspend calls = %d, delete calls = %d, any state = %t", actors.suspendCalls, actors.deleteCalls, actors.deleteAnyState)
	}
}

func TestActorWorkflowForkCreatesSuspendedActorFromCheckpoint(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "fork-1", Namespace: "team-a", PreparedRevision: "revision-1",
		State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING,
	}
	store := &lifecycleTestStore{
		instance: instance,
		revision: &dbpkg.RuntimeRevision{
			Revision: "revision-1", ActorTemplateNamespace: "team-a", ActorTemplateName: "assistant-kagent-revision",
		},
	}
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	checkpoint := &dbpkg.AgentInstanceCheckpoint{
		ID: uuid.MustParse("018f47a2-4efb-7c21-a848-123456789abc"), SnapshotAtespace: "team-a", SnapshotName: "snapshot-1", SnapshotUID: "snapshot-uid",
	}
	fork, err := NewActorWorkflow(store, actors).Fork(context.Background(), instance, checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	actor := actors.actors[actorKey("team-a", actorName(instance.GetId()))]
	if fork.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY ||
		actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED ||
		actor.GetSourceSnapshotTag().GetName() != "checkpoint-018f47a2-4efb-7c21-a848-123456789abc" {
		t.Fatalf("fork = %+v, actor = %+v", fork, actor)
	}
	instance.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING
	store.instance = instance
	actor.SourceSnapshotTag.Name = "wrong-tag"
	if _, err := NewActorWorkflow(store, actors).Fork(context.Background(), instance, checkpoint); err == nil {
		t.Fatal("Fork() accepted an existing Actor with the wrong snapshot tag")
	}
}

func TestActorWorkflowExternalSlotRejectsSnapshotLifecycleBeforeMutation(t *testing.T) {
	checkpoint := &dbpkg.AgentInstanceCheckpoint{
		ID: "checkpoint-1", SnapshotAtespace: "team-a", SnapshotName: "snapshot-1", SnapshotUID: "snapshot-uid",
	}
	tests := []struct {
		name  string
		state apiv1alpha1.AgentInstanceState
		run   func(*ActorWorkflow, *apiv1alpha1.AgentInstance) error
	}{
		{name: "quiesce", state: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY, run: func(workflow *ActorWorkflow, instance *apiv1alpha1.AgentInstance) error {
			_, err := workflow.Quiesce(t.Context(), instance)
			return err
		}},
		{name: "suspend", state: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY, run: func(workflow *ActorWorkflow, instance *apiv1alpha1.AgentInstance) error {
			_, err := workflow.Suspend(t.Context(), instance)
			return err
		}},
		{name: "resume", state: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED, run: func(workflow *ActorWorkflow, instance *apiv1alpha1.AgentInstance) error {
			_, err := workflow.Resume(t.Context(), instance)
			return err
		}},
		{name: "fork", state: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_CREATING, run: func(workflow *ActorWorkflow, instance *apiv1alpha1.AgentInstance) error {
			_, err := workflow.Fork(t.Context(), instance, checkpoint)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := &apiv1alpha1.AgentInstance{
				Id: "external-1", Namespace: "team-a", PreparedRevision: "revision-external", State: test.state,
			}
			store := &lifecycleTestStore{
				instance: instance,
				revision: &dbpkg.RuntimeRevision{
					Revision: "revision-external", Placement: dbpkg.RuntimeRevisionPlacementExternalSlot,
					ActorTemplateNamespace: "team-a", ActorTemplateName: "assistant-codex-revision",
				},
			}
			actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}

			err := test.run(NewActorWorkflow(store, actors), instance)
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("error = %v, want FailedPrecondition", err)
			}
			if store.transitionCalls != 0 || actors.ensureCalls != 0 || actors.suspendCalls != 0 || actors.resumeCalls != 0 || actors.createFromSnapshotCalls != 0 {
				t.Fatalf("mutation calls: transitions=%d ensure=%d suspend=%d resume=%d create-from-snapshot=%d",
					store.transitionCalls, actors.ensureCalls, actors.suspendCalls, actors.resumeCalls, actors.createFromSnapshotCalls)
			}
		})
	}
}

func TestActorWorkflowExternalSlotDeleteSkipsSuspend(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "external-1", Namespace: "team-a", PreparedRevision: "revision-external",
		State: apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY,
	}
	store := &lifecycleTestStore{
		instance: instance,
		revision: &dbpkg.RuntimeRevision{
			Revision: "revision-external", Placement: dbpkg.RuntimeRevisionPlacementExternalSlot,
			ActorTemplateNamespace: "team-a", ActorTemplateName: "assistant-codex-revision",
		},
	}
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{
		actorKey("team-a", actorName(instance.GetId())): {
			Metadata:               &ateapipb.ResourceMetadata{Atespace: "team-a", Name: actorName(instance.GetId()), Uid: "actor-uid"},
			ActorTemplateNamespace: "team-a", ActorTemplateName: "assistant-codex-revision",
			Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
		},
	}}

	deleted, err := NewActorWorkflow(store, actors).Delete(t.Context(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.GetState() != apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_DELETED || store.instance != nil {
		t.Fatalf("deleted instance = %+v, stored = %+v", deleted, store.instance)
	}
	if actors.suspendCalls != 0 || actors.deleteCalls != 1 || !actors.deleteAnyState {
		t.Fatalf("suspend calls = %d, delete calls = %d, any state = %t", actors.suspendCalls, actors.deleteCalls, actors.deleteAnyState)
	}
}

type lifecycleTestStore struct {
	instance        *apiv1alpha1.AgentInstance
	revision        *dbpkg.RuntimeRevision
	transitionCalls int
}

func (s *lifecycleTestStore) GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error) {
	return s.revision, nil
}

func (s *lifecycleTestStore) MarkAgentInstanceReady(_ context.Context, _ string, authority string) (*apiv1alpha1.AgentInstance, error) {
	s.instance.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_READY
	s.instance.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	s.instance.A2AAuthority = authority
	return s.instance, nil
}

func (s *lifecycleTestStore) TransitionAgentInstance(_ context.Context, instance *apiv1alpha1.AgentInstance, expectedState apiv1alpha1.AgentInstanceState, expectedOperation apiv1alpha1.AgentInstanceOperation) (*apiv1alpha1.AgentInstance, error) {
	s.transitionCalls++
	if s.instance.GetState() != expectedState || s.instance.GetOperation() != expectedOperation {
		return s.instance, dbpkg.ErrAgentInstanceConflict
	}
	s.instance = proto.Clone(instance).(*apiv1alpha1.AgentInstance)
	return s.instance, nil
}

func (s *lifecycleTestStore) DeleteAgentInstance(context.Context, string) error {
	s.instance = nil
	return nil
}

type lifecycleTestActors struct {
	actors                  map[string]*ateapipb.Actor
	ensureCalls             int
	suspendCalls            int
	resumeCalls             int
	createFromSnapshotCalls int
	deleteCalls             int
	deleteAnyState          bool
}

func actorKey(atespace, name string) string { return atespace + "/" + name }

func (a *lifecycleTestActors) EnsureAtespace(context.Context, string) error {
	a.ensureCalls++
	return nil
}

func (a *lifecycleTestActors) GetActor(_ context.Context, atespace, name string) (*ateapipb.Actor, error) {
	actor := a.actors[actorKey(atespace, name)]
	if actor == nil {
		return nil, status.Error(codes.NotFound, "missing")
	}
	return actor, nil
}

func (a *lifecycleTestActors) CreateActor(_ context.Context, atespace, name, templateNamespace, templateName string) (*ateapipb.Actor, error) {
	actor := &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: "actor-uid"},
		ActorTemplateNamespace: templateNamespace, ActorTemplateName: templateName,
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	}
	a.actors[actorKey(atespace, name)] = actor
	return actor, nil
}

func (a *lifecycleTestActors) CreateActorFromSnapshotTag(_ context.Context, atespace, name, templateNamespace, templateName, tagAtespace, tagName string) (*ateapipb.Actor, error) {
	a.createFromSnapshotCalls++
	actor := &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: "actor-uid"},
		ActorTemplateNamespace: templateNamespace, ActorTemplateName: templateName,
		SourceSnapshotTag: &ateapipb.ObjectRef{Atespace: tagAtespace, Name: tagName},
		Status: &ateapipb.ActorStatus{
			State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED,
			SourceSnapshot: &ateapipb.ActorSourceSnapshotStatus{
				Snapshot: &ateapipb.ObjectRef{Atespace: tagAtespace, Name: "snapshot-1"}, SnapshotUid: "snapshot-uid",
			},
		},
	}
	a.actors[actorKey(atespace, name)] = actor
	return actor, nil
}

func (a *lifecycleTestActors) ResumeActor(_ context.Context, atespace, name string) (*ateapipb.Actor, error) {
	a.resumeCalls++
	actor := a.actors[actorKey(atespace, name)]
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	return actor, nil
}

func (a *lifecycleTestActors) SuspendActor(_ context.Context, atespace, name string) (*ateapipb.Actor, error) {
	a.suspendCalls++
	actor := a.actors[actorKey(atespace, name)]
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
	actor.Status.LatestSnapshot = &ateapipb.ObjectRef{Atespace: atespace, Name: "snapshot-1"}
	return actor, nil
}

func (a *lifecycleTestActors) GetActorSnapshot(_ context.Context, atespace, name string) (*ateapipb.ActorSnapshot, error) {
	return &ateapipb.ActorSnapshot{
		Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: "snapshot-uid"},
		Status:   &ateapipb.ActorSnapshotStatus{ContentScope: ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA},
	}, nil
}

func (a *lifecycleTestActors) DeleteActor(_ context.Context, atespace, name string, anyState bool) error {
	a.deleteCalls++
	a.deleteAnyState = anyState
	actor := a.actors[actorKey(atespace, name)]
	if actor != nil && actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING && !anyState {
		return status.Error(codes.FailedPrecondition, "running actor requires any_state deletion")
	}
	delete(a.actors, actorKey(atespace, name))
	return nil
}
