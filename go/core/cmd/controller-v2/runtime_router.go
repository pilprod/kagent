package main

import (
	"context"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
)

type controllerRuntimeRevisionStore interface {
	GetRuntimeRevision(context.Context, string) (*dbpkg.RuntimeRevision, error)
}

func newRuntimeBackendRouter(
	store controllerRuntimeRevisionStore,
	substrate runtimebackend.Backend,
	external *runtimebackend.Backend,
) (*runtimebackend.Router, error) {
	selector, err := runtimebackend.NewRevisionSelector(store)
	if err != nil {
		return nil, err
	}
	registrations := []runtimebackend.Registration{{Kind: runtimebackend.KindSubstrate, Backend: substrate}}
	if external != nil {
		registrations = append(registrations, runtimebackend.Registration{Kind: runtimebackend.KindExternal, Backend: *external})
	}
	return runtimebackend.NewRouter(selector, registrations...)
}
