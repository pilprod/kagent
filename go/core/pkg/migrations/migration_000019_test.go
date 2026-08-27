package migrations

import (
	"context"
	"database/sql"
	"testing"
)

func TestMigration000019RuntimeRevisionPlacementDefaultConstraintAndRollback(t *testing.T) {
	connStr := startTestDB(t)
	migrateCoreTo(t, connStr, 18)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	insertRevision := func(revision, actorTemplate string) error {
		t.Helper()
		_, err := db.ExecContext(ctx, `
			INSERT INTO runtime_revision (
				revision, namespace, agent_template_name, agent_template_uid,
				harness_name, harness_uid, source_snapshot, agent_card, egress_destinations,
				actor_template_namespace, actor_template_name, actor_template_uid, phase, golden_snapshot
			) VALUES (
				$1, 'team-a', 'assistant', 'template-uid',
				'kagent', 'harness-uid', '{}', '{}', '{}',
				'team-a', $2, 'actor-template-uid', 'Ready', ''
			)
		`, revision, actorTemplate)
		return err
	}
	if err := insertRevision("revision-before-placement", "actor-before-placement"); err != nil {
		t.Fatal(err)
	}

	migrateCoreTo(t, connStr, 19)
	assertPlacement := func(revision, want string) {
		t.Helper()
		var got string
		if err := db.QueryRowContext(ctx, `SELECT placement FROM runtime_revision WHERE revision = $1`, revision).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("revision %s placement = %q, want %q", revision, got, want)
		}
	}
	assertPlacement("revision-before-placement", "KubernetesPod")

	// A previous controller omits placement while the schema is already at 19.
	if err := insertRevision("revision-old-writer", "actor-old-writer"); err != nil {
		t.Fatal(err)
	}
	assertPlacement("revision-old-writer", "KubernetesPod")

	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card, egress_destinations,
			actor_template_namespace, actor_template_name, actor_template_uid, phase, golden_snapshot, placement
		) VALUES (
			'revision-external', 'team-a', 'assistant', 'template-uid',
			'codex', 'codex-harness-uid', '{}', '{}', '{}',
			'team-a', 'actor-external', 'actor-template-uid', 'Ready', '', 'ExternalSlot'
		)
	`); err != nil {
		t.Fatalf("valid ExternalSlot placement failed: %v", err)
	}
	assertPlacement("revision-external", "ExternalSlot")

	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card, egress_destinations,
			actor_template_namespace, actor_template_name, actor_template_uid, phase, golden_snapshot, placement
		) VALUES (
			'revision-invalid', 'team-a', 'assistant', 'template-uid',
			'codex', 'codex-harness-uid', '{}', '{}', '{}',
			'team-a', 'actor-invalid', 'actor-template-uid', 'Ready', '', 'NativeProcess'
		)
	`); err == nil {
		t.Fatal("invalid placement passed the database constraint")
	}

	migrateCoreTo(t, connStr, 18)
	var columnExists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'runtime_revision' AND column_name = 'placement'
		)
	`).Scan(&columnExists); err != nil {
		t.Fatal(err)
	}
	if columnExists {
		t.Fatal("down migration left runtime_revision.placement")
	}

	migrateCoreTo(t, connStr, 19)
	assertPlacement("revision-external", "KubernetesPod")
}
