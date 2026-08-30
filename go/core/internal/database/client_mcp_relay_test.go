package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	apiv1alpha1 "github.com/kagent-dev/kagent/go/api/gen/kagent/api/v1alpha1"
	"github.com/kagent-dev/kagent/go/core/v2/mcprelay"
	"github.com/kagent-dev/kagent/go/core/v2/translator"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
)

func relayPolicyFixture(t *testing.T, serverName string, tools ...string) (translator.MCPPolicyV1, translator.MCPPolicyBinding) {
	t.Helper()
	tools = slices.Clone(tools)
	slices.Sort(tools)
	binding := translator.MCPPolicyBinding{
		SubjectPath: []string{"root"},
		Server: translator.MCPServerIdentity{
			Namespace: "team-a", Name: serverName, UID: serverName + "-uid",
			SpecHash: strings.Repeat("a", sha256.Size*2),
		},
		Tools: tools,
	}
	// Grant-visible binding identities deliberately exclude SpecHash: the
	// persisted policy still pins it, but including it here would expose an
	// offline oracle for private RemoteMCPServer connection material.
	type grantServerIdentity struct {
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		UID       string `json:"uid"`
	}
	raw, err := json.Marshal(struct {
		SubjectPath []string            `json:"subjectPath"`
		Server      grantServerIdentity `json:"server"`
		Tools       []string            `json:"tools"`
	}{
		SubjectPath: binding.SubjectPath,
		Server: grantServerIdentity{
			Namespace: binding.Server.Namespace,
			Name:      binding.Server.Name,
			UID:       binding.Server.UID,
		},
		Tools: binding.Tools,
	})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	binding.ID = "mcp-" + hex.EncodeToString(digest[:])
	return translator.MCPPolicyV1{
		Version:  translator.MCPPolicyVersionV1,
		Bindings: []translator.MCPPolicyBinding{binding},
	}, binding
}

func relayRevisionFixture(t *testing.T, client dbpkg.Client, revisionID string, policy translator.MCPPolicyV1) dbpkg.RuntimeRevision {
	t.Helper()
	policyJSON, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	revision := dbpkg.RuntimeRevision{
		Revision: revisionID, Namespace: "team-a",
		AgentTemplateName: "assistant", AgentTemplateUID: "assistant-uid",
		HarnessName: "kagent", HarnessUID: "kagent-uid",
		SourceSnapshot:         []byte(`{"remoteMCPServer":{"specHash":"` + strings.Repeat("a", 64) + `"}}`),
		AgentCard:              []byte(`{"name":"assistant"}`),
		MCPPolicy:              policyJSON,
		EgressDestinations:     []string{},
		ActorTemplateNamespace: "team-a", ActorTemplateName: revisionID + "-actor",
		ActorTemplateUID: revisionID + "-actor-uid", Phase: "Ready",
	}
	if err := client.UpsertRuntimeRevision(context.Background(), revision); err != nil {
		t.Fatal(err)
	}
	pair := dbpkg.AgentTemplateHarnessPair{
		Namespace: "team-a", AgentTemplateName: "assistant", AgentTemplateUID: "assistant-uid",
		HarnessName: "kagent", HarnessUID: "kagent-uid", DesiredRevision: revisionID,
	}
	if err := client.UpsertAgentTemplateHarnessPair(context.Background(), pair); err != nil {
		t.Fatal(err)
	}
	if err := client.MarkRuntimeRevisionSuccessful(context.Background(), pair); err != nil {
		t.Fatal(err)
	}
	return revision
}

func createRelayInstance(t *testing.T, client dbpkg.Client, id string) *apiv1alpha1.AgentInstance {
	t.Helper()
	instance, created, err := client.CreateAgentInstance(context.Background(), &apiv1alpha1.AgentInstance{
		Id: id, Namespace: "team-a", Creator: "alice",
		Harness:       &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "kagent"},
		AgentTemplate: &apiv1alpha1.ResourceReference{Namespace: "team-a", Name: "assistant"},
	}, id+"-request")
	if err != nil || !created {
		t.Fatalf("CreateAgentInstance() = created %v, error %v", created, err)
	}
	return instance
}

func TestCanonicalRuntimeRevisionMCPPolicy(t *testing.T) {
	empty, err := canonicalRuntimeRevisionMCPPolicy(nil)
	if err != nil || string(empty) != emptyMCPPolicyJSON {
		t.Fatalf("nil compatibility policy = %s, error %v", empty, err)
	}

	policy, _ := relayPolicyFixture(t, "knowledge", "search")
	indented, err := json.MarshalIndent(policy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := canonicalRuntimeRevisionMCPPolicy(indented)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(want) {
		t.Fatalf("canonical policy = %s, want %s", canonical, want)
	}
	if _, err := canonicalRuntimeRevisionMCPPolicy([]byte(`{"version":"v1","bindings":[],"bindings":[]}`)); err == nil {
		t.Fatal("duplicate policy field was accepted at the database boundary")
	}
}

func TestPersistMCPRelayGrantRejectsZeroDigestBeforeDatabase(t *testing.T) {
	store := &MCPRelayStore{}
	err := store.PersistMCPRelayGrant(context.Background(), mcprelay.CapabilityDigest{}, mcprelay.Grant{
		AgentInstanceID: "instance", Revision: "revision",
		BindingID: "mcp-" + strings.Repeat("a", sha256.Size*2),
		ExpiresAt: time.Unix(1, 0).UTC(),
	})
	if !errors.Is(err, ErrMCPRelayGrantDigest) {
		t.Fatalf("zero capability digest error = %v", err)
	}
}

func TestRuntimeRevisionMCPPolicyPersistenceIsStrictAndImmutable(t *testing.T) {
	db := setupTestDB(t)
	client := NewClient(db)
	store := NewMCPRelayStore(db)
	policy, binding := relayPolicyFixture(t, "knowledge", "search")
	revision := relayRevisionFixture(t, client, "relay-revision-1", policy)

	stored, err := client.GetRuntimeRevision(context.Background(), revision.Revision)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := translator.DecodeMCPPolicyV1(stored.MCPPolicy)
	if err != nil || decoded.Bindings[0].ID != binding.ID {
		t.Fatalf("stored policy = %+v, error %v", decoded, err)
	}
	loaded, err := store.MCPPolicy(context.Background(), revision.Revision)
	if err != nil || loaded.Bindings[0].ID != binding.ID {
		t.Fatalf("MCPPolicy() = %+v, error %v", loaded, err)
	}

	// Formatting does not change semantic JSONB identity or create a false
	// immutable-revision collision.
	reformatted := revision
	reformatted.MCPPolicy, err = json.MarshalIndent(policy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpsertRuntimeRevision(context.Background(), reformatted); err != nil {
		t.Fatalf("semantic policy re-upsert: %v", err)
	}

	different, _ := relayPolicyFixture(t, "different", "search")
	conflicting := revision
	conflicting.MCPPolicy, err = json.Marshal(different)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpsertRuntimeRevision(context.Background(), conflicting); !errors.Is(err, dbpkg.ErrRuntimeRevisionConflict) {
		t.Fatalf("immutable policy collision error = %v", err)
	}
	loaded, err = store.MCPPolicy(context.Background(), revision.Revision)
	if err != nil || loaded.Bindings[0].ID != binding.ID {
		t.Fatalf("policy after collision = %+v, error %v", loaded, err)
	}

	for index, raw := range []string{
		`{"version":"v1","version":"v1","bindings":[]}`,
		`{"version":"v1","bindings":[],"unknown":true}`,
		`{"version":"v1","bindings":[]} {}`,
	} {
		invalid := revision
		invalid.Revision = "invalid-policy-" + string(rune('a'+index))
		invalid.ActorTemplateName = invalid.Revision + "-actor"
		invalid.MCPPolicy = []byte(raw)
		if err := client.UpsertRuntimeRevision(context.Background(), invalid); err == nil {
			t.Fatalf("invalid policy %d was persisted", index)
		}
	}

	// A legacy writer omitting the field receives the explicit deny-all policy.
	empty := revision
	empty.Revision = "relay-revision-empty"
	empty.ActorTemplateName = empty.Revision + "-actor"
	empty.MCPPolicy = nil
	if err := client.UpsertRuntimeRevision(context.Background(), empty); err != nil {
		t.Fatal(err)
	}
	emptyPolicy, err := store.MCPPolicy(context.Background(), empty.Revision)
	if err != nil || emptyPolicy.Version != translator.MCPPolicyVersionV1 || len(emptyPolicy.Bindings) != 0 {
		t.Fatalf("empty compatibility policy = %+v, error %v", emptyPolicy, err)
	}

	var persisted string
	if err := db.QueryRow(context.Background(), `SELECT mcp_policy::text FROM runtime_revision WHERE revision = $1`, revision.Revision).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"cluster.internal", "Authorization", "Bearer secret", "https://"} {
		if strings.Contains(persisted, secret) {
			t.Fatalf("persisted private policy leaked %q: %s", secret, persisted)
		}
	}

	// Privileged database tampering is detected on the next read rather than
	// silently interpreted with permissive unknown fields.
	if _, err := db.Exec(context.Background(), `
		UPDATE runtime_revision
		SET mcp_policy = '{"version":"v1","bindings":[],"connectionURL":"https://cluster.internal"}'::jsonb
		WHERE revision = $1
	`, revision.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MCPPolicy(context.Background(), revision.Revision); err == nil {
		t.Fatal("tampered persisted policy was accepted")
	}
}

type relayPersistenceUpstream struct {
	calls int
}

func (u *relayPersistenceUpstream) ListTools(
	_ context.Context,
	_ mcprelay.UpstreamTarget,
	yield func(mcprelay.ToolPage) error,
) error {
	return yield(mcprelay.ToolPage{})
}

func (u *relayPersistenceUpstream) CallTool(
	_ context.Context,
	_ mcprelay.UpstreamTarget,
	_ string,
	_ json.RawMessage,
) (*mcp.CallToolResult, error) {
	u.calls++
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "unexpected"}}}, nil
}

func TestMCPRelayGrantPersistenceAndLifecycleAdapters(t *testing.T) {
	db := setupTestDB(t)
	client := NewClient(db)
	store := NewMCPRelayStore(db)
	policy, binding := relayPolicyFixture(t, "knowledge", "search")
	revision := relayRevisionFixture(t, client, "relay-revision-1", policy)
	instance := createRelayInstance(t, client, "relay-instance-1")

	lifecycle, err := store.MCPInstanceLifecycle(context.Background(), instance.GetId())
	if err != nil || lifecycle.PreparedRevision != revision.Revision || lifecycle.State != mcprelay.InstanceStateCreating || !lifecycle.OperationPending {
		t.Fatalf("creating lifecycle = %+v, error %v", lifecycle, err)
	}
	ready, err := client.MarkAgentInstanceReady(context.Background(), instance.GetId(), "actor.example")
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err = store.MCPInstanceLifecycle(context.Background(), instance.GetId())
	if err != nil || lifecycle.State != mcprelay.InstanceStateReady || lifecycle.OperationPending {
		t.Fatalf("ready lifecycle = %+v, error %v", lifecycle, err)
	}

	now := time.Date(2026, time.August, 26, 12, 0, 0, 0, time.UTC)
	capability := strings.Repeat("c", 43)
	digest := mcprelay.CapabilityDigest(sha256.Sum256([]byte(capability)))
	grant := mcprelay.Grant{
		AgentInstanceID: instance.GetId(), Revision: revision.Revision, BindingID: binding.ID,
		ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := store.PersistMCPRelayGrant(context.Background(), digest, grant); err != nil {
		t.Fatal(err)
	}
	verified, err := store.VerifyMCPGrant(context.Background(), digest)
	if err != nil || verified.AgentInstanceID != grant.AgentInstanceID || verified.Revision != grant.Revision ||
		verified.BindingID != grant.BindingID || !verified.ExpiresAt.Equal(grant.ExpiresAt) || verified.RevokedAt != nil {
		t.Fatalf("VerifyMCPGrant() = %+v, error %v", verified, err)
	}

	var rowJSON string
	if err := db.QueryRow(context.Background(), `
		SELECT row_to_json(g)::text FROM mcp_relay_grant g WHERE capability_hash = $1
	`, digest[:]).Scan(&rowJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rowJSON, capability) || strings.Contains(rowJSON, "Bearer") {
		t.Fatalf("grant row contains plaintext capability material: %s", rowJSON)
	}
	var unsafeColumns int
	if err := db.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'mcp_relay_grant'
		  AND column_name ~ '(token|secret|plaintext|capability)$'
	`).Scan(&unsafeColumns); err != nil {
		t.Fatal(err)
	}
	if unsafeColumns != 0 {
		t.Fatalf("grant table has %d plaintext-looking columns", unsafeColumns)
	}

	second := createRelayInstance(t, client, "relay-instance-2")
	if _, err := client.MarkAgentInstanceReady(context.Background(), second.GetId(), "actor-2.example"); err != nil {
		t.Fatal(err)
	}
	collision := grant
	collision.AgentInstanceID = second.GetId()
	if err := store.PersistMCPRelayGrant(context.Background(), digest, collision); !errors.Is(err, ErrMCPRelayGrantConflict) {
		t.Fatalf("digest collision error = %v", err)
	}
	verified, err = store.VerifyMCPGrant(context.Background(), digest)
	if err != nil || verified.AgentInstanceID != instance.GetId() {
		t.Fatalf("grant after collision = %+v, error %v", verified, err)
	}

	otherPolicy, otherBinding := relayPolicyFixture(t, "other", "read")
	otherRevision := relayRevisionFixture(t, client, "relay-revision-2", otherPolicy)
	invalidDigest := mcprelay.CapabilityDigest(sha256.Sum256([]byte(strings.Repeat("i", 43))))
	invalidScopes := []mcprelay.Grant{
		{AgentInstanceID: instance.GetId(), Revision: revision.Revision, BindingID: otherBinding.ID, ExpiresAt: now.Add(time.Minute)},
		{AgentInstanceID: instance.GetId(), Revision: otherRevision.Revision, BindingID: otherBinding.ID, ExpiresAt: now.Add(time.Minute)},
		{AgentInstanceID: instance.GetId(), Revision: revision.Revision, BindingID: binding.ID},
	}
	for index, invalid := range invalidScopes {
		candidate := invalidDigest
		candidate[0] = byte(index + 1)
		if err := store.PersistMCPRelayGrant(context.Background(), candidate, invalid); !errors.Is(err, ErrMCPRelayGrantScope) {
			t.Fatalf("invalid scope %d error = %v", index, err)
		}
	}

	upstream := &relayPersistenceUpstream{}
	engine, err := mcprelay.New(mcprelay.Config{
		Policies: store, Grants: store, Lifecycles: store, Upstream: upstream,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	expiredCapability := strings.Repeat("e", 43)
	expiredDigest := mcprelay.CapabilityDigest(sha256.Sum256([]byte(expiredCapability)))
	expired := grant
	expired.ExpiresAt = now.Add(-time.Minute)
	if err := store.PersistMCPRelayGrant(context.Background(), expiredDigest, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.CallTool(context.Background(), expiredCapability, binding.ID, "search", json.RawMessage(`{}`)); !errors.Is(err, mcprelay.ErrUnauthenticated) {
		t.Fatalf("expired grant error = %v", err)
	}
	if upstream.calls != 0 {
		t.Fatalf("expired grant reached upstream %d times", upstream.calls)
	}

	revokedAt := now.Add(time.Second)
	if err := store.RevokeMCPRelayGrant(context.Background(), digest, revokedAt); err != nil {
		t.Fatal(err)
	}
	verified, err = store.VerifyMCPGrant(context.Background(), digest)
	if err != nil || verified.RevokedAt == nil || !verified.RevokedAt.Equal(revokedAt) {
		t.Fatalf("revoked grant = %+v, error %v", verified, err)
	}
	if err := store.RevokeMCPRelayGrant(context.Background(), digest, revokedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	verified, err = store.VerifyMCPGrant(context.Background(), digest)
	if err != nil || verified.RevokedAt == nil || !verified.RevokedAt.Equal(revokedAt) {
		t.Fatalf("idempotently revoked grant = %+v, error %v", verified, err)
	}
	if _, err := engine.CallTool(context.Background(), capability, binding.ID, "search", json.RawMessage(`{}`)); !errors.Is(err, mcprelay.ErrUnauthenticated) {
		t.Fatalf("revoked grant error = %v", err)
	}
	if upstream.calls != 0 {
		t.Fatalf("revoked grant reached upstream %d times", upstream.calls)
	}

	suspending := proto.Clone(ready).(*apiv1alpha1.AgentInstance)
	suspending.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_SUSPEND
	if _, err := client.TransitionAgentInstance(context.Background(), suspending, ready.GetState(), ready.GetOperation()); err != nil {
		t.Fatal(err)
	}
	lifecycle, err = store.MCPInstanceLifecycle(context.Background(), instance.GetId())
	if err != nil || lifecycle.State != mcprelay.InstanceStateReady || !lifecycle.OperationPending {
		t.Fatalf("pending suspend lifecycle = %+v, error %v", lifecycle, err)
	}
	suspended := proto.Clone(suspending).(*apiv1alpha1.AgentInstance)
	suspended.State = apiv1alpha1.AgentInstanceState_AGENT_INSTANCE_STATE_SUSPENDED
	suspended.Operation = apiv1alpha1.AgentInstanceOperation_AGENT_INSTANCE_OPERATION_UNSPECIFIED
	if _, err := client.TransitionAgentInstance(context.Background(), suspended, ready.GetState(), suspending.GetOperation()); err != nil {
		t.Fatal(err)
	}
	lifecycle, err = store.MCPInstanceLifecycle(context.Background(), instance.GetId())
	if err != nil || lifecycle.State != mcprelay.InstanceStateSuspended || lifecycle.OperationPending {
		t.Fatalf("suspended lifecycle = %+v, error %v", lifecycle, err)
	}
}
