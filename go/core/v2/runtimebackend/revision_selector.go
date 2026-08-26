package runtimebackend

import (
	"context"
	"fmt"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

type runtimeRevisionStore interface {
	GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error)
}

// RevisionSelector resolves the private backend identity pinned to an
// AgentInstance's prepared runtime revision.
type RevisionSelector struct {
	store runtimeRevisionStore
}

var _ Selector = (*RevisionSelector)(nil)

// NewRevisionSelector constructs a selector backed only by persisted runtime
// revisions. It never infers routing from names, labels, or authorities.
func NewRevisionSelector(store runtimeRevisionStore) (*RevisionSelector, error) {
	if isNil(store) {
		return nil, fmt.Errorf("runtime revision store is nil")
	}
	return &RevisionSelector{store: store}, nil
}

// Select loads and validates the exact prepared revision before translating
// its database kind into a registered runtime backend kind.
func (s *RevisionSelector) Select(ctx context.Context, instance *apiv1alpha1.AgentInstance) (Kind, error) {
	if ctx == nil {
		return "", fmt.Errorf("runtime backend selection requires a context")
	}
	if instance == nil {
		return "", fmt.Errorf("runtime backend selection requires an AgentInstance")
	}
	if instance.GetPreparedRevision() == "" {
		return "", newRevisionSelectionError(instance, fmt.Errorf("prepared revision is empty"))
	}

	revision, err := s.store.GetRuntimeRevision(ctx, instance.GetPreparedRevision())
	if err != nil {
		return "", newRevisionSelectionError(instance, err)
	}
	if revision == nil {
		return "", newRevisionSelectionError(instance, fmt.Errorf("runtime revision store returned nil"))
	}
	if err := revision.ValidateBackendIdentity(); err != nil {
		return "", newRevisionSelectionError(instance, err)
	}

	switch revision.BackendKind {
	case dbpkg.RuntimeBackendKindSubstrate:
		return KindSubstrate, nil
	case dbpkg.RuntimeBackendKindExternal:
		return KindExternal, nil
	default:
		return "", newRevisionSelectionError(instance, fmt.Errorf("runtime revision backend kind is invalid"))
	}
}

type revisionSelectionError struct {
	instanceID string
	cause      error
}

func newRevisionSelectionError(instance *apiv1alpha1.AgentInstance, cause error) error {
	return &revisionSelectionError{instanceID: instance.GetId(), cause: cause}
}

func (e *revisionSelectionError) Error() string {
	return fmt.Sprintf("failed to select persisted runtime backend for AgentInstance %q", e.instanceID)
}

func (e *revisionSelectionError) Unwrap() error {
	return e.cause
}
