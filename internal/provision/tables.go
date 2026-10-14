package provision

// ADR 0116: what the Tables view, the preview, the SQL console and the database.table.* events
// need from PostgreSQL.
//
//   - IntrospectWorkspace lists a workspace database on the admin connection (catalog reads only).
//   - OpenViewerSession opens a connection AS a fresh read-only lease of the workspace's `_ro`
//     group: every preview and console query runs as that, whatever the caller's role
//     (viewer-capped, ADR 0104's pattern). Closing it drops the lease role and terminates the
//     session, so a cancelled query releases both.
//   - The state store (published-table state and the workspace slugs the reconcile loop knows)
//     lives in a private schema of the admin's maintenance database: docs/decisions/0006 §1.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"

	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/tables"
)

// ErrNotProvisioned means the workspace has never had a database created (databases are created
// lazily on the first credential request, docs/decisions/0001 §4). Read paths never create one.
var ErrNotProvisioned = errors.New("this workspace has no database yet")

// provisioned reports whether ws's database exists and is fully set up, without creating it: the
// ready marker, a shared-catalog read (no pg_database_size, which stats every file).
func (p *Provisioner) provisioned(ctx context.Context, n naming.Workspace) (bool, error) {
	var comment *string
	err := p.pool.QueryRow(ctx, "SELECT shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", n.Database).Scan(&comment)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return isReady(comment), nil
}

// IntrospectWorkspace lists ws's database (internal/tables). provisioned=false, with no error,
// for a workspace that has no database yet.
func (p *Provisioner) IntrospectWorkspace(ctx context.Context, ws string) (rels []tables.Relation, provisioned bool, err error) {
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		return nil, false, err
	}
	if ok, err := p.provisioned(ctx, n); err != nil || !ok {
		return nil, false, err
	}
	err = p.inDatabase(ctx, n.Database, func(c *pgx.Conn) error {
		var ierr error
		rels, ierr = tables.Introspect(ctx, c)
		return ierr
	})
	if err != nil {
		return nil, true, err
	}
	return rels, true, nil
}

// ViewerSession is one connection to a workspace database as a dedicated read-only lease.
type ViewerSession struct {
	Conn *pgx.Conn
	// Database is the workspace database the session is connected to.
	Database string

	p     *Provisioner
	role  string
	group string
}

// OpenViewerSession mints a read-only lease for ws's database, good for ttl, and connects as it.
// The lease is a member of `<db>_ro` only (SELECT, nothing else) and carries
// default_transaction_read_only, whatever role the caller holds in the workspace. It never
// provisions: a workspace without a database gets ErrNotProvisioned.
//
// The lease is not handed to anyone: its password exists only in this process, for this one
// connection, and Close drops the role. If the process dies first, PostgreSQL stops accepting
// it at ttl and the reaper removes it.
func (p *Provisioner) OpenViewerSession(ctx context.Context, ws string, ttl time.Duration) (*ViewerSession, error) {
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		return nil, err
	}
	ok, err := p.provisioned(ctx, n)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrNotProvisioned
	}
	l, err := p.Issue(ctx, ws, uuid.NewString(), false, ttl)
	if err != nil {
		return nil, err
	}
	s := &ViewerSession{Database: l.Database, p: p, role: l.Username, group: n.ReadOnly}

	cfg := p.connCfg.Copy()
	cfg.User, cfg.Password, cfg.Database = l.Username, l.Password, l.Database
	cfg.RuntimeParams = map[string]string{"application_name": "booth-database query"}
	// On a cancelled context (the caller's cancel, a disconnect, or the hard deadline), ask the
	// server to cancel the running statement at once, and give up on the socket 2s later.
	cfg.BuildContextWatcherHandler = func(pc *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: pc, CancelRequestDelay: 0, DeadlineDelay: 2 * time.Second}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		s.dropLease()
		return nil, fmt.Errorf("connecting as a read-only lease: %w", sanitize(err))
	}
	s.Conn = conn
	return s, nil
}

// Close closes the connection and drops the lease role, terminating any session still open on
// it. Safe to call once; uses its own context so it completes even after the caller's ended.
func (s *ViewerSession) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if s.Conn != nil {
		_ = s.Conn.Close(ctx)
	}
	s.dropLease()
}

func (s *ViewerSession) dropLease() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.p.dropLease(ctx, s.role, s.group); err != nil {
		// The role expires on its own (VALID UNTIL) and the reaper drops it then.
		log.Printf("query: couldn't drop read-only lease %s yet (the reaper will): %v", s.role, err)
	}
}

// LeaseRole is the session's login role, for tests.
func (s *ViewerSession) LeaseRole() string { return s.role }
