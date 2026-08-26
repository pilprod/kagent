package runtimebackend_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type revisionStoreFunc func(context.Context, string) (*dbpkg.RuntimeRevision, error)

func (f revisionStoreFunc) GetRuntimeRevision(ctx context.Context, revision string) (*dbpkg.RuntimeRevision, error) {
	return f(ctx, revision)
}

func TestRevisionSelectorUsesOnlyPreparedRevisionIdentity(t *testing.T) {
	tests := []struct {
		name     string
		revision dbpkg.RuntimeRevision
		want     runtimebackend.Kind
	}{
		{
			name:     "substrate",
			revision: dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindSubstrate},
			want:     runtimebackend.KindSubstrate,
		},
		{
			name:     "external codex",
			revision: dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex},
			want:     runtimebackend.KindExternal,
		},
		{
			name:     "external claude",
			revision: dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeClaude},
			want:     runtimebackend.KindExternal,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			instance := &apiv1alpha1.AgentInstance{
				Id: "instance-1", PreparedRevision: "prepared-revision",
				A2AAuthority: "external-looking.example",
				Labels:       map[string]string{"runtime": "must-not-be-inferred"},
			}
			var loadedRevision string
			selector, err := runtimebackend.NewRevisionSelector(revisionStoreFunc(func(_ context.Context, revision string) (*dbpkg.RuntimeRevision, error) {
				loadedRevision = revision
				result := test.revision
				return &result, nil
			}))
			require.NoError(t, err)

			kind, err := selector.Select(t.Context(), instance)
			require.NoError(t, err)
			assert.Equal(t, test.want, kind)
			assert.Equal(t, instance.GetPreparedRevision(), loadedRevision)
		})
	}
}

func TestRevisionSelectorRejectsInvalidPersistedIdentity(t *testing.T) {
	const credential = "credential-shaped-unknown"
	tests := []struct {
		name     string
		revision *dbpkg.RuntimeRevision
	}{
		{name: "nil revision"},
		{name: "missing kind", revision: &dbpkg.RuntimeRevision{}},
		{name: "unknown kind", revision: &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKind(credential)}},
		{name: "substrate runtime", revision: &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindSubstrate, ExternalRuntime: dbpkg.ExternalRuntimeCodex}},
		{name: "external missing runtime", revision: &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindExternal}},
		{name: "external unknown runtime", revision: &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntime(credential)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selector, err := runtimebackend.NewRevisionSelector(revisionStoreFunc(func(context.Context, string) (*dbpkg.RuntimeRevision, error) {
				return test.revision, nil
			}))
			require.NoError(t, err)
			kind, err := selector.Select(t.Context(), &apiv1alpha1.AgentInstance{Id: "instance-1", PreparedRevision: credential})
			assert.Empty(t, kind)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), credential)
		})
	}
}

func TestRevisionSelectorPreservesContextAndSanitizesStoreErrors(t *testing.T) {
	type contextKey struct{}
	const credential = "private-database-credential"
	cause := fmt.Errorf("query postgres://user:%s@database.invalid", credential)
	wantContextValue := new(int)
	ctx := context.WithValue(t.Context(), contextKey{}, wantContextValue)
	selector, err := runtimebackend.NewRevisionSelector(revisionStoreFunc(func(gotCtx context.Context, revision string) (*dbpkg.RuntimeRevision, error) {
		assert.Same(t, wantContextValue, gotCtx.Value(contextKey{}))
		assert.Equal(t, "prepared-revision", revision)
		return nil, cause
	}))
	require.NoError(t, err)

	_, err = selector.Select(ctx, &apiv1alpha1.AgentInstance{Id: "instance-1", PreparedRevision: "prepared-revision"})
	require.ErrorIs(t, err, cause)
	assert.NotContains(t, err.Error(), credential)
}

func TestRevisionSelectorPropagatesCancellationIdentity(t *testing.T) {
	selector, err := runtimebackend.NewRevisionSelector(revisionStoreFunc(func(ctx context.Context, _ string) (*dbpkg.RuntimeRevision, error) {
		return nil, ctx.Err()
	}))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = selector.Select(ctx, &apiv1alpha1.AgentInstance{Id: "instance-1", PreparedRevision: "prepared-revision"})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestRevisionSelectorValidatesDependenciesAndInput(t *testing.T) {
	var typedNil revisionStoreFunc
	for _, store := range []revisionStoreFunc{nil, typedNil} {
		selector, err := runtimebackend.NewRevisionSelector(store)
		require.ErrorContains(t, err, "store is nil")
		assert.Nil(t, selector)
	}

	selector, err := runtimebackend.NewRevisionSelector(revisionStoreFunc(func(context.Context, string) (*dbpkg.RuntimeRevision, error) {
		return &dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindSubstrate}, nil
	}))
	require.NoError(t, err)
	_, err = selector.Select(t.Context(), nil)
	require.ErrorContains(t, err, "requires an AgentInstance")
	_, err = selector.Select(t.Context(), &apiv1alpha1.AgentInstance{Id: "instance-1"})
	require.ErrorContains(t, err, "failed to select persisted")
	_, err = selector.Select(nil, &apiv1alpha1.AgentInstance{Id: "instance-1", PreparedRevision: "revision"})
	require.ErrorContains(t, err, "requires a context")
}
