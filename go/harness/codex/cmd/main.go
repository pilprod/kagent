// Command kagent-codex runs the Codex Harness runtime adapter in an in-cluster
// sandbox or through an enrolled external execution provider.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	a2atype "github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/kagent-dev/kagent/go/adk/pkg/app"
	"github.com/kagent-dev/kagent/go/harness/codex/internal/adapter"
	runtimea2a "github.com/kagent-dev/kagent/go/harness/runtime/a2a"
	"github.com/kagent-dev/kagent/go/harness/runtime/managedtransport"
	"github.com/kagent-dev/kagent/go/harness/runtime/session"
)

const (
	configEnv            = "KAGENT_CONFIG_JSON"
	agentCardEnv         = "KAGENT_AGENT_CARD_JSON"
	defaultDataDir       = "/data"
	defaultClusterPort   = "80"
	defaultExternalPort  = "8080"
	readinessTokenEnv    = "YOUROWN_CHAT_READINESS_TOKEN"
	transportTokenEnv    = "YOUROWN_CHAT_TRANSPORT_TOKEN"
	readinessTokenHeader = "X-YourOwn-Chat-Readiness-Token"
)

type runtimeOptions struct {
	Check                 bool
	Mode                  adapter.Mode
	DataDir               string
	Workspace             string
	CodexHome             string
	CodexExecutable       string
	SkillRepositoryPrefix string
	PrivatePort           string
}

func main() {
	check := flag.Bool("check", false, "validate configuration and Codex version, then exit")
	mode := flag.String("mode", string(adapter.ModeInCluster), "runtime mode: in-cluster or external-host")
	dataDir := flag.String("data-dir", "", "durable runtime directory (required for external-host)")
	workspace := flag.String("workspace", "", "agent workspace (required for external-host)")
	codexHome := flag.String("codex-home", "", "isolated pre-authenticated CODEX_HOME (required for external-host)")
	codexExecutable := flag.String("codex-bin", "", "owner-selected absolute Codex executable (required for external-host)")
	skillRepositoryPrefix := flag.String("skill-repository-prefix", "", "owner-approved OCI repository prefix for external-host standalone skills")
	privatePort := flag.String("port", "", "private A2A listen port")
	flag.Parse()
	if err := run(context.Background(), runtimeOptions{
		Check: *check, Mode: adapter.Mode(*mode), DataDir: *dataDir,
		Workspace: *workspace, CodexHome: *codexHome, CodexExecutable: *codexExecutable,
		SkillRepositoryPrefix: *skillRepositoryPrefix, PrivatePort: *privatePort,
	}, os.Getenv, os.Environ()); err != nil {
		log.Fatal(err)
	}
}

func run(ctx context.Context, options runtimeOptions, getenv func(string) string, environment []string) error {
	options, err := resolveOptions(options)
	if err != nil {
		return err
	}
	configJSON, err := requiredEnvironment(getenv, configEnv)
	if err != nil {
		return err
	}
	agentCardJSON, err := requiredEnvironment(getenv, agentCardEnv)
	if err != nil {
		return err
	}
	var card a2atype.AgentCard
	if err := json.Unmarshal(agentCardJSON, &card); err != nil {
		return fmt.Errorf("decode agent card: %w", err)
	}
	if strings.TrimSpace(card.Name) == "" {
		return fmt.Errorf("agent card name is required")
	}
	configurePrivateInterface(&card, options.PrivatePort)

	validateCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	runner, err := adapter.New(validateCtx, adapter.Input{
		ConfigJSON: configJSON, Mode: options.Mode,
		Workspace: options.Workspace, DurableDir: options.DataDir,
		CodexHome: options.CodexHome, CodexExecutable: options.CodexExecutable,
		SkillRepositoryPrefix: options.SkillRepositoryPrefix, Environment: environment,
	})
	cancel()
	if err != nil {
		return fmt.Errorf("configure Codex Harness: %w", err)
	}
	if options.Check {
		return nil
	}

	store, err := session.New(filepath.Join(options.DataDir, "adapter"), "codex")
	if err != nil {
		return err
	}
	executor, err := runtimea2a.New(runner, store)
	if err != nil {
		return err
	}
	appConfig := app.AppConfig{
		AgentCard: card, Port: options.PrivatePort, AppName: card.Name,
	}
	if options.Mode == adapter.ModeExternalHost {
		transportToken, readinessToken, tokenErr := externalTokens(getenv)
		if tokenErr != nil {
			return tokenErr
		}
		listener, listenerErr := externalListener(options.PrivatePort, transportToken)
		if listenerErr != nil {
			return listenerErr
		}
		defer listener.Close()
		appConfig.Host = "127.0.0.1"
		appConfig.Listener = listener
		appConfig.ReadyzHandler = readinessHandler(readinessToken)
		appConfig.DisableSeparateReadiness = true
	}
	application, err := app.New(appConfig, executor)
	if err != nil {
		return fmt.Errorf("construct private A2A app: %w", err)
	}
	return application.Run()
}

func externalTokens(getenv func(string) string) (transportToken, readinessToken string, err error) {
	transportToken, err = requiredEnvironmentString(getenv, transportTokenEnv)
	if err != nil {
		return "", "", err
	}
	readinessToken, err = requiredEnvironmentString(getenv, readinessTokenEnv)
	if err != nil {
		return "", "", err
	}
	if err := managedtransport.ValidateToken(transportToken); err != nil {
		return "", "", fmt.Errorf("validate %s: %w", transportTokenEnv, err)
	}
	if err := managedtransport.ValidateToken(readinessToken); err != nil {
		return "", "", fmt.Errorf("validate %s: %w", readinessTokenEnv, err)
	}
	if transportToken == readinessToken {
		return "", "", fmt.Errorf("external-host transport and readiness tokens must be distinct")
	}
	return transportToken, readinessToken, nil
}

func resolveOptions(options runtimeOptions) (runtimeOptions, error) {
	switch options.Mode {
	case adapter.ModeInCluster:
		if options.CodexExecutable != "" {
			return runtimeOptions{}, fmt.Errorf("in-cluster mode does not accept --codex-bin")
		}
		if options.DataDir == "" {
			options.DataDir = defaultDataDir
		}
		if options.Workspace == "" {
			options.Workspace = filepath.Join(options.DataDir, "workspace")
		}
		if options.CodexHome == "" {
			options.CodexHome = filepath.Join(options.DataDir, "codex")
		}
		if options.PrivatePort == "" {
			options.PrivatePort = defaultClusterPort
		}
	case adapter.ModeExternalHost:
		if options.DataDir == "" || options.Workspace == "" || options.CodexHome == "" || options.CodexExecutable == "" {
			return runtimeOptions{}, fmt.Errorf("external-host mode requires --data-dir, --workspace, --codex-home, and --codex-bin")
		}
		if options.PrivatePort == "" {
			options.PrivatePort = defaultExternalPort
		}
	default:
		return runtimeOptions{}, fmt.Errorf("unsupported runtime mode %q", options.Mode)
	}
	for name, path := range map[string]string{
		"data directory": options.DataDir, "workspace": options.Workspace, "CODEX_HOME": options.CodexHome,
	} {
		if !filepath.IsAbs(path) {
			return runtimeOptions{}, fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if options.Mode == adapter.ModeExternalHost && pathWithin(options.DataDir, options.CodexHome) {
		return runtimeOptions{}, fmt.Errorf("external-host CODEX_HOME must be outside the durable data directory")
	}
	if options.Mode == adapter.ModeExternalHost && (!filepath.IsAbs(options.CodexExecutable) || filepath.Clean(options.CodexExecutable) != options.CodexExecutable) {
		return runtimeOptions{}, fmt.Errorf("external-host --codex-bin must be an absolute normalized path")
	}
	if _, err := net.LookupPort("tcp", options.PrivatePort); err != nil {
		return runtimeOptions{}, fmt.Errorf("invalid private A2A port %q: %w", options.PrivatePort, err)
	}
	return options, nil
}

func externalListener(port, token string) (net.Listener, error) {
	if err := managedtransport.ValidateToken(token); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		return nil, fmt.Errorf("listen on private external-host endpoint: %w", err)
	}
	wrapped, err := managedtransport.WrapListener(listener, token)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	return wrapped, nil
}

func readinessHandler(token string) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			response.Header().Set("Allow", http.MethodGet)
			http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		response.Header().Set(readinessTokenHeader, token)
		response.WriteHeader(http.StatusOK)
	})
}

func configurePrivateInterface(card *a2atype.AgentCard, port string) {
	for _, endpoint := range card.SupportedInterfaces {
		if endpoint != nil {
			endpoint.URL = "http://127.0.0.1:" + port
		}
	}
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func requiredEnvironment(getenv func(string) string, name string) ([]byte, error) {
	value, err := requiredEnvironmentString(getenv, name)
	if err != nil {
		return nil, err
	}
	return []byte(value), nil
}

func requiredEnvironmentString(getenv func(string) string, name string) (string, error) {
	value := strings.TrimSpace(getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}
