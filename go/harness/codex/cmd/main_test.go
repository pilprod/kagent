package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/harness/codex/internal/adapter"
)

func TestRequiredEnvironment(t *testing.T) {
	values := map[string]string{configEnv: "  {\"version\":1}  "}
	got, err := requiredEnvironment(func(name string) string { return values[name] }, configEnv)
	if err != nil || string(got) != `{"version":1}` {
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
	wantCluster := runtimeOptions{
		Mode: adapter.ModeInCluster, DataDir: "/data", Workspace: "/data/workspace",
		CodexHome: "/data/codex", PrivatePort: "80",
	}
	if !reflect.DeepEqual(cluster, wantCluster) {
		t.Fatalf("cluster options = %#v, want %#v", cluster, wantCluster)
	}
	external, err := resolveOptions(runtimeOptions{
		Mode: adapter.ModeExternalHost, DataDir: "/runtime/codex", Workspace: "/src/project", CodexHome: "/auth/codex",
		CodexExecutable: "/opt/yourown-chat/codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	if external.PrivatePort != "8080" {
		t.Fatalf("external port = %q", external.PrivatePort)
	}
	for name, options := range map[string]runtimeOptions{
		"missing paths": {Mode: adapter.ModeExternalHost},
		"auth below snapshot": {
			Mode: adapter.ModeExternalHost, DataDir: "/runtime", Workspace: "/src", CodexHome: "/runtime/auth", CodexExecutable: "/opt/codex",
		},
		"relative path": {Mode: adapter.ModeExternalHost, DataDir: "runtime", Workspace: "/src", CodexHome: "/auth", CodexExecutable: "/opt/codex"},
		"relative executable": {
			Mode: adapter.ModeExternalHost, DataDir: "/runtime", Workspace: "/src", CodexHome: "/auth", CodexExecutable: "codex",
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

func TestReadinessHandlerReturnsExactLaunchToken(t *testing.T) {
	token := strings.Repeat("a", 64)
	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	readinessHandler(token).ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get(readinessTokenHeader) != token {
		t.Fatalf("readiness response = %d, %q", response.Code, response.Header().Get(readinessTokenHeader))
	}

	request = httptest.NewRequest(http.MethodPost, "/readyz", nil)
	response = httptest.NewRecorder()
	readinessHandler(token).ServeHTTP(response, request)
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get(readinessTokenHeader) != "" {
		t.Fatalf("POST readiness response = %d, %q", response.Code, response.Header().Get(readinessTokenHeader))
	}
}

func TestExternalTokensAreStrictAndDomainSeparated(t *testing.T) {
	transport := strings.Repeat("a", 64)
	readiness := strings.Repeat("b", 64)
	values := map[string]string{
		transportTokenEnv: transport,
		readinessTokenEnv: readiness,
	}
	gotTransport, gotReadiness, err := externalTokens(func(name string) string { return values[name] })
	if err != nil || gotTransport != transport || gotReadiness != readiness {
		t.Fatalf("externalTokens() = %q, %q, %v", gotTransport, gotReadiness, err)
	}

	for name, mutate := range map[string]func(map[string]string){
		"missing transport": func(values map[string]string) { delete(values, transportTokenEnv) },
		"invalid readiness": func(values map[string]string) { values[readinessTokenEnv] = "not-a-token" },
		"same token":        func(values map[string]string) { values[readinessTokenEnv] = values[transportTokenEnv] },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := map[string]string{transportTokenEnv: transport, readinessTokenEnv: readiness}
			mutate(candidate)
			if _, _, err := externalTokens(func(key string) string { return candidate[key] }); err == nil {
				t.Fatal("externalTokens() accepted a non-separated token contract")
			}
		})
	}
}

func TestConfigurePrivateInterface(t *testing.T) {
	card := &a2atype.AgentCard{SupportedInterfaces: []*a2atype.AgentInterface{{URL: "http://127.0.0.1:80"}, nil}}
	configurePrivateInterface(card, "8080")
	if card.SupportedInterfaces[0].URL != "http://127.0.0.1:8080" {
		t.Fatalf("private interface = %q", card.SupportedInterfaces[0].URL)
	}
}
