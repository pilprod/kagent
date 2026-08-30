package config

import (
	"strings"
	"testing"
)

func TestParsePortableExternalTranslatesOnlyPortableSelection(t *testing.T) {
	raw := []byte(`{"version":"v2","runtime":"codex","root":{"templateName":"agent-one","instruction":"Stay in scope.","model":{"provider":"OpenAI","name":"gpt-5.6-sol","reasoningEffort":"ultra","serviceTier":"fast"}}}`)

	got, err := ParsePortableExternal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-5.6-sol" || got.ReasoningEffort != "ultra" || got.ServiceTier != "fast" || got.DeveloperInstructions != "Stay in scope." {
		t.Fatalf("portable selection was not translated: %#v", got)
	}
	if got.CodexExecutable != "codex" || got.ExpectedCodexVersion != PinnedCodexVersion || !got.StrictVersion ||
		got.ModelProvider != "openai" || got.Provider != nil || got.NetworkAccess != NetworkRestricted ||
		got.MaxEventBytes != defaultMaxEvent || got.MaxStderrBytes != defaultMaxStderr ||
		got.HandshakeTimeoutMS != defaultHandshakeMS || got.ShutdownGraceMS != defaultShutdownMS {
		t.Fatalf("portable input changed private adapter policy: %#v", got)
	}
}

func TestParsePortableExternalForRepositoryPrefixTranslatesStandaloneOCISkills(t *testing.T) {
	raw := []byte(`{"version":"v2","runtime":"codex","root":{"templateName":"agent-one","model":{"provider":"OpenAI","name":"gpt-5"},"skills":[{"name":"review","source":{"oci":"ghcr.io/acme/codex/skills/review@sha256:` + strings.Repeat("a", 64) + `"}}]}}`)
	got, err := ParsePortableExternalForRepositoryPrefix(raw, "ghcr.io/acme/codex/skills/")
	if err != nil {
		t.Fatal(err)
	}
	if got.SkillResources == nil || len(got.SkillResources.Skills) != 1 ||
		got.SkillResources.Skills[0].Name != "review" || got.SkillResources.Skills[0].Source.OCI == "" {
		t.Fatalf("portable skills were not translated: %#v", got.SkillResources)
	}
	if _, err := ParsePortableExternal(raw); err == nil || !strings.Contains(err.Error(), "owner-approved OCI repository prefix") {
		t.Fatalf("ParsePortableExternal() accepted an unbound skill repository prefix: %v", err)
	}
	if _, err := ParsePortableExternalForRepositoryPrefix(raw, "ghcr.io/other/codex/skills/"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("ParsePortableExternalForRepositoryPrefix() accepted a sibling repository: %v", err)
	}
}

func TestParsePortableExternalRejectsPrivateAuthorityAndUnsupportedResources(t *testing.T) {
	base := `{"version":"v2","runtime":"codex","root":{"templateName":"agent-one","model":{"provider":"OpenAI","name":"gpt-5"}}}`
	tests := map[string]string{
		"private executable": strings.Replace(base, `"root":`, `"codexExecutable":"/tmp/codex","root":`, 1),
		"numeric private v1": `{"version":1,"codex_executable":"codex"}`,
		"noncanonical":       " " + base,
		"wrong runtime":      strings.Replace(base, `"runtime":"codex"`, `"runtime":"claude"`, 1),
		"MCP grant": strings.Replace(base, `"model":{"provider":"OpenAI","name":"gpt-5"}`,
			`"model":{"provider":"OpenAI","name":"gpt-5"},"mcpGrants":[{"id":"mcp-`+strings.Repeat("a", 64)+`","tools":["lookup"]}]`, 1),
		"skill source path": strings.Replace(base, `"model":{"provider":"OpenAI","name":"gpt-5"}`,
			`"model":{"provider":"OpenAI","name":"gpt-5"},"skills":[{"name":"review","source":{"oci":"ghcr.io/acme/review@sha256:`+strings.Repeat("a", 64)+`","path":"review"}}]`, 1),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePortableExternal([]byte(raw)); err == nil {
				t.Fatal("ParsePortableExternal() accepted non-portable authority")
			}
		})
	}
}
