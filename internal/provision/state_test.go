package provision

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The table-events state (state.go) holds workspace slugs, so no workspace lease may read it
// (docs/decisions/0006 §1). Two cases: the bundled server, where a lease can't even connect to the
// maintenance database, and an external cluster without restrictMaintenanceAccess, simulated by
// granting the lease CONNECT there (what PUBLIC's default grant would give it). Both a read and a
// write lease are tried.
func TestState_LeasesCannotReadIt(t *testing.T) {
	p := newProvisioner(t)
	ctx := context.Background()
	ws := uniqueWorkspace("state")
	if err := p.RememberWorkspace(ctx, ws); err != nil {
		t.Fatal(err)
	}
	maintenance := p.connCfg.Database

	for _, readWrite := range []bool{false, true} {
		l := issue(t, p, ws, readWrite, time.Hour)

		// Bundled: CONNECT on the maintenance database is revoked from PUBLIC.
		if c, err := connectAs(t, l, maintenance); err == nil {
			c.Close(ctx)
			t.Fatalf("rw=%v: a lease connected to the maintenance database %s", readWrite, maintenance)
		}

		// External without restrictMaintenanceAccess: it can connect, but sees nothing.
		if _, err := p.pool.Exec(ctx, "GRANT CONNECT ON DATABASE "+ident(maintenance)+" TO "+ident(l.Username)); err != nil {
			t.Fatal(err)
		}
		c, err := connectAs(t, l, maintenance)
		if err != nil {
			t.Fatalf("rw=%v: connecting after the grant: %v", readWrite, err)
		}
		// Reads must fail on privilege itself. Writes may be stopped earlier by a read lease's
		// default_transaction_read_only (25006); either way they're refused.
		for sql, codes := range map[string][]string{
			"SELECT slug FROM booth_database.workspaces":                               {"42501"},
			"SELECT workspace FROM booth_database.published_tables":                    {"42501"},
			"INSERT INTO booth_database.workspaces (slug, database) VALUES ('x', 'y')": {"42501", "25006"},
			"CREATE TABLE booth_database.planted (v int)":                              {"42501", "25006"},
		} {
			_, err := c.Exec(ctx, sql)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || !slices.Contains(codes, pgErr.Code) {
				t.Errorf("rw=%v: %s: want %v, got %v", readWrite, sql, codes, err)
			}
		}
		c.Close(ctx)
	}
}
