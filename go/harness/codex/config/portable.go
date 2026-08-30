package config

import (
	"fmt"

	"github.com/kagent-dev/kagent/go/core/v2/translator/codingagent"
)

// ParsePortableExternal validates the canonical, credential-free v2 revision
// contract and translates its portable Codex selection into this adapter's
// private runtime configuration. Executable paths, CODEX_HOME, credentials,
// command arguments, network policy, protocol limits, and the pinned Codex
// version are deliberately absent from the revision and remain host-owned.
func ParsePortableExternal(contents []byte) (Config, error) {
	return ParsePortableExternalForRepositoryPrefix(contents, "")
}

// ParsePortableExternalForRepositoryPrefix additionally permits standalone OCI
// skills below the exact repository prefix authorized by local owner policy.
// The prefix is a host input and is never read from compiler-owned config.
func ParsePortableExternalForRepositoryPrefix(contents []byte, allowedSkillRepositoryPrefix string) (Config, error) {
	portable, err := codingagent.Decode(contents)
	if err != nil {
		return Config{}, fmt.Errorf("decode portable external-host config: %w", err)
	}
	if portable.Runtime != codingagent.RuntimeCodex {
		return Config{}, fmt.Errorf("portable external-host config selects runtime %q, want %q", portable.Runtime, codingagent.RuntimeCodex)
	}
	resources, err := codingagent.ExternalHostSkillResources(portable.Root, allowedSkillRepositoryPrefix)
	if err != nil {
		return Config{}, fmt.Errorf("validate portable Codex resources: %w", err)
	}

	// Preauthenticated supplies adapter-owned executable/version/protocol and
	// sandbox defaults. Only ModelConfig-owned request selection and the
	// AgentTemplate instruction cross the control-plane boundary.
	translated := Preauthenticated(
		portable.Root.Model.Name,
		portable.Root.Model.ReasoningEffort,
		portable.Root.Instruction,
	)
	translated.ServiceTier = portable.Root.Model.ServiceTier
	translated.SkillResources = resources
	if err := translated.Validate(); err != nil {
		return Config{}, fmt.Errorf("translate portable external-host config: %w", err)
	}
	return translated, nil
}
