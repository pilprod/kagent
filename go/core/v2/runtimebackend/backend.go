// Package runtimebackend defines the private boundary between AgentInstance
// orchestration, the public A2A gateway, and the runtime implementation.
package runtimebackend

import (
	"context"

	a2aclient "github.com/a2aproject/a2a-go/v2/a2aclient"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
)

// Endpoint is the private routing information published after a runtime has
// been created. It deliberately contains no public or user-selectable address.
type Endpoint struct {
	A2AAuthority string
}

// Lifecycle owns the runtime-specific side of an AgentInstance lifecycle.
// Every method must converge safely when the same AgentInstance operation is
// repeated or joined concurrently after an ambiguous caller outcome.
type Lifecycle interface {
	Create(context.Context, *apiv1alpha1.AgentInstance) (Endpoint, error)
	Suspend(context.Context, *apiv1alpha1.AgentInstance) error
	Resume(context.Context, *apiv1alpha1.AgentInstance) error
	Delete(context.Context, *apiv1alpha1.AgentInstance) error
}

// Connector creates a private A2A client for an AgentInstance. The gateway
// owns public routing and authorization; implementations own runtime transport
// and private authentication.
type Connector interface {
	Dial(context.Context, *apiv1alpha1.AgentInstance) (*a2aclient.Client, error)
}
