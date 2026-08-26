package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func TestMigration000019ExternalRuntimeProfile(t *testing.T) {
	connStr := startTestDB(t)
	migrateCoreTo(t, connStr, 18)

	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card,
			backend_kind, external_runtime,
			actor_template_namespace, actor_template_name, phase
		) VALUES (
			'legacy-empty-runtime', 'team-a', 'assistant', 'template-uid',
			'kagent', 'harness-uid', '{}', '{}',
			'substrate', '', 'team-a', 'legacy-empty-runtime', 'Ready'
		)
	`); err != nil {
		t.Fatalf("insert version 18 substrate revision: %v", err)
	}

	migrateCoreTo(t, connStr, 19)

	var externalRuntime *string
	if err := db.QueryRowContext(ctx, `
		SELECT external_runtime
		FROM runtime_revision
		WHERE revision = 'legacy-empty-runtime'
	`).Scan(&externalRuntime); err != nil {
		t.Fatalf("read normalized substrate runtime: %v", err)
	}
	if externalRuntime != nil {
		t.Fatalf("substrate external_runtime = %q, want NULL", *externalRuntime)
	}

	tests := []struct {
		name            string
		backendKind     any
		externalRuntime any
		externalProfile any
		actorNamespace  any
		actorName       any
		actorUID        any
		phase           any
		goldenSnapshot  any
		wantValid       bool
	}{
		{name: "substrate", backendKind: "substrate", actorNamespace: "team-a", actorName: "actor-substrate", actorUID: "actor-uid", phase: "Ready", goldenSnapshot: "snapshot", wantValid: true},
		{name: "substrate empty runtime", backendKind: "substrate", externalRuntime: "", actorNamespace: "team-a", actorName: "actor-empty", phase: "Ready"},
		{name: "substrate runtime", backendKind: "substrate", externalRuntime: "codex", actorNamespace: "team-a", actorName: "actor-runtime", phase: "Ready"},
		{name: "substrate profile", backendKind: "substrate", externalProfile: `{}`, actorNamespace: "team-a", actorName: "actor-profile", phase: "Ready"},
		{name: "external codex", backendKind: "external", externalRuntime: "codex", externalProfile: `{"version":"v1"}`, actorNamespace: "_external", actorName: "revision-4", actorUID: "", phase: "Ready", goldenSnapshot: "", wantValid: true},
		{name: "external claude", backendKind: "external", externalRuntime: "claude", externalProfile: `{"version":"v1"}`, actorNamespace: "_external", actorName: "revision-5", actorUID: "", phase: "Ready", goldenSnapshot: "", wantValid: true},
		{name: "external null runtime", backendKind: "external", externalProfile: `{}`, actorNamespace: "_external", actorName: "revision-6", actorUID: "", phase: "Ready", goldenSnapshot: ""},
		{name: "external missing profile", backendKind: "external", externalRuntime: "codex", actorNamespace: "_external", actorName: "revision-7", actorUID: "", phase: "Ready", goldenSnapshot: ""},
		{name: "external array profile", backendKind: "external", externalRuntime: "codex", externalProfile: `[]`, actorNamespace: "_external", actorName: "revision-8", actorUID: "", phase: "Ready", goldenSnapshot: ""},
		{name: "external real actor", backendKind: "external", externalRuntime: "codex", externalProfile: `{}`, actorNamespace: "team-a", actorName: "revision-9", actorUID: "", phase: "Ready", goldenSnapshot: ""},
		{name: "external wrong sentinel name", backendKind: "external", externalRuntime: "codex", externalProfile: `{}`, actorNamespace: "_external", actorName: "other", actorUID: "", phase: "Ready", goldenSnapshot: ""},
		{name: "external pending", backendKind: "external", externalRuntime: "codex", externalProfile: `{}`, actorNamespace: "_external", actorName: "revision-11", actorUID: "", phase: "Pending", goldenSnapshot: ""},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			revision := fmt.Sprintf("revision-%d", index)
			_, err := db.ExecContext(ctx, `
				INSERT INTO runtime_revision (
					revision, namespace, agent_template_name, agent_template_uid,
					harness_name, harness_uid, source_snapshot, agent_card,
					backend_kind, external_runtime, external_profile,
					actor_template_namespace, actor_template_name, actor_template_uid,
					phase, golden_snapshot
				) VALUES ($1, 'team-a', 'assistant', 'template-uid',
					'kagent', 'harness-uid', '{}', '{}', $2, $3, $4,
					$5, $6, $7, $8, $9)
			`, revision, test.backendKind, test.externalRuntime, test.externalProfile,
				test.actorNamespace, test.actorName, test.actorUID, test.phase, test.goldenSnapshot)
			if test.wantValid && err != nil {
				t.Fatalf("valid identity rejected: %v", err)
			}
			if !test.wantValid && err == nil {
				t.Fatal("invalid identity was accepted")
			}
		})
	}

	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card,
			backend_kind, actor_template_namespace, actor_template_name, phase
		) VALUES (
			'duplicate-substrate', 'team-a', 'assistant', 'template-uid',
			'kagent', 'harness-uid', '{}', '{}',
			'substrate', 'team-a', 'actor-substrate', 'Ready'
		)
	`); err == nil {
		t.Fatal("duplicate substrate ActorTemplate identity was accepted")
	}

	migrateCoreTo(t, connStr, 18)
	assertColumnExists(t, ctx, db, "runtime_revision", "external_profile", false)

	var archivedRuntime, archivedProfile string
	if err := db.QueryRowContext(ctx, `
		SELECT
			source_snapshot #>> '{__kagent_external_profile_compat_v1,externalRuntime}',
			(source_snapshot #> '{__kagent_external_profile_compat_v1,externalProfile}')::text
		FROM runtime_revision
		WHERE revision = 'revision-4'
	`).Scan(&archivedRuntime, &archivedProfile); err != nil {
		t.Fatalf("read archived external profile: %v", err)
	}
	if archivedRuntime != "codex" || archivedProfile != `{"version": "v1"}` {
		t.Fatalf("archived external identity = %s/%s", archivedRuntime, archivedProfile)
	}

	migrateCoreTo(t, connStr, 19)
	var restoredSource, restoredProfile string
	if err := db.QueryRowContext(ctx, `
		SELECT source_snapshot::text, external_profile::text
		FROM runtime_revision
		WHERE revision = 'revision-4'
	`).Scan(&restoredSource, &restoredProfile); err != nil {
		t.Fatalf("read restored external profile: %v", err)
	}
	if restoredSource != `{}` || restoredProfile != `{"version": "v1"}` {
		t.Fatalf("restored source/profile = %s/%s", restoredSource, restoredProfile)
	}

	// The compatibility envelope survives the older v18 down migration as well,
	// so 19 -> 17 -> 19 restores both backend identity and the profile instead of
	// silently reclassifying external revisions as Substrate.
	migrateCoreTo(t, connStr, 17)
	migrateCoreTo(t, connStr, 19)
	var backendKind, restoredRuntime, roundTripProfile string
	if err := db.QueryRowContext(ctx, `
		SELECT backend_kind, external_runtime, external_profile::text
		FROM runtime_revision
		WHERE revision = 'revision-4'
	`).Scan(&backendKind, &restoredRuntime, &roundTripProfile); err != nil {
		t.Fatalf("read external revision after 19 -> 17 -> 19: %v", err)
	}
	if backendKind != "external" || restoredRuntime != "codex" || roundTripProfile != `{"version": "v1"}` {
		t.Fatalf("restored external identity = %s/%s/%s", backendKind, restoredRuntime, roundTripProfile)
	}

	// A full rollback below the migration that created runtime_revision must not
	// be blocked by compatibility state. Reapplying the core track recreates a
	// clean table because version 8 intentionally dropped the old revision data.
	migrateCoreTo(t, connStr, 7)
	assertTableExists(t, ctx, db, "runtime_revision", false)
	migrateCoreTo(t, connStr, 19)
	assertTableExists(t, ctx, db, "runtime_revision", true)
	var count int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM runtime_revision`).Scan(&count); err != nil {
		t.Fatalf("count revisions after 19 -> 7 -> 19: %v", err)
	}
	if count != 0 {
		t.Fatalf("runtime revisions after destructive rollback = %d, want 0", count)
	}
}

func TestMigration000019RejectsUnprofiledVersion18ExternalRevision(t *testing.T) {
	connStr := startTestDB(t)
	migrateCoreTo(t, connStr, 18)
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card,
			backend_kind, external_runtime,
			actor_template_namespace, actor_template_name, phase
		) VALUES (
			'unprofiled-external', 'team-a', 'assistant', 'template-uid',
			'codex', 'harness-uid', '{}', '{}',
			'external', 'codex', 'team-a', 'legacy-external', 'Ready'
		)
	`); err != nil {
		t.Fatalf("insert version 18 external revision: %v", err)
	}

	_, err = applySource(ctx, connStr, realCoreSource())
	if err == nil {
		t.Fatal("version 19 accepted an external revision without a recoverable profile")
	}
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read migration state after rejected upgrade: %v", err)
	}
	if version != 18 || dirty {
		t.Fatalf("migration state = version %d dirty %t, want clean version 18", version, dirty)
	}
	assertColumnExists(t, ctx, db, "runtime_revision", "external_profile", false)
}

func TestMigration000019RejectsCorruptReclassifiedCompatibilityEnvelope(t *testing.T) {
	connStr := startTestDB(t)
	migrateCoreTo(t, connStr, 18)
	db, err := sql.Open("pgx", connStr)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	// Version 18 reclassifies archived external rows as substrate after a
	// 19 -> 17 -> 18 rollback/upgrade. The reserved actor sentinel lets v19
	// distinguish those rows from real substrate revisions and fail closed when
	// their compatibility envelope has been lost or corrupted.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runtime_revision (
			revision, namespace, agent_template_name, agent_template_uid,
			harness_name, harness_uid, source_snapshot, agent_card,
			backend_kind, actor_template_namespace, actor_template_name, phase
		) VALUES (
			'corrupt-envelope', 'team-a', 'assistant', 'template-uid',
			'codex', 'harness-uid', '{}', '{}',
			'substrate', '_external', 'corrupt-envelope', 'Ready'
		)
	`); err != nil {
		t.Fatalf("insert reclassified version 18 revision: %v", err)
	}

	_, err = applySource(ctx, connStr, realCoreSource())
	if err == nil {
		t.Fatal("version 19 accepted a corrupt reclassified compatibility envelope")
	}
	var version int
	var dirty bool
	if err := db.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil {
		t.Fatalf("read migration state after rejected upgrade: %v", err)
	}
	if version != 18 || dirty {
		t.Fatalf("migration state = version %d dirty %t, want clean version 18", version, dirty)
	}
	assertColumnExists(t, ctx, db, "runtime_revision", "external_profile", false)
}

func assertColumnExists(t *testing.T, ctx context.Context, db *sql.DB, table, column string, want bool) {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = $1
			  AND column_name = $2
		)
	`, table, column).Scan(&exists); err != nil {
		t.Fatalf("check column %s.%s: %v", table, column, err)
	}
	if exists != want {
		t.Fatalf("column %s.%s exists = %t, want %t", table, column, exists, want)
	}
}

func assertTableExists(t *testing.T, ctx context.Context, db *sql.DB, table string, want bool) {
	t.Helper()
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
		t.Fatalf("check table %s: %v", table, err)
	}
	if exists != want {
		t.Fatalf("table %s exists = %t, want %t", table, exists, want)
	}
}
