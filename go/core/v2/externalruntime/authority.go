package externalruntime

import (
	"errors"
	"fmt"
	"strings"

	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
)

const (
	authorityPartCount = 3
	maxAuthorityLength = 3*63 + 2
)

// ErrInvalidAuthority means persisted external runtime routing information is
// malformed or contains anything other than a supported slot identity.
var ErrInvalidAuthority = errors.New("invalid external runtime authority")

// EncodeAuthority encodes a slot as three lowercase DNS labels without a
// scheme, host, path, user information, or credential.
func EncodeAuthority(slot externalgateway.SlotKey) (string, error) {
	if err := validateSlot(slot); err != nil {
		return "", err
	}
	return strings.Join([]string{slot.DeviceID, slot.SlotID, string(slot.Runtime)}, "."), nil
}

// DecodeAuthority decodes the exact device.slot.runtime private authority
// format.
func DecodeAuthority(authority string) (externalgateway.SlotKey, error) {
	if len(authority) > maxAuthorityLength {
		return externalgateway.SlotKey{}, fmt.Errorf("external runtime authority exceeds the DNS label limit: %w", ErrInvalidAuthority)
	}
	parts := strings.Split(authority, ".")
	if len(parts) != authorityPartCount {
		return externalgateway.SlotKey{}, fmt.Errorf("external runtime authority must contain exactly three DNS labels: %w", ErrInvalidAuthority)
	}
	slot := externalgateway.SlotKey{
		DeviceID: parts[0],
		SlotID:   parts[1],
		Runtime:  externalgateway.Runtime(parts[2]),
	}
	if err := validateSlot(slot); err != nil {
		return externalgateway.SlotKey{}, err
	}
	return slot, nil
}

func validateSlot(slot externalgateway.SlotKey) error {
	if !validDNSLabel(slot.DeviceID) {
		return fmt.Errorf("external runtime device ID must be a lowercase DNS label: %w", ErrInvalidAuthority)
	}
	if !validDNSLabel(slot.SlotID) {
		return fmt.Errorf("external runtime slot ID must be a lowercase DNS label: %w", ErrInvalidAuthority)
	}
	if slot.Runtime != externalgateway.RuntimeCodex && slot.Runtime != externalgateway.RuntimeClaude {
		return fmt.Errorf("external runtime ID is not supported: %w", ErrInvalidAuthority)
	}
	return nil
}

func validDNSLabel(value string) bool {
	if len(value) == 0 || len(value) > 63 || !lowerAlphaNumeric(value[0]) || !lowerAlphaNumeric(value[len(value)-1]) {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if lowerAlphaNumeric(character) || character == '-' {
			continue
		}
		return false
	}
	return true
}

func lowerAlphaNumeric(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= '0' && value <= '9')
}
