package provision

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/naming"
)

// Status is what the read-only admin view (ADR 0093) shows for one workspace database: only
// what PostgreSQL can report cheaply, never anything read from inside the workspace's data.
type Status struct {
	Database string `json:"database"`
	// CreatedAt is nil for a database provisioned before creation times were recorded.
	CreatedAt *time.Time `json:"createdAt"`
	// SizeBytes is pg_database_size: a stat of the database's files, not a scan of its rows.
	SizeBytes int64 `json:"sizeBytes"`
	// ActiveCredentials counts unexpired lease roles for this database.
	ActiveCredentials int `json:"activeCredentials"`
	// OpenConnections counts sessions currently connected to this database.
	OpenConnections int `json:"openConnections"`
	// Tables is only filled in for a single-workspace lookup (it needs a connection into the
	// database itself); nil in a cross-workspace listing.
	Tables *int `json:"tables,omitempty"`
}

// statusQuery reports every ready workspace database, or just one when $2 is non-empty. Every
// column comes from the shared catalogs or pg_database_size, so this never connects into a
// workspace database.
const statusQuery = `
SELECT d.datname,
       shobj_description(d.oid, 'pg_database'),
       pg_database_size(d.oid),
       (SELECT count(*) FROM pg_roles r
          JOIN pg_auth_members m ON m.member = r.oid
          JOIN pg_roles g ON g.oid = m.roleid
         WHERE r.rolname LIKE $1 AND r.rolvaliduntil > now()
           AND g.rolname IN (d.datname || '_rw', d.datname || '_ro')),
       (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname)
  FROM pg_database d
 WHERE d.datname LIKE $3 AND ($2 = '' OR d.datname = $2)
 ORDER BY d.datname`

func (p *Provisioner) statuses(ctx context.Context, only string) ([]Status, error) {
	rows, err := p.pool.Query(ctx, statusQuery, likePrefix(naming.LeasePrefix), only, likePrefix(naming.DatabasePrefix))
	if err != nil {
		return nil, fmt.Errorf("reading database status: %w", err)
	}
	defer rows.Close()
	var out []Status
	for rows.Next() {
		var s Status
		var comment *string
		if err := rows.Scan(&s.Database, &comment, &s.SizeBytes, &s.ActiveCredentials, &s.OpenConnections); err != nil {
			return nil, err
		}
		if !isReady(comment) {
			continue // half-provisioned: not a database anyone can use yet
		}
		s.CreatedAt = createdAtFromComment(comment)
		out = append(out, s)
	}
	return out, rows.Err()
}

// WorkspaceStatus reports one workspace's database. provisioned=false means the workspace has
// never requested a credential (databases are created lazily, 0001 §4). Read-only: it never
// provisions anything.
func (p *Provisioner) WorkspaceStatus(ctx context.Context, ws string) (st Status, provisioned bool, err error) {
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		return Status{}, false, err
	}
	list, err := p.statuses(ctx, n.Database)
	if err != nil || len(list) == 0 {
		return Status{}, false, err
	}
	st = list[0]
	if err := p.inDatabase(ctx, n.Database, func(c *pgx.Conn) error {
		var tables int
		err := c.QueryRow(ctx, `
			SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE c.relkind IN ('r', 'p') AND n.nspname <> 'information_schema'
			   AND n.nspname NOT LIKE 'pg\_%'`).Scan(&tables)
		st.Tables = &tables
		return err
	}); err != nil {
		// The size/credential figures are still worth showing; the table count is best-effort.
		log.Printf("status: counting tables in %s: %v", n.Database, err)
		st.Tables = nil
	}
	return st, true, nil
}

// ListDatabases reports every workspace database on the server — for the operator listing only,
// never shown to an ordinary workspace owner (docs/decisions/0004 §1).
func (p *Provisioner) ListDatabases(ctx context.Context) ([]Status, error) {
	return p.statuses(ctx, "")
}

func likePrefix(prefix string) string { return strings.ReplaceAll(prefix, "_", `\_`) + "%" }
