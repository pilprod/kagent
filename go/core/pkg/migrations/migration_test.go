package migrations

import (
	"context"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// realCoreSource is the embedded core track, as BuiltinSources builds it.
func realCoreSource() Source {
	return Source{Name: "core", TrackingTable: "schema_migrations", FS: FS, Dir: "core"}
}

// migrateCoreTo moves the core track to the given version (up or down).
func migrateCoreTo(t *testing.T, connStr string, version uint) {
	t.Helper()
	err := WithMigrator(context.Background(), connStr, realCoreSource(), func(mg *migrate.Migrate) error {
		return mg.Migrate(version)
	})
	if err != nil {
		t.Fatalf("migrate core to %d: %v", version, err)
	}
}
