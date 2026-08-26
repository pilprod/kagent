package externalprofile_test

import (
	"encoding/json"
	"testing"

	"github.com/kagent-dev/kagent/go/core/v2/externalprofile"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRevision = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestDecodeAndBuildMetadata(t *testing.T) {
	profile, err := externalprofile.Decode(json.RawMessage(`{
		"version":"v1","instruction":"review carefully",
		"tools":[{"server":"files","allow":["read","write"]},{"server":"git","allow":["status"]}]
	}`))
	require.NoError(t, err)
	envelope, err := externalprofile.NewEnvelope(testRevision, profile)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{
		"version":     "v1",
		"revision":    testRevision,
		"instruction": "review carefully",
		"tools": []any{
			map[string]any{"server": "files", "allow": []any{"read", "write"}},
			map[string]any{"server": "git", "allow": []any{"status"}},
		},
	}, envelope.Metadata())
}

func TestDecodeRejectsMalformedOrNonCanonicalProfiles(t *testing.T) {
	tests := map[string]string{
		"malformed":           `{"version":`,
		"trailing":            `{"version":"v1","instruction":"","tools":[]} {}`,
		"unknown field":       `{"version":"v1","instruction":"","tools":[],"endpoint":"https://private.invalid"}`,
		"missing version":     `{"instruction":"","tools":[]}`,
		"wrong version":       `{"version":"v2","instruction":"","tools":[]}`,
		"missing instruction": `{"version":"v1","tools":[]}`,
		"null tools":          `{"version":"v1","instruction":"","tools":null}`,
		"empty server":        `{"version":"v1","instruction":"","tools":[{"server":"","allow":["read"]}]}`,
		"empty allow":         `{"version":"v1","instruction":"","tools":[{"server":"files","allow":[]}]}`,
		"unsorted servers":    `{"version":"v1","instruction":"","tools":[{"server":"git","allow":["status"]},{"server":"files","allow":["read"]}]}`,
		"duplicate servers":   `{"version":"v1","instruction":"","tools":[{"server":"files","allow":["read"]},{"server":"files","allow":["write"]}]}`,
		"unsorted allow":      `{"version":"v1","instruction":"","tools":[{"server":"files","allow":["write","read"]}]}`,
		"duplicate allow":     `{"version":"v1","instruction":"","tools":[{"server":"files","allow":["read","read"]}]}`,
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := externalprofile.Decode(json.RawMessage(raw))
			require.Error(t, err)
		})
	}
}

func TestNewEnvelopeRequiresLowercaseSHA256(t *testing.T) {
	for _, revision := range []string{"", "revision-one", testRevision[:63], "ABCDEF" + testRevision[6:]} {
		_, err := externalprofile.NewEnvelope(revision, externalprofile.Profile{})
		require.Error(t, err)
	}
}
