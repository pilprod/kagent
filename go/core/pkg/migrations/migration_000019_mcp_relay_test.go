package migrations

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestMigration000019MCPRelayPersistenceBackfillAndRollback(t *testing.T) {
	connStr := startTestDB(t)
	migrateCoreTo(t, connStr, 18)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	exec := func(query string, args ...any) error {
		t.Helper()
		_, err := db.ExecContext(ctx, query, args...)
		return err
	}

	if err := exec(`
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card, egress_destinations,
			actor_template_namespace, actor_template_name, actor_template_uid, phase, golden_snapshot
		) VALUES (
			'revision-before-relay', 'team-a', 'assistant', 'template-uid',
			'kagent', 'harness-uid', '{}', '{}', '{}',
			'team-a', 'assistant-revision', 'actor-template-uid', 'Ready', ''
		)
	`); err != nil {
		t.Fatal(err)
	}

	migrateCoreTo(t, connStr, 19)
	for _, revision := range []string{"revision-before-relay", "revision-old-writer"} {
		if revision == "revision-old-writer" {
			// This intentionally omits mcp_policy exactly as the previous binary
			// does while the schema is already at version 19.
			if err := exec(`
				INSERT INTO runtime_revision (
					revision, namespace, agent_template_name, agent_template_uid,
					harness_name, harness_uid, source_snapshot, agent_card, egress_destinations,
					actor_template_namespace, actor_template_name, actor_template_uid, phase, golden_snapshot
				) VALUES (
					$1, 'team-a', 'assistant', 'template-uid',
					'kagent', 'harness-uid', '{}', '{}', '{}',
					'team-a', $1 || '-actor', 'actor-template-uid', 'Ready', ''
				)
			`, revision); err != nil {
				t.Fatal(err)
			}
		}
		var version string
		var bindingCount int
		if err := db.QueryRowContext(ctx, `
			SELECT mcp_policy ->> 'version', jsonb_array_length(mcp_policy -> 'bindings')
			FROM runtime_revision WHERE revision = $1
		`, revision).Scan(&version, &bindingCount); err != nil {
			t.Fatal(err)
		}
		if version != "v1" || bindingCount != 0 {
			t.Fatalf("revision %s policy = version %q, bindings %d", revision, version, bindingCount)
		}
	}

	instanceID := "018f47a2-4efb-7c21-a848-123456789abc"
	requestID := "018f47a2-4efb-7c21-a848-123456789abd"
	if err := exec(`INSERT INTO a2a_context (id, namespace, user_id) VALUES ($1, 'team-a', 'alice')`, instanceID); err != nil {
		t.Fatal(err)
	}
	if err := exec(`
		INSERT INTO agent_instance (
			id, namespace, user_id, request_id, context_id, prepared_revision, state, operation, labels, data
		) VALUES (
			$1, 'team-a', 'alice', $2, $1,
			'revision-before-relay', 'READY', 'NONE', '{}', '\x00'
		)
	`, instanceID, requestID); err != nil {
		t.Fatal(err)
	}
	bindingID := "mcp-" + strings.Repeat("a", 64)
	validGrant := `
		INSERT INTO mcp_relay_grant (
			capability_hash, agent_instance_id, revision, binding_id, expires_at
		) VALUES ($1, $2, 'revision-before-relay', $3, '2027-01-01 00:00:00+00')
	`
	if err := exec(validGrant, make([]byte, 31), instanceID, bindingID); err == nil {
		t.Fatal("31-byte capability hash passed the migration constraint")
	}
	if err := exec(validGrant, make([]byte, 32), instanceID, bindingID); err == nil {
		t.Fatal("zero capability hash passed the migration constraint")
	}
	if err := exec(validGrant, append([]byte{1}, make([]byte, 31)...), instanceID, "not-a-binding"); err == nil {
		t.Fatal("non-canonical binding ID passed the migration constraint")
	}
	if err := exec(`
		INSERT INTO mcp_relay_grant (
			capability_hash, agent_instance_id, revision, binding_id, expires_at
		) VALUES ($1, $2, 'revision-before-relay', $3, '1970-01-01 00:00:00+00')
	`, append([]byte{1}, make([]byte, 31)...), instanceID, bindingID); err == nil {
		t.Fatal("zero/epoch expiry passed the migration constraint")
	}
	if err := exec(`
		INSERT INTO mcp_relay_grant (
			capability_hash, agent_instance_id, revision, binding_id, expires_at
		) VALUES ($1, $2, 'revision-before-relay', $3, 'infinity')
	`, append([]byte{2}, make([]byte, 31)...), instanceID, bindingID); err == nil {
		t.Fatal("infinite expiry passed the migration constraint")
	}
	validHash := make([]byte, 32)
	validHash[0] = 1
	if err := exec(validGrant, validHash, instanceID, bindingID); err != nil {
		t.Fatalf("valid grant failed migration constraints: %v", err)
	}

	// Down must remove dependent grants before removing the revision policy.
	migrateCoreTo(t, connStr, 18)
	var grantTableExists, policyColumnExists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('mcp_relay_grant') IS NOT NULL`).Scan(&grantTableExists); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'runtime_revision' AND column_name = 'mcp_policy'
		)
	`).Scan(&policyColumnExists); err != nil {
		t.Fatal(err)
	}
	if grantTableExists || policyColumnExists {
		t.Fatalf("down migration left grant table=%v policy column=%v", grantTableExists, policyColumnExists)
	}

	// A second upgrade safely re-backfills surviving old revisions.
	migrateCoreTo(t, connStr, 19)
	var policy string
	if err := db.QueryRowContext(ctx, `SELECT mcp_policy::text FROM runtime_revision WHERE revision = 'revision-before-relay'`).Scan(&policy); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(policy, `"version": "v1"`) || !strings.Contains(policy, `"bindings": []`) {
		t.Fatalf("re-upgrade compatibility policy = %s", policy)
	}
}
