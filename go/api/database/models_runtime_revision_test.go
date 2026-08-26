package database_test

import (
	"testing"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRevisionValidateBackendIdentity(t *testing.T) {
	substrate := dbpkg.RuntimeRevision{
		BackendKind: dbpkg.RuntimeBackendKindSubstrate, ActorTemplateNamespace: "team-a", ActorTemplateName: "actor",
	}
	external := dbpkg.RuntimeRevision{
		BackendKind: dbpkg.RuntimeBackendKindExternal, ExternalRuntime: dbpkg.ExternalRuntimeCodex,
		ExternalProfile: []byte(`{"version":"v1"}`), Phase: "Ready",
	}
	tests := []struct {
		name     string
		revision dbpkg.RuntimeRevision
		wantErr  string
	}{
		{name: "substrate", revision: substrate},
		{name: "external codex", revision: external},
		{name: "external claude", revision: func() dbpkg.RuntimeRevision {
			value := external
			value.ExternalRuntime = dbpkg.ExternalRuntimeClaude
			return value
		}()},
		{name: "missing kind", revision: dbpkg.RuntimeRevision{}, wantErr: "backend kind is invalid"},
		{name: "unknown kind", revision: dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKind("credential-shaped-unknown")}, wantErr: "backend kind is invalid"},
		{name: "substrate missing actor", revision: dbpkg.RuntimeRevision{BackendKind: dbpkg.RuntimeBackendKindSubstrate}, wantErr: "actor template identity"},
		{name: "substrate with runtime", revision: func() dbpkg.RuntimeRevision {
			value := substrate
			value.ExternalRuntime = dbpkg.ExternalRuntimeCodex
			return value
		}(), wantErr: "must not select"},
		{name: "substrate with profile", revision: func() dbpkg.RuntimeRevision { value := substrate; value.ExternalProfile = []byte(`{}`); return value }(), wantErr: "must not select"},
		{name: "external missing runtime", revision: func() dbpkg.RuntimeRevision { value := external; value.ExternalRuntime = ""; return value }(), wantErr: "supported runtime"},
		{name: "external unknown runtime", revision: func() dbpkg.RuntimeRevision {
			value := external
			value.ExternalRuntime = dbpkg.ExternalRuntime("credential-shaped-unknown")
			return value
		}(), wantErr: "supported runtime"},
		{name: "external missing profile", revision: func() dbpkg.RuntimeRevision { value := external; value.ExternalProfile = nil; return value }(), wantErr: "JSON object profile"},
		{name: "external array profile", revision: func() dbpkg.RuntimeRevision { value := external; value.ExternalProfile = []byte(`[]`); return value }(), wantErr: "JSON object profile"},
		{name: "external with actor", revision: func() dbpkg.RuntimeRevision { value := external; value.ActorTemplateName = "actor"; return value }(), wantErr: "must not select an actor"},
		{name: "external not ready", revision: func() dbpkg.RuntimeRevision { value := external; value.Phase = "Pending"; return value }(), wantErr: "must be ready"},
		{name: "external with snapshot", revision: func() dbpkg.RuntimeRevision { value := external; value.GoldenSnapshot = "snapshot"; return value }(), wantErr: "without a golden snapshot"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.revision.ValidateBackendIdentity()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
			require.NotContains(t, err.Error(), "credential-shaped-unknown")
		})
	}
}
