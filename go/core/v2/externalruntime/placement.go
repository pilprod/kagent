package externalruntime

import (
	"fmt"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
)

type placement interface {
	Select(dbpkg.ExternalRuntime) (externalgateway.SlotKey, error)
}

// StaticPlacement is an immutable runtime-to-slot mapping for the online MVP.
type StaticPlacement struct {
	slots map[dbpkg.ExternalRuntime]externalgateway.SlotKey
}

var _ placement = (*StaticPlacement)(nil)

// NewStaticPlacement validates and copies an explicit runtime-to-slot mapping.
func NewStaticPlacement(slots map[dbpkg.ExternalRuntime]externalgateway.SlotKey) (*StaticPlacement, error) {
	if len(slots) == 0 {
		return nil, fmt.Errorf("external runtime placement requires at least one slot")
	}
	copied := make(map[dbpkg.ExternalRuntime]externalgateway.SlotKey, len(slots))
	for runtime, slot := range slots {
		expected, err := gatewayRuntime(runtime)
		if err != nil {
			return nil, err
		}
		if slot.Runtime != expected {
			return nil, fmt.Errorf("external runtime placement slot does not match its runtime")
		}
		if _, err := EncodeAuthority(slot); err != nil {
			return nil, fmt.Errorf("external runtime placement contains an invalid slot: %w", err)
		}
		copied[runtime] = slot
	}
	return &StaticPlacement{slots: copied}, nil
}

// Select returns the one explicitly configured slot for a runtime.
func (p *StaticPlacement) Select(runtime dbpkg.ExternalRuntime) (externalgateway.SlotKey, error) {
	slot, exists := p.slots[runtime]
	if !exists {
		return externalgateway.SlotKey{}, fmt.Errorf("external runtime has no configured placement")
	}
	return slot, nil
}

func gatewayRuntime(runtime dbpkg.ExternalRuntime) (externalgateway.Runtime, error) {
	switch runtime {
	case dbpkg.ExternalRuntimeCodex:
		return externalgateway.RuntimeCodex, nil
	case dbpkg.ExternalRuntimeClaude:
		return externalgateway.RuntimeClaude, nil
	default:
		return "", fmt.Errorf("runtime revision selects an unsupported external runtime")
	}
}
