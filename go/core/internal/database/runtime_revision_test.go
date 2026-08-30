package database

import (
	"context"
	"errors"
	"testing"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	dbgen "github.com/kagent-dev/kagent/go/core/internal/database/gen"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRevisionBackendIdentityPersistence(t *testing.T) {
	client := NewClient(setupTestDB(t))

	substrate := testRuntimeRevision("substrate-revision", "substrate-actor")
	substrate.BackendKind = dbpkg.RuntimeBackendKindSubstrate
	require.NoError(t, client.UpsertRuntimeRevision(t.Context(), substrate))
	storedSubstrate, err := client.GetRuntimeRevision(t.Context(), substrate.Revision)
	require.NoError(t, err)
	assert.Equal(t, dbpkg.RuntimeBackendKindSubstrate, storedSubstrate.BackendKind)
	assert.Empty(t, storedSubstrate.ExternalRuntime)

	external := testRuntimeRevision("external-revision", "external-actor")
	external.BackendKind = dbpkg.RuntimeBackendKindExternal
	external.ExternalRuntime = dbpkg.ExternalRuntimeCodex
	require.NoError(t, client.UpsertRuntimeRevision(t.Context(), external))
	storedExternal, err := client.GetRuntimeRevision(t.Context(), external.Revision)
	require.NoError(t, err)
	assert.Equal(t, dbpkg.RuntimeBackendKindExternal, storedExternal.BackendKind)
	assert.Equal(t, dbpkg.ExternalRuntimeCodex, storedExternal.ExternalRuntime)

	listed, err := client.ListUnreferencedRuntimeRevisions(t.Context())
	require.NoError(t, err)
	require.Len(t, listed, 2)
	kinds := map[string]dbpkg.RuntimeBackendKind{}
	for _, revision := range listed {
		kinds[revision.Revision] = revision.BackendKind
	}
	assert.Equal(t, dbpkg.RuntimeBackendKindSubstrate, kinds[substrate.Revision])
	assert.Equal(t, dbpkg.RuntimeBackendKindExternal, kinds[external.Revision])
}

func TestUpsertRuntimeRevisionRequiresValidExplicitBackendIdentity(t *testing.T) {
	client := NewClient(setupTestDB(t))
	const credential = "credential-shaped-unknown"

	tests := []struct {
		name    string
		kind    dbpkg.RuntimeBackendKind
		runtime dbpkg.ExternalRuntime
	}{
		{name: "missing kind"},
		{name: "unknown kind", kind: dbpkg.RuntimeBackendKind(credential)},
		{name: "substrate with runtime", kind: dbpkg.RuntimeBackendKindSubstrate, runtime: dbpkg.ExternalRuntimeCodex},
		{name: "external missing runtime", kind: dbpkg.RuntimeBackendKindExternal},
		{name: "external unknown runtime", kind: dbpkg.RuntimeBackendKindExternal, runtime: dbpkg.ExternalRuntime(credential)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			revision := testRuntimeRevision("invalid-"+test.name, "actor-"+test.name)
			revision.BackendKind = test.kind
			revision.ExternalRuntime = test.runtime
			err := client.UpsertRuntimeRevision(t.Context(), revision)
			require.Error(t, err)
			assert.NotContains(t, err.Error(), credential)
		})
	}
}

func TestUpsertRuntimeRevisionDoesNotChangePersistedBackendIdentity(t *testing.T) {
	client := NewClient(setupTestDB(t))
	revision := testRuntimeRevision("immutable-revision", "immutable-actor")
	revision.BackendKind = dbpkg.RuntimeBackendKindSubstrate
	require.NoError(t, client.UpsertRuntimeRevision(t.Context(), revision))

	revision.BackendKind = dbpkg.RuntimeBackendKindExternal
	revision.ExternalRuntime = dbpkg.ExternalRuntimeClaude
	err := client.UpsertRuntimeRevision(t.Context(), revision)
	require.ErrorContains(t, err, "failed to upsert")

	stored, err := client.GetRuntimeRevision(t.Context(), revision.Revision)
	require.NoError(t, err)
	assert.Equal(t, dbpkg.RuntimeBackendKindSubstrate, stored.BackendKind)
	assert.Empty(t, stored.ExternalRuntime)
}

func TestRuntimeRevisionMappingRejectsUnknownIdentityWithoutLeakingIt(t *testing.T) {
	const credential = "credential-shaped-unknown"
	credentialValue := credential
	_, err := runtimeRevisionFromRow(dbgen.RuntimeRevision{
		BackendKind:     credential,
		ExternalRuntime: &credentialValue,
	})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), credential)
}

func TestUpsertRuntimeRevisionPropagatesContextIdentity(t *testing.T) {
	client := NewClient(setupTestDB(t))
	revision := testRuntimeRevision("cancelled-revision", "cancelled-actor")
	revision.BackendKind = dbpkg.RuntimeBackendKindSubstrate
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := client.UpsertRuntimeRevision(ctx, revision)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, errors.Is(err, context.Canceled))
}

func testRuntimeRevision(revision string, actorTemplate string) dbpkg.RuntimeRevision {
	return dbpkg.RuntimeRevision{
		Revision: revision, Namespace: "team-a",
		AgentTemplateName: "assistant", AgentTemplateUID: "template-uid",
		HarnessName: "kagent", HarnessUID: "harness-uid",
		SourceSnapshot: []byte("{}"), AgentCard: []byte(`{"name":"assistant"}`),
		EgressDestinations:     []string{},
		ActorTemplateNamespace: "team-a", ActorTemplateName: actorTemplate,
		ActorTemplateUID: "actor-template-uid", Phase: "Ready",
	}
}
