package database_test

import (
	"testing"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRevisionValidateBackendIdentity(t *testing.T) {
	tests := []struct {
		name    string
		kind    dbpkg.RuntimeBackendKind
		runtime dbpkg.ExternalRuntime
		wantErr string
	}{
		{name: "substrate", kind: dbpkg.RuntimeBackendKindSubstrate},
		{name: "external codex", kind: dbpkg.RuntimeBackendKindExternal, runtime: dbpkg.ExternalRuntimeCodex},
		{name: "external claude", kind: dbpkg.RuntimeBackendKindExternal, runtime: dbpkg.ExternalRuntimeClaude},
		{name: "missing kind", wantErr: "backend kind is invalid"},
		{name: "unknown kind", kind: dbpkg.RuntimeBackendKind("credential-shaped-unknown"), wantErr: "backend kind is invalid"},
		{name: "substrate with runtime", kind: dbpkg.RuntimeBackendKindSubstrate, runtime: dbpkg.ExternalRuntimeCodex, wantErr: "must not select"},
		{name: "external missing runtime", kind: dbpkg.RuntimeBackendKindExternal, wantErr: "supported runtime"},
		{name: "external unknown runtime", kind: dbpkg.RuntimeBackendKindExternal, runtime: dbpkg.ExternalRuntime("credential-shaped-unknown"), wantErr: "supported runtime"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			revision := dbpkg.RuntimeRevision{BackendKind: test.kind, ExternalRuntime: test.runtime}
			err := revision.ValidateBackendIdentity()
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantErr)
			require.NotContains(t, err.Error(), "credential-shaped-unknown")
		})
	}
}
