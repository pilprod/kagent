package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestMigration000017RuntimeRevisionBackendIdentity(t *testing.T) {
	connStr := startTestDB(t)
	migrateCoreTo(t, connStr, 16)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	insertLegacyRuntimeRevision(t, ctx, db, "legacy", "legacy-actor")
	migrateCoreTo(t, connStr, 17)

	var backendKind string
	var externalRuntime *string
	if err := db.QueryRowContext(ctx, `
		SELECT backend_kind, external_runtime
		FROM runtime_revision
		WHERE revision = 'legacy'
	`).Scan(&backendKind, &externalRuntime); err != nil {
		t.Fatalf("read migrated revision: %v", err)
	}
	if backendKind != "substrate" || externalRuntime != nil {
		t.Fatalf("legacy backend identity = %q/%v, want substrate/NULL", backendKind, externalRuntime)
	}

	tests := []struct {
		name            string
		backendKind     any
		externalRuntime any
		wantValid       bool
	}{
		{name: "substrate null", backendKind: "substrate", wantValid: true},
		{name: "substrate empty", backendKind: "substrate", externalRuntime: "", wantValid: true},
		{name: "substrate runtime", backendKind: "substrate", externalRuntime: "codex"},
		{name: "external codex", backendKind: "external", externalRuntime: "codex", wantValid: true},
		{name: "external claude", backendKind: "external", externalRuntime: "claude", wantValid: true},
		{name: "external null", backendKind: "external"},
		{name: "external empty", backendKind: "external", externalRuntime: ""},
		{name: "external unknown", backendKind: "external", externalRuntime: "other"},
		{name: "unknown backend", backendKind: "other"},
		{name: "null backend"},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := db.ExecContext(ctx, `
				INSERT INTO runtime_revision (
					revision, namespace, agent_template_name, agent_template_uid,
					harness_name, harness_uid, source_snapshot, agent_card,
					backend_kind, external_runtime,
					actor_template_namespace, actor_template_name, phase
				) VALUES ($1, 'team-a', 'assistant', 'template-uid',
					'kagent', 'harness-uid', '{}', '{}', $2, $3,
					'team-a', $4, 'Ready')
			`, fmt.Sprintf("revision-%d", index), test.backendKind, test.externalRuntime, fmt.Sprintf("actor-%d", index))
			if test.wantValid && err != nil {
				t.Fatalf("valid identity rejected: %v", err)
			}
			if !test.wantValid && err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}

	migrateCoreTo(t, connStr, 16)
	for _, column := range []string{"backend_kind", "external_runtime"} {
		var exists bool
		if err := db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'runtime_revision' AND column_name = $1
			)
		`, column).Scan(&exists); err != nil {
			t.Fatalf("check column %s: %v", column, err)
		}
		if exists {
			t.Errorf("down migration retained runtime_revision.%s", column)
		}
	}
}

func insertLegacyRuntimeRevision(t *testing.T, ctx context.Context, db *sql.DB, revision string, actorTemplate string) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card,
			actor_template_namespace, actor_template_name, phase
		) VALUES ($1, 'team-a', 'assistant', 'template-uid',
			'kagent', 'harness-uid', '{}', '{}',
			'team-a', $2, 'Ready')
	`, revision, actorTemplate); err != nil {
		t.Fatalf("insert legacy runtime revision: %v", err)
	}
}
