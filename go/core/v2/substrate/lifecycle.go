package substrate

import (
	"context"
	"fmt"
	"strings"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type revisionStore interface {
	GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error)
}

type actorClient interface {
	EnsureAtespace(context.Context, string) error
	GetActor(context.Context, string, string) (*ateapipb.Actor, error)
	CreateActor(context.Context, string, string, string, string) (*ateapipb.Actor, error)
	CreateActorFromSnapshotTag(context.Context, string, string, string, string, string, string) (*ateapipb.Actor, error)
	ResumeActor(context.Context, string, string) (*ateapipb.Actor, error)
	SuspendActor(context.Context, string, string) (*ateapipb.Actor, error)
	GetActorSnapshot(context.Context, string, string) (*ateapipb.ActorSnapshot, error)
	DeleteActor(context.Context, string, string, bool) error
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

// Create converges the deterministic Actor into existence. KubernetesPod
// Actors remain suspended until their ordinary lifecycle resumes them, while
// ExternalSlot Actors must be running before their route is published because
// ResumeActor is what claims an external provider assignment.
func (l *Lifecycle) Create(ctx context.Context, instance *apiv1alpha1.AgentInstance) (runtimebackend.Endpoint, error) {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return runtimebackend.Endpoint{}, err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
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
	placement, err := dbpkg.NormalizeRuntimeRevisionPlacement(revision.Placement)
	if err != nil {
		return runtimebackend.Endpoint{}, fmt.Errorf("load prepared revision: %w", err)
	}
	if placement == dbpkg.RuntimeRevisionPlacementExternalSlot {
		// Always issue the idempotent resume, including after a retry discovers a
		// running Actor. This closes the crash window between provider assignment
		// and publishing READY in the AgentInstance row.
		actor, err = l.actors.ResumeActor(ctx, atespace, name)
		if err != nil {
			return runtimebackend.Endpoint{}, fmt.Errorf("resume ExternalSlot Actor %s/%s: %w", atespace, name, err)
		}
		if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_RUNNING {
			return runtimebackend.Endpoint{}, fmt.Errorf("resume ExternalSlot Actor %s/%s returned status %s", atespace, name, actor.GetStatus().GetState())
		}
	}
	return endpoint(atespace, name), nil
}

// Fork converges a deterministic, initially suspended Actor from the retained
// checkpoint snapshot tag and verifies the exact source snapshot identity.
func (l *Lifecycle) Fork(ctx context.Context, instance *apiv1alpha1.AgentInstance, checkpoint *dbpkg.AgentInstanceCheckpoint) (runtimebackend.Endpoint, error) {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return runtimebackend.Endpoint{}, err
	}
	if err := requireSnapshotLifecycle(revision, "fork"); err != nil {
		return runtimebackend.Endpoint{}, err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	if err := l.actors.EnsureAtespace(ctx, atespace); err != nil {
		return runtimebackend.Endpoint{}, fmt.Errorf("ensure Atespace %s: %w", atespace, err)
	}
	tag := &ateapipb.ObjectRef{Atespace: checkpoint.SnapshotAtespace, Name: "checkpoint-" + checkpoint.ID.String()}
	actor, err := l.actors.GetActor(ctx, atespace, name)
	if status.Code(err) == codes.NotFound {
		actor, err = l.actors.CreateActorFromSnapshotTag(ctx, atespace, name,
			revision.ActorTemplateNamespace, revision.ActorTemplateName, tag.GetAtespace(), tag.GetName())
	}
	if err != nil {
		return runtimebackend.Endpoint{}, fmt.Errorf("ensure fork Actor %s/%s: %w", atespace, name, err)
	}
	if err := validateActorTemplate(actor, revision, atespace, name); err != nil {
		return runtimebackend.Endpoint{}, err
	}
	if !proto.Equal(actor.GetSourceSnapshotTag(), tag) {
		return runtimebackend.Endpoint{}, fmt.Errorf("actor %s/%s uses unexpected source snapshot tag", atespace, name)
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return runtimebackend.Endpoint{}, fmt.Errorf("fork actor %s/%s is not suspended", atespace, name)
	}
	source := actor.GetStatus().GetSourceSnapshot()
	if source.GetSnapshot().GetAtespace() != checkpoint.SnapshotAtespace || source.GetSnapshot().GetName() != checkpoint.SnapshotName ||
		source.GetSnapshotUid() != checkpoint.SnapshotUID {
		return runtimebackend.Endpoint{}, fmt.Errorf("actor %s/%s uses unexpected source snapshot", atespace, name)
	}
	return endpoint(atespace, name), nil
}

// Quiesce durably suspends the Actor and resolves the exact immutable snapshot
// produced by that suspend operation.
func (l *Lifecycle) Quiesce(ctx context.Context, instance *apiv1alpha1.AgentInstance) (*dbpkg.AgentInstanceTaskSnapshot, error) {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return nil, err
	}
	if err := requireSnapshotLifecycle(revision, "quiesce"); err != nil {
		return nil, err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	actor, err := l.actors.SuspendActor(ctx, atespace, name)
	if err != nil {
		return nil, fmt.Errorf("suspend Actor %s/%s: %w", atespace, name, err)
	}
	if actor.GetStatus().GetState() != ateapipb.ActorState_ACTOR_STATE_SUSPENDED {
		return nil, fmt.Errorf("suspend Actor %s/%s returned status %s", atespace, name, actor.GetStatus().GetState())
	}
	ref := actor.GetStatus().GetLatestSnapshot()
	if ref.GetAtespace() == "" || ref.GetName() == "" {
		return nil, fmt.Errorf("suspend Actor %s/%s returned no snapshot", atespace, name)
	}
	snapshot, err := l.actors.GetActorSnapshot(ctx, ref.GetAtespace(), ref.GetName())
	if err != nil {
		return nil, fmt.Errorf("get ActorSnapshot %s/%s: %w", ref.GetAtespace(), ref.GetName(), err)
	}
	metadata := snapshot.GetMetadata()
	if metadata.GetAtespace() != ref.GetAtespace() || metadata.GetName() != ref.GetName() || metadata.GetUid() == "" {
		return nil, fmt.Errorf("ActorSnapshot %s/%s returned invalid identity", ref.GetAtespace(), ref.GetName())
	}
	scope := snapshot.GetStatus().GetContentScope()
	if scope != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_FULL && scope != ateapipb.SnapshotContentScope_SNAPSHOT_CONTENT_SCOPE_DATA {
		return nil, fmt.Errorf("ActorSnapshot %s/%s returned invalid content scope %s", ref.GetAtespace(), ref.GetName(), scope)
	}
	return &dbpkg.AgentInstanceTaskSnapshot{
		Atespace: metadata.GetAtespace(), Name: metadata.GetName(), UID: metadata.GetUid(),
		ContentScope: strings.TrimPrefix(scope.String(), "SNAPSHOT_CONTENT_SCOPE_"),
	}, nil
}

// Suspend is lookup-only and never recreates a missing Actor.
func (l *Lifecycle) Suspend(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return err
	}
	if err := requireSnapshotLifecycle(revision, "suspend"); err != nil {
		return err
	}
	actor, err := l.lifecycleActor(ctx, instance, revision)
	if err != nil {
		return err
	}
	atespace, name := instance.GetNamespace(), actorName(instance.GetId())
	switch actor.GetStatus().GetState() {
	case ateapipb.ActorState_ACTOR_STATE_SUSPENDED:
		return nil
	case ateapipb.ActorState_ACTOR_STATE_RUNNING, ateapipb.ActorState_ACTOR_STATE_RESUMING, ateapipb.ActorState_ACTOR_STATE_SUSPENDING:
		_, err = l.actors.SuspendActor(ctx, atespace, name)
		return err
	default:
		return fmt.Errorf("actor %s/%s cannot be suspended from status %s", atespace, name, actor.GetStatus().GetState())
	}
}

// Resume is lookup-only and never recreates a missing Actor.
func (l *Lifecycle) Resume(ctx context.Context, instance *apiv1alpha1.AgentInstance) error {
	revision, err := l.revision(ctx, instance)
	if err != nil {
		return err
	}
	if err := requireSnapshotLifecycle(revision, "resume"); err != nil {
		return err
	}
	actor, err := l.lifecycleActor(ctx, instance, revision)
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
	placement, err := dbpkg.NormalizeRuntimeRevisionPlacement(revision.Placement)
	if err != nil {
		return fmt.Errorf("load prepared revision: %w", err)
	}
	if placement == dbpkg.RuntimeRevisionPlacementKubernetesPod {
		switch actor.GetStatus().GetState() {
		case ateapipb.ActorState_ACTOR_STATE_SUSPENDED, ateapipb.ActorState_ACTOR_STATE_CRASHED, ateapipb.ActorState_ACTOR_STATE_DELETING:
		default:
			if _, err := l.actors.SuspendActor(ctx, atespace, name); err != nil && status.Code(err) != codes.NotFound {
				return fmt.Errorf("suspend Actor %s/%s before deletion: %w", atespace, name, err)
			}
		}
	}
	if err := l.actors.DeleteActor(ctx, atespace, name, placement == dbpkg.RuntimeRevisionPlacementExternalSlot); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete Actor %s/%s: %w", atespace, name, err)
	}
	return nil
}

func (l *Lifecycle) lifecycleActor(ctx context.Context, instance *apiv1alpha1.AgentInstance, revision *dbpkg.RuntimeRevision) (*ateapipb.Actor, error) {
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
	if revision == nil {
		return nil, fmt.Errorf("load prepared revision: empty result")
	}
	return revision, nil
}

func requireSnapshotLifecycle(revision *dbpkg.RuntimeRevision, operation string) error {
	placement, err := dbpkg.NormalizeRuntimeRevisionPlacement(revision.Placement)
	if err != nil {
		return status.Errorf(codes.FailedPrecondition, "%s runtime revision: %v", operation, err)
	}
	if placement == dbpkg.RuntimeRevisionPlacementExternalSlot {
		return status.Errorf(codes.FailedPrecondition, "%s is not supported for ExternalSlot runtime revisions", operation)
	}
	return nil
}

func endpoint(atespace, name string) runtimebackend.Endpoint {
	return runtimebackend.Endpoint{A2AAuthority: ActorHost(atespace, name, "")}
}

func validateActorTemplate(actor *ateapipb.Actor, revision *dbpkg.RuntimeRevision, atespace, name string) error {
	if actor.GetActorTemplateNamespace() != revision.ActorTemplateNamespace || actor.GetActorTemplateName() != revision.ActorTemplateName {
		return fmt.Errorf("actor %s/%s uses unexpected ActorTemplate %s/%s", atespace, name, actor.GetActorTemplateNamespace(), actor.GetActorTemplateName())
	}
	return nil
}

func actorName(instanceID string) string { return "ai-" + strings.ToLower(instanceID) }
