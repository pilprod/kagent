// Package externalgateway brokers online-only HTTP requests to authenticated
// external agent runtimes over reverse HTTPS long polling.
//
// Broker state is deliberately in memory. A session is owned by exactly one
// broker replica and is lost when that replica stops. Deployments with multiple
// replicas therefore need session affinity, and callers must treat a replica
// loss after dispatch as an unknown outcome. Durable task and retry semantics
// remain the responsibility of the kagent A2A layer; this package does not
// persist, replay, or retry dispatched requests.
package externalgateway
