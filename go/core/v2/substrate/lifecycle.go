package substrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	legacysubstrate "github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type revisionStore interface {
	GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error)
}

type actorClient interface {
	EnsureAtespace(context.Context, string) error
	GetActor(context.Context, string, string) (*ateapipb.Actor, error)
	CreateActor(context.Context, string, string, string, string) (*ateapipb.Actor, error)
	ResumeActor(context.Context, string, string) (*ateapipb.Actor, error)
	SuspendActor(context.Context, string, string) error
	DeleteActor(context.Context, string, string) error
}

// Lifecycle implements the private AgentInstance runtime contract with one
// deterministic Substrate Actor per instance.
type Lifecycle struct {
	revisions revisionStore
	actors    actorClient
}

var _ runtimebackend.Lifecycle = (*Lifecycle)(nil)

func NewLifecycle(revisions revisionStore, actors actorClient) *Lifecycle {
	return &Lifecycle{revisions: revisions, actors: actors}
}

// Create converges the deterministic Actor to running. An existing Actor is
// accepted only when it still references the instance's prepared template.
func (l *Lifecycle) Create(ctx context.Context, instance *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return runtimebackend.Endpoint{}, err
	}
	atespace := instance.GetNamespace()
	name := actorName(instance.GetId())
	if err := l.actors.EnsureAtespace(ctx, atespace); err != nil {
		return runtimebackend.Endpoint{}, fmt.Errorf("ensure Atespace %s: %w", atespace, err)
	}

	actor, err := l.actors.GetActor(ctx, atespace, name)
	if status.Code(err) == codes.NotFound {
		actor, err = l.actors.CreateActor(ctx, atespace, name, revision.ActorTemplateNamespace, revision.ActorTemplateName)
	}
	if err != nil {
		return runtimebackend.Endpoint{}, fmt.Errorf("ensure Actor %s/%s: %w", atespace, name, err)
	}
	if err := validateActorTemplate(actor, revision, atespace, name); err != nil {
		return runtimebackend.Endpoint{}, err
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
		actor, err = l.actors.ResumeActor(ctx, atespace, name)
		if err != nil {
			return runtimebackend.Endpoint{}, fmt.Errorf("resume Actor %s/%s: %w", atespace, name, err)
		}
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			return runtimebackend.Endpoint{}, fmt.Errorf("resume Actor %s/%s returned status %s", atespace, name, actor.GetStatus().GetState())
		}
	}

	return runtimebackend.Endpoint{A2AAuthority: legacysubstrate.ActorHost(atespace, name, "")}, nil
}

// Suspend is lookup-only and never recreates a missing Actor.
func (l *Lifecycle) Suspend(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	actor, err := l.lifecycleActor(ctx, instance)
	if err != nil {
		return err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return nil
	case ateapipb.ActorState_ACTOR_STATE_RUNNING, ateapipb.ActorState_ACTOR_STATE_RESUMING, ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		return l.actors.SuspendActor(ctx, atespace, name)
	default:
		return fmt.Errorf("actor %s/%s cannot be suspended from status %s", atespace, name, actor.GetStatus().GetState())
	}
}

// Resume is lookup-only and never recreates a missing Actor.
func (l *Lifecycle) Resume(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	actor, err := l.lifecycleActor(ctx, instance)
	if err != nil {
		return err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_RUNNING:
		return nil
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_SUSPENDING, ateapipb.ActorState_ACTOR_STATE_RESUMING:
		actor, err = l.actors.ResumeActor(ctx, atespace, name)
		if err != nil {
			return err
		}
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			return fmt.Errorf("resume Actor %s/%s returned status %s", atespace, name, actor.GetStatus().GetState())
		}
		return nil
	default:
		return fmt.Errorf("actor %s/%s cannot be resumed from status %s", atespace, name, actor.GetStatus().GetState())
	}
}

// Delete treats a missing Actor as recovery from a previously completed
// deletion. Existing Actors are identity-checked and suspended before delete.
func (l *Lifecycle) Delete(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	actor, err := l.actors.GetActor(ctx, atespace, name)
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get Actor %s/%s for deletion: %w", atespace, name, err)
	}
	if err := validateActorTemplate(actor, revision, atespace, name); err != nil {
		return fmt.Errorf("refuse to delete Actor %s/%s: ActorTemplate changed", atespace, name)
	}
	// Substrate's suspend and delete RPCs each run their workflows to
	// completion, so no local status polling is needed between them.
	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_CRASHED, ateapipb.ActorState_ACTOR_STATE_DELETING:
	default:
		if err := l.actors.SuspendActor(ctx, atespace, name); err != nil && status.Code(err) != codes.NotFound {
			return fmt.Errorf("suspend Actor %s/%s before deletion: %w", atespace, name, err)
		}
	}
	if err := l.actors.DeleteActor(ctx, atespace, name); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete Actor %s/%s: %w", atespace, name, err)
	}
	return nil
}

func (l *Lifecycle) lifecycleActor(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*ateapipb.Actor, error) {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return nil, err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	actor, err := l.actors.GetActor(ctx, atespace, name)
	if err != nil {
		return nil, fmt.Errorf("get Actor %s/%s: %w", atespace, name, err)
	}
	if err := validateActorTemplate(actor, revision, atespace, name); err != nil {
		return nil, err
	}
	return actor, nil
}

func (l *Lifecycle) revision(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*dbpkg.RuntimeRevision, error) {
	revision, err := l.revisions.GetRuntimeRevision(ctx, instance.GetPreparedRevision())
	if err != nil {
		return nil, fmt.Errorf("load prepared revision: %w", err)
	}
	return revision, nil
}

func validateActorTemplate(actor *ateapipb.Actor, revision *dbpkg.RuntimeRevision, atespace, name string) error {
	if actor.GetActorTemplateNamespace() != revision.ActorTemplateNamespace || actor.GetActorTemplateName() != revision.ActorTemplateName {
		return fmt.Errorf("actor %s/%s uses unexpected ActorTemplate %s/%s", atespace, name, actor.GetActorTemplateNamespace(), actor.GetActorTemplateName())
	}
	return nil
}

func actorName(instanceID string) string { return "ai-" + strings.ToLower(instanceID) }
