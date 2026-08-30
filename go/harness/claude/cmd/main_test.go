package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/kagent-dev/kagent/go/harness/claude/internal/adapter"
)

func TestRequiredEnvironment(t *testing.T) {
	values := map[string]string{configEnv: "  {\"version\":2}  "}
	got, err := requiredEnvironment(func(name string) string { return values[name] }, configEnv)
	if err != nil || string(got) != `{"version":2}` {
		t.Fatalf("requiredEnvironment() = %q, %v", got, err)
	}
	_, err = requiredEnvironment(func(string) string { return " " }, agentCardEnv)
	if err == nil || !strings.Contains(err.Error(), agentCardEnv) {
		t.Fatalf("requiredEnvironment() error = %v", err)
	}
}

func TestResolveOptions(t *testing.T) {
	cluster, err := resolveOptions(runtimeOptions{Mode: adapter.ModeInCluster})
	if err != nil {
		t.Fatal(err)
	}
	wantCluster := runtimeOptions{Mode: adapter.ModeInCluster, DataDir: "/data", Workspace: "/data/workspace", PrivatePort: "80"}
	if !reflect.DeepEqual(cluster, wantCluster) {
		t.Fatalf("cluster options = %#v, want %#v", cluster, wantCluster)
	}
	external, err := resolveOptions(runtimeOptions{
		Mode: adapter.ModeExternalHost, DataDir: "/runtime/claude", Workspace: "/src/project",
		ClaudeConfigDir: "/auth/claude", ClaudeExecutable: "/opt/yourown-chat/claude", SkillRepositoryPrefix: "ghcr.io/acme/claude/skills/",
	})
	if err != nil || external.PrivatePort != "8080" {
		t.Fatalf("external options = %#v, %v", external, err)
	}
	for name, options := range map[string]runtimeOptions{
		"missing paths": {Mode: adapter.ModeExternalHost},
		"auth below snapshot": {
			Mode: adapter.ModeExternalHost, DataDir: "/runtime", Workspace: "/src",
			ClaudeConfigDir: "/runtime/auth", ClaudeExecutable: "/opt/claude",
		},
		"relative executable": {
			Mode: adapter.ModeExternalHost, DataDir: "/runtime", Workspace: "/src",
			ClaudeConfigDir: "/auth", ClaudeExecutable: "claude",
		},
		"unknown mode": {Mode: adapter.Mode("remote")},
		"invalid port": {Mode: adapter.ModeInCluster, PrivatePort: "70000"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := resolveOptions(options); err == nil {
				t.Fatalf("resolveOptions() accepted %#v", options)
			}
		})
	}
}

func TestExternalTokensAreStrictAndDomainSeparated(t *testing.T) {
	transport := strings.Repeat("a", 64)
	readiness := strings.Repeat("b", 64)
	values := map[string]string{transportTokenEnv: transport, readinessTokenEnv: readiness}
	gotTransport, gotReadiness, err := externalTokens(func(name string) string { return values[name] })
	if err != nil || gotTransport != transport || gotReadiness != readiness {
		t.Fatalf("externalTokens() = %q, %q, %v", gotTransport, gotReadiness, err)
	}
	values[readinessTokenEnv] = transport
	if _, _, err := externalTokens(func(name string) string { return values[name] }); err == nil {
		t.Fatal("externalTokens() accepted equal readiness and transport tokens")
	}
}

func TestReadinessHandlerReturnsExactLaunchToken(t *testing.T) {
	token := strings.Repeat("a", 64)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	readinessHandler(token).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get(readinessTokenHeader) != token {
		t.Fatalf("readiness response = %d, %q", response.Code, response.Header().Get(readinessTokenHeader))
	}
}
