package translator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const shortRevisionBytes = 6

// RevisionPlacement is the internal execution-capacity boundary selected by a
// Harness compiler. It is intentionally not part of the public Harness API:
// users select a runtime, and the compiler owns its compatible placement.
type RevisionPlacement string

const (
	RevisionPlacementKubernetesPod RevisionPlacement = "KubernetesPod"
	RevisionPlacementExternalSlot  RevisionPlacement = "ExternalSlot"
)

// Validate rejects revisions that did not make an explicit placement choice.
func (p RevisionPlacement) Validate() error {
	switch p {
	case RevisionPlacementKubernetesPod, RevisionPlacementExternalSlot:
		return nil
	default:
		return fmt.Errorf("unsupported runtime revision placement %q", p)
	}
}

// SandboxClass is the execution-isolation contract compiled into an immutable
// runtime revision. It is internal rather than user-selected: each Harness
// compiler owns the class compatible with its placement.
type SandboxClass string

const (
	SandboxClassGvisor              SandboxClass = "gvisor"
	SandboxClassMicroVM             SandboxClass = "microvm"
	SandboxClassHostProcessHardened SandboxClass = "host-process-hardened"
)

// ValidateForPlacement rejects a class that cannot be served by the selected
// execution provider. The current runtime contracts are deliberately narrow;
// adding another class requires an explicit compiler and Substrate contract.
func (s SandboxClass) ValidateForPlacement(placement RevisionPlacement) error {
	switch placement {
	case RevisionPlacementKubernetesPod:
		if s == SandboxClassGvisor || s == SandboxClassMicroVM {
			return nil
		}
		return fmt.Errorf("%s runtime revision requires an in-cluster sandbox class, got %q", placement, s)
	case RevisionPlacementExternalSlot:
		if s == SandboxClassHostProcessHardened {
			return nil
		}
		return fmt.Errorf("%s runtime revision requires sandbox class %q, got %q", placement, SandboxClassHostProcessHardened, s)
	default:
		return placement.Validate()
	}
}

// RevisionID is the SHA-256 identity of a compiled runtime revision. Keeping
// the digest as a fixed-size value makes invalid lengths unrepresentable.
type RevisionID [sha256.Size]byte

// String returns the full database identity.
func (id RevisionID) String() string { return hex.EncodeToString(id[:]) }

// Short returns the readable prefix used in Kubernetes names and labels.
func (id RevisionID) Short() string { return hex.EncodeToString(id[:shortRevisionBytes]) }

// IsZero reports whether compilation has not produced an identity.
func (id RevisionID) IsZero() bool { return id == RevisionID{} }

// Revision is the resolved runtime configuration for one immutable revision.
type Revision struct {
	// These fields identify the public attachment that produced the revision.
	Namespace         string
	AgentTemplateName string
	HarnessName       string

	// Image and Environment describe the runtime container.
	Image       string
	Environment []corev1.EnvVar
	// ConfigJSON and AgentCardJSON are injected into that container verbatim.
	ConfigJSON    []byte
	AgentCardJSON []byte

	// Placement selects the internal Substrate worker provider. Harness
	// compilers set it; it is never copied from user-authored generic provider
	// configuration.
	Placement RevisionPlacement
	// SandboxClass selects the isolation contract within that provider. It is
	// compiled together with Placement and participates in revision identity.
	SandboxClass SandboxClass

	// WorkerPoolName and SnapshotLocation control Substrate placement and state.
	WorkerPoolName   string
	SnapshotLocation string

	// Provenance identifies every Kubernetes input to this revision. Secret
	// values are represented only by hashes.
	Provenance json.RawMessage
	// MCPPolicy is private control-plane authorization data. It participates in
	// the immutable revision identity but is never materialized into the Actor
	// environment, agent card, or harness runtime configuration.
	MCPPolicy MCPPolicyV1
	// EgressDestinations is the hostname allowlist required by this revision.
	EgressDestinations []string
	// Warnings are non-blocking compilation diagnostics. They are deliberately
	// excluded from Digest because they do not change runtime behavior.
	Warnings []string
}

// Digest returns the immutable identity of every input that affects runtime
// behavior. The full digest is the database key; Kubernetes names use a short
// prefix only for readability.
func (r *Revision) Digest() (RevisionID, error) {
	if r == nil {
		return RevisionID{}, fmt.Errorf("runtime revision is required")
	}
	if err := r.Placement.Validate(); err != nil {
		return RevisionID{}, err
	}
	if err := r.SandboxClass.ValidateForPlacement(r.Placement); err != nil {
		return RevisionID{}, err
	}
	raw, err := json.Marshal(struct {
		Namespace          string            `json:"namespace"`
		AgentTemplateName  string            `json:"agentTemplateName"`
		HarnessName        string            `json:"harnessName"`
		Image              string            `json:"image"`
		Environment        []corev1.EnvVar   `json:"environment"`
		ConfigJSON         json.RawMessage   `json:"config"`
		AgentCardJSON      json.RawMessage   `json:"agentCard"`
		Placement          RevisionPlacement `json:"placement"`
		SandboxClass       SandboxClass      `json:"sandboxClass"`
		WorkerPoolName     string            `json:"workerPoolName"`
		SnapshotLocation   string            `json:"snapshotLocation"`
		Provenance         json.RawMessage   `json:"provenance"`
		MCPPolicy          MCPPolicyV1       `json:"mcpPolicy"`
		EgressDestinations []string          `json:"egressDestinations"`
	}{
		Namespace: r.Namespace, AgentTemplateName: r.AgentTemplateName, HarnessName: r.HarnessName,
		Image: r.Image, Environment: r.Environment, ConfigJSON: r.ConfigJSON, AgentCardJSON: r.AgentCardJSON,
		Placement:      r.Placement,
		SandboxClass:   r.SandboxClass,
		WorkerPoolName: r.WorkerPoolName, SnapshotLocation: r.SnapshotLocation, Provenance: r.Provenance,
		MCPPolicy:          r.MCPPolicy,
		EgressDestinations: r.EgressDestinations,
	})
	if err != nil {
		return RevisionID{}, fmt.Errorf("marshal runtime revision inputs: %w", err)
	}
	return RevisionID(sha256.Sum256(raw)), nil
}
