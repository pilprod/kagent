/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/grpcserver"
	authimpl "github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	sessionservice "github.com/kagent-dev/kagent/go/core/internal/service/session"
	taskservice "github.com/kagent-dev/kagent/go/core/internal/service/task"
	"github.com/kagent-dev/kagent/go/core/pkg/migrations"
	legacysubstrate "github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate"
	"github.com/kagent-dev/kagent/go/core/v2/a2agateway"
	"github.com/kagent-dev/kagent/go/core/v2/agentinstance"
	"github.com/kagent-dev/kagent/go/core/v2/checkpoint"
	v2controller "github.com/kagent-dev/kagent/go/core/v2/controller"
	"github.com/kagent-dev/kagent/go/core/v2/externalgateway"
	"github.com/kagent-dev/kagent/go/core/v2/externalruntime"
	"github.com/kagent-dev/kagent/go/core/v2/runtimebackend"
	v2substrate "github.com/kagent-dev/kagent/go/core/v2/substrate"
	"golang.org/x/sync/errgroup"
	"k8s.io/client-go/tools/clientcmd"
	ctrl "sigs.k8s.io/controller-runtime"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	externalConfig, err := loadExternalGatewayConfig(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}
	var externalBroker *externalgateway.Broker
	var externalPlacement *externalruntime.StaticPlacement
	var externalConnector *externalruntime.Connector
	if externalConfig.Enabled {
		deviceAuthenticator, err := newDeviceTokenAuthenticatorFromFile(
			externalConfig.TokenFile,
			externalConfig.DeviceID,
			externalConfig.slots(),
		)
		if err != nil {
			log.Fatal(err)
		}
		externalBroker, err = externalgateway.NewBroker(externalConfig.Broker, deviceAuthenticator)
		if err != nil {
			log.Fatalf("configure external gateway broker: %v", err)
		}
		externalPlacement, err = externalruntime.NewStaticPlacement(externalConfig.placement())
		if err != nil {
			log.Fatalf("configure external runtime placement: %v", err)
		}
		externalConnector, err = externalruntime.NewConnector(externalBroker)
		if err != nil {
			log.Fatal(err)
		}
	}

	dbURL, err := database.ResolveURL(env("POSTGRES_DATABASE_URL", "postgres://postgres:kagent@kagent-postgresql.kagent.svc.cluster.local:5432/postgres"), os.Getenv("POSTGRES_DATABASE_URL_FILE"))
	if err != nil {
		log.Fatal(err)
	}
	if err := migrations.RunUp(ctx, dbURL, migrations.BuiltinSources(false)); err != nil {
		log.Fatalf("run database migrations: %v", err)
	}
	db, err := database.Connect(ctx, &database.PostgresConfig{URL: dbURL})
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	store := database.NewClient(db)

	kubeConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{},
	).ClientConfig()
	if err != nil {
		log.Fatalf("load Kubernetes config: %v", err)
	}
	manager, err := ctrl.NewManager(kubeConfig, ctrl.Options{
		Metrics:                 metricsserver.Options{BindAddress: "0"},
		LeaderElection:          envBool("LEADER_ELECT"),
		LeaderElectionID:        "0e9f6799.kagent.dev",
		LeaderElectionNamespace: env("KAGENT_NAMESPACE", "kagent"),
	})
	if err != nil {
		log.Fatalf("create controller manager: %v", err)
	}
	runtime, err := v2controller.NewRuntime(kubeConfig, namespaces(os.Getenv("WATCH_NAMESPACES")), ctx.Done())
	if err != nil {
		log.Fatal(err)
	}
	reconciler, err := v2controller.NewReconciler(kubeConfig, runtime.Collections, store)
	if err != nil {
		log.Fatal(err)
	}
	if err := manager.Add(reconciler); err != nil {
		log.Fatalf("add reconciler to controller manager: %v", err)
	}

	actors, err := legacysubstrate.Dial(ctx, legacysubstrate.Config{
		AteAPIEndpoint: env("SUBSTRATE_ATE_API_ENDPOINT", "dns:///api.ate-system.svc:443"),
		CAFile:         os.Getenv("SUBSTRATE_ATE_API_CA_FILE"),
		ClientCertFile: os.Getenv("SUBSTRATE_ATE_API_CLIENT_CERT_FILE"),
		CallTimeout:    30 * time.Second,
	})
	if err != nil {
		log.Fatal(err)
	}
	defer actors.Close()

	authenticator := &authimpl.UnsecureAuthenticator{}
	authorizer := &authimpl.NoopAuthorizer{}
	substrateLifecycle := v2substrate.NewLifecycle(store, actors)
	substrateConnector, err := v2substrate.NewConnector(
		env("SUBSTRATE_ATENET_ROUTER_URL", legacysubstrate.DefaultAtenetRouterURL),
		authenticator,
	)
	if err != nil {
		log.Fatal(err)
	}
	substrateBackend := runtimebackend.Backend{
		Lifecycle: substrateLifecycle,
		Connector: substrateConnector,
	}
	var externalBackend *runtimebackend.Backend
	if externalConfig.Enabled {
		externalLifecycle, err := externalruntime.NewLifecycle(store, externalPlacement, externalBroker, externalConfig.ProbeTimeout)
		if err != nil {
			log.Fatal(err)
		}
		externalBackend = &runtimebackend.Backend{Lifecycle: externalLifecycle, Connector: externalConnector}
	}
	runtimeRouter, err := newRuntimeBackendRouter(store, substrateBackend, externalBackend)
	if err != nil {
		log.Fatal(err)
	}
	instanceWorkflow := agentinstance.NewRuntimeWorkflow(store, runtimeRouter)
	instances := agentinstance.NewService(store, authorizer, instanceWorkflow)
	checkpoints := checkpoint.NewService(store, authorizer, actors, instanceWorkflow)
	server, err := grpcserver.New(grpcserver.Config{
		BindAddress:          env("GRPC_BIND_ADDRESS", ":8084"),
		Reflection:           envBool("GRPC_REFLECTION"),
		Authenticator:        authenticator,
		ShareStore:           store,
		SessionService:       sessionservice.NewService(store),
		TaskService:          taskservice.NewService(store),
		AgentInstanceService: instances,
		CheckpointService:    checkpoints,
		A2AHandler: a2agateway.New(store, authorizer, runtimeRouter, instanceWorkflow,
			env("A2A_GATEWAY_URL", "http://127.0.0.1:8084")),
	})
	if err != nil {
		log.Fatal(err)
	}

	health := &http.Server{Addr: ":8083", Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})}
	var deviceServer *http.Server
	var deviceListener net.Listener
	if externalConfig.Enabled {
		deviceServer, err = newExternalGatewayHTTPServer(externalConfig.BindAddress, externalBroker)
		if err != nil {
			log.Fatal(err)
		}
		deviceListener, err = net.Listen("tcp", deviceServer.Addr)
		if err != nil {
			log.Fatalf("listen for external gateway devices: %v", err)
		}
		defer deviceListener.Close()
	}

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return runtime.Start(ctx) })
	group.Go(func() error { return manager.Start(ctx) })
	group.Go(func() error { return server.Start(ctx) })
	group.Go(func() error {
		go func() {
			<-ctx.Done()
			_ = health.Shutdown(context.Background())
		}()
		if err := health.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("serve health endpoint: %w", err)
		}
		return nil
	})
	if externalConfig.Enabled {
		group.Go(func() error {
			if err := serveHTTP(ctx, deviceServer, deviceListener); err != nil {
				return fmt.Errorf("serve external gateway devices: %w", err)
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		log.Fatal(err)
	}
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string) bool {
	value, _ := strconv.ParseBool(os.Getenv(name))
	return value
}

func namespaces(value string) []string {
	var result []string
	for namespace := range strings.SplitSeq(value, ",") {
		if namespace = strings.TrimSpace(namespace); namespace != "" {
			result = append(result, namespace)
		}
	}
	return result
}
