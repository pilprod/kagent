package substrate

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLifecyclePreservesSubstrateActorBehavior(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{
		Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a", PreparedRevision: "revision-1",
	}
	revisions := &lifecycleTestRevisions{revision: &dbpkg.RuntimeRevision{
		Revision: "revision-1", ActorTemplateNamespace: "team-a", ActorTemplateName: "assistant-kagent-revision",
	}}
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}}
	lifecycle := NewLifecycle(revisions, actors)

	endpoint, err := lifecycle.Create(t.Context(), instance)
	if err != nil {
		t.Fatal(err)
	}
	if endpoint.A2AAuthority == "" || len(actors.actors) != 1 {
		t.Fatalf("Create() endpoint = %+v, actors = %v", endpoint, actors.actors)
	}
	if _, err := lifecycle.Create(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	if actors.createCalls != 1 || len(actors.actors) != 1 {
		t.Fatalf("retried Create() made %d create calls, actors = %v", actors.createCalls, actors.actors)
	}
	actor := actors.actors[actorKey("team-a", actorName(instance.GetId()))]
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("created Actor status = %s", actor.GetStatus().GetState())
	}

	if err := lifecycle.Suspend(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("suspended Actor status = %s", actor.GetStatus().GetState())
	}
	if err := lifecycle.Resume(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("resumed Actor status = %s", actor.GetStatus().GetState())
	}
	if err := lifecycle.Delete(t.Context(), instance); err != nil {
		t.Fatal(err)
	}
	if len(actors.actors) != 0 {
		t.Fatalf("Delete() actors = %v", actors.actors)
	}
}

func TestLifecycleRejectsMismatchedActor(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a", PreparedRevision: "revision-1"}
	revisions := &lifecycleTestRevisions{revision: &dbpkg.RuntimeRevision{
		Revision: "revision-1", ActorTemplateNamespace: "team-a", ActorTemplateName: "expected-template",
	}}
	name := actorName(instance.GetId())
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{
		actorKey("team-a", name): {
			ActorTemplateNamespace: "team-a", ActorTemplateName: "other-template",
			Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_RUNNING},
		},
	}}

	if _, err := NewLifecycle(revisions, actors).Create(t.Context(), instance); err == nil || !strings.Contains(err.Error(), "unexpected ActorTemplate") {
		t.Fatalf("Create() error = %v", err)
	}
	if actors.createCalls != 0 {
		t.Fatalf("Create() replaced a mismatched Actor")
	}
}

func TestLifecycleMissingActorSemantics(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a", PreparedRevision: "revision-1"}
	revisions := &lifecycleTestRevisions{revision: &dbpkg.RuntimeRevision{
		Revision: "revision-1", ActorTemplateNamespace: "team-a", ActorTemplateName: "expected-template",
	}}
	lifecycle := NewLifecycle(revisions, &lifecycleTestActors{actors: map[string]*ateapipb.Actor{}})

	if err := lifecycle.Suspend(t.Context(), instance); err == nil {
		t.Fatal("Suspend() recreated or accepted a missing Actor")
	}
	if err := lifecycle.Resume(t.Context(), instance); err == nil {
		t.Fatal("Resume() recreated or accepted a missing Actor")
	}
	if err := lifecycle.Delete(t.Context(), instance); err != nil {
		t.Fatalf("Delete() did not converge after a missing Actor: %v", err)
	}
}

func TestLifecycleJoinsTransitionalActorStates(t *testing.T) {
	instance := &apiv1alpha1.AgentInstance{Id: "8bd650a8-9775-488f-8bc1-0d52bf7bdcab", Namespace: "team-a", PreparedRevision: "revision-1"}
	revision := &dbpkg.RuntimeRevision{Revision: "revision-1", ActorTemplateNamespace: "team-a", ActorTemplateName: "expected-template"}
	actor := &ateapipb.Actor{
		ActorTemplateNamespace: revision.ActorTemplateNamespace, ActorTemplateName: revision.ActorTemplateName,
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDING},
	}
	actors := &lifecycleTestActors{actors: map[string]*ateapipb.Actor{actorKey("team-a", actorName(instance.GetId())): actor}}
	lifecycle := NewLifecycle(&lifecycleTestRevisions{revision: revision}, actors)

	if err := lifecycle.Suspend(t.Context(), instance); err != nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		t.Fatalf("Suspend() = %v, state = %s", err, actor.GetStatus().GetState())
	}
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RESUMING
	if err := lifecycle.Resume(t.Context(), instance); err != nil || actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("Resume() = %v, state = %s", err, actor.GetStatus().GetState())
	}
}

func TestConnectorRequiresAuthority(t *testing.T) {
	if _, err := (&Connector{}).Dial(t.Context(), &apiv1alpha1.AgentInstance{}); err == nil {
		t.Fatal("Dial() accepted an empty runtime authority")
	}
}

type lifecycleTestRevisions struct {
	revision *dbpkg.RuntimeRevision
}

func (s *lifecycleTestRevisions) GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error) {
	return s.revision, nil
}

type lifecycleTestActors struct {
	actors      map[string]*ateapipb.Actor
	createCalls int
}

func actorKey(atespace, name string) string { return atespace + "/" + name }

func (*lifecycleTestActors) EnsureAtespace(context.Context, string) error { return nil }

func (a *lifecycleTestActors) GetActor(_ context.Context, atespace, name string) (*ateapipb.Actor, error) {
	actor := a.actors[actorKey(atespace, name)]
	if actor == nil {
		return nil, status.Error(codes.NotFound, "missing")
	}
	return actor, nil
}

func (a *lifecycleTestActors) CreateActor(_ context.Context, atespace, name, templateNamespace, templateName string) (*ateapipb.Actor, error) {
	a.createCalls++
	actor := &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: atespace, Name: name, Uid: "actor-uid"},
		ActorTemplateNamespace: templateNamespace, ActorTemplateName: templateName,
		Status: &ateapipb.ActorStatus{State: ateapipb.ActorState_ACTOR_STATE_SUSPENDED},
	}
	a.actors[actorKey(atespace, name)] = actor
	return actor, nil
}

func (a *lifecycleTestActors) ResumeActor(_ context.Context, atespace, name string) (*ateapipb.Actor, error) {
	actor := a.actors[actorKey(atespace, name)]
	actor.Status.State = ateapipb.ActorState_ACTOR_STATE_RUNNING
	return actor, nil
}

func (a *lifecycleTestActors) SuspendActor(_ context.Context, atespace, name string) error {
	a.actors[actorKey(atespace, name)].Status.State = ateapipb.ActorState_ACTOR_STATE_SUSPENDED
	return nil
}

func (a *lifecycleTestActors) DeleteActor(_ context.Context, atespace, name string) error {
	delete(a.actors, actorKey(atespace, name))
	return nil
}
