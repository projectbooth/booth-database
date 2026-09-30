// Package provision is everything booth-database does to PostgreSQL itself: create a workspace's
// database on first use, mint a short-lived login role for one credential-broker lease, and reap
// expired leases. It holds the one admin connection this module has (the bundled server's
// superuser, or the operator-supplied role on an external cluster) — that credential never
// leaves this package and is never handed to anyone (ADR 0080: a provider mints derived
// credentials, never returns its own).
//
// The isolation model (docs/decisions/0001 §1-§3):
//
//   - One database per workspace, owned by a NOLOGIN group role `<db>_rw`, with a second NOLOGIN
//     group `<db>_ro` granted SELECT on everything `_rw` creates (via default privileges).
//     CONNECT and TEMPORARY are revoked from PUBLIC, so no role outside those groups can even
//     open a session on the database.
//   - A lease is a fresh LOGIN role, NOINHERIT, member of exactly one of the two groups, with
//     CONNECT granted directly (NOINHERIT means it doesn't pick up the group's CONNECT), and a
//     per-role default `role = <group>` so every session starts as the group. Objects it creates
//     are therefore owned by the group, not the lease, and survive the lease being dropped. A
//     session that runs RESET ROLE falls back to the bare lease role, which by construction has
//     no privileges on anything but CONNECT.
//   - A lease's password is set as a precomputed SCRAM verifier (scram.go), so the plaintext
//     never appears in any SQL statement the server could log.
//   - Expiry is enforced twice: PostgreSQL itself refuses a new login after VALID UNTIL, and the
//     reaper terminates any session still open past it and drops the role.
package provision

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/projectbooth/booth-database/internal/naming"
)

// readyMarker is written as a workspace database's COMMENT once it is fully set up, so a
// half-provisioned database (a crash between CREATE DATABASE and its grants) is finished on the
// next request rather than mistaken for a ready one. Deliberately carries no workspace slug:
// pg_shdescription is readable by every role on the server (see internal/naming).
//
// Since the admin view (ADR 0093) the marker is followed by "; created <RFC 3339 UTC>", because
// PostgreSQL itself records no creation time for a database. The timestamp reveals only when some
// unnamed workspace first used its database. Databases provisioned before that change carry the
// bare marker and report an unknown creation time; readiness is a prefix match so both count.
const readyMarker = "booth-database workspace database v1"

const createdSep = "; created "

func isReady(comment *string) bool {
	return comment != nil && (*comment == readyMarker || strings.HasPrefix(*comment, readyMarker+createdSep))
}

// createdAtFromComment returns the creation time recorded in a ready marker, or nil if none.
func createdAtFromComment(comment *string) *time.Time {
	if comment == nil {
		return nil
	}
	s, ok := strings.CutPrefix(*comment, readyMarker+createdSep)
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

const leaseComment = "booth-database credential-broker lease"

// Options configures a Provisioner.
type Options struct {
	// LeaseConnectionLimit is each lease role's CONNECTION LIMIT, so one issued credential can't
	// exhaust the server's max_connections on its own. <= 0 means 10.
	LeaseConnectionLimit int
	// Now is overridable in tests.
	Now func() time.Time
}

// Provisioner owns the admin connection.
type Provisioner struct {
	pool    *pgxpool.Pool
	connCfg *pgx.ConnConfig // the admin connection's config, re-targeted per workspace database
	opts    Options

	mu    sync.Mutex
	ready map[string]naming.Workspace // databases known fully provisioned, by slug
}

// New opens the admin pool. adminDSN points at a maintenance database (normally `postgres`)
// as a role with CREATEDB and CREATEROLE — the bundled server's superuser by default.
func New(ctx context.Context, adminDSN string, opts Options) (*Provisioner, error) {
	if opts.LeaseConnectionLimit <= 0 {
		opts.LeaseConnectionLimit = 10
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	poolCfg, err := pgxpool.ParseConfig(adminDSN)
	if err != nil {
		return nil, fmt.Errorf("parsing admin DSN: %w", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("opening admin pool: %w", err)
	}
	return &Provisioner{pool: pool, connCfg: poolCfg.ConnConfig, opts: opts, ready: map[string]naming.Workspace{}}, nil
}

// Close releases the admin pool.
func (p *Provisioner) Close() { p.pool.Close() }

// Ping reports whether the admin connection works — what /healthz is built on.
func (p *Provisioner) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

// RestrictMaintenanceAccess revokes PUBLIC's default CONNECT on the `postgres` and `template1`
// maintenance databases, so a lease can only ever open a session on its own workspace's
// database. Always done on the bundled server; opt-in on an external one (ADR 0054's exact
// precedent — this module doesn't own that cluster, and the operator's tooling may rely on the
// default grant).
func (p *Provisioner) RestrictMaintenanceAccess(ctx context.Context) error {
	for _, db := range []string{"postgres", "template1"} {
		if _, err := p.pool.Exec(ctx, "REVOKE CONNECT ON DATABASE "+ident(db)+" FROM PUBLIC"); err != nil {
			return fmt.Errorf("revoking PUBLIC CONNECT on %s: %w", db, err)
		}
	}
	return nil
}

// EnsureWorkspace makes a workspace's database exist and be fully set up, creating it on first
// use (docs/decisions/0001 §4: provisioning is lazy, driven by the first credential request,
// since no workspace-lifecycle signal exists for a module to act on). Idempotent and safe to
// call concurrently, from this process or several.
func (p *Provisioner) EnsureWorkspace(ctx context.Context, ws string) (naming.Workspace, error) {
	names, err := naming.ForWorkspace(ws)
	if err != nil {
		return naming.Workspace{}, err
	}
	p.mu.Lock()
	_, ok := p.ready[ws]
	p.mu.Unlock()
	if ok {
		return names, nil
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return naming.Workspace{}, fmt.Errorf("acquiring admin connection: %w", err)
	}
	defer conn.Release()

	// Serialize provisioning of this one database across every replica/process: CREATE DATABASE
	// can't run inside a transaction, so a session-level advisory lock is the only way to make
	// "check, then create" atomic.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(hashtext($1))", names.Database); err != nil {
		return naming.Workspace{}, fmt.Errorf("locking %s for provisioning: %w", names.Database, err)
	}
	defer func() {
		// Background context: the unlock must happen even if ctx was cancelled mid-provision,
		// or this pooled session keeps holding the lock.
		_, _ = conn.Exec(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", names.Database)
	}()

	if err := p.provision(ctx, conn.Conn(), names); err != nil {
		return naming.Workspace{}, err
	}
	p.mu.Lock()
	p.ready[ws] = names
	p.mu.Unlock()
	return names, nil
}

func (p *Provisioner) provision(ctx context.Context, conn *pgx.Conn, n naming.Workspace) error {
	var exists bool
	var comment *string
	err := conn.QueryRow(ctx,
		"SELECT true, shobj_description(oid, 'pg_database') FROM pg_database WHERE datname = $1", n.Database,
	).Scan(&exists, &comment)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("checking for %s: %w", n.Database, err)
	}
	if exists && isReady(comment) {
		return nil
	}

	for _, role := range []string{n.ReadWrite, n.ReadOnly} {
		if err := createRoleIfMissing(ctx, conn, role); err != nil {
			return err
		}
	}
	// A non-superuser admin (external cluster) must be able to SET ROLE to the owner to create a
	// database owned by it and to set default privileges for it; it has ADMIN on roles it
	// created, which is enough to grant itself membership. Harmless for a superuser.
	if _, err := conn.Exec(ctx, "GRANT "+ident(n.ReadWrite)+" TO CURRENT_USER"); err != nil {
		return fmt.Errorf("granting admin membership in %s: %w", n.ReadWrite, err)
	}

	if !exists {
		if _, err := conn.Exec(ctx, "CREATE DATABASE "+ident(n.Database)+" OWNER "+ident(n.ReadWrite)); err != nil {
			return fmt.Errorf("creating database %s: %w", n.Database, err)
		}
	}
	db := ident(n.Database)
	for _, stmt := range []string{
		"REVOKE ALL ON DATABASE " + db + " FROM PUBLIC",
		"GRANT CONNECT, TEMPORARY ON DATABASE " + db + " TO " + ident(n.ReadWrite),
		"GRANT CONNECT ON DATABASE " + db + " TO " + ident(n.ReadOnly),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("setting database privileges on %s: %w", n.Database, err)
		}
	}

	if err := p.inDatabase(ctx, n.Database, func(wc *pgx.Conn) error {
		rw, ro := ident(n.ReadWrite), ident(n.ReadOnly)
		return execAll(ctx, wc,
			// PostgreSQL < 15 grants CREATE on public to PUBLIC; revoke explicitly on every
			// version so behaviour doesn't depend on which server the admin pointed us at.
			"REVOKE ALL ON SCHEMA public FROM PUBLIC",
			"ALTER SCHEMA public OWNER TO "+rw,
			"GRANT USAGE ON SCHEMA public TO "+ro,
			"GRANT SELECT ON ALL TABLES IN SCHEMA public TO "+ro,
			"GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO "+ro,
			"ALTER DEFAULT PRIVILEGES FOR ROLE "+rw+" GRANT SELECT ON TABLES TO "+ro,
			"ALTER DEFAULT PRIVILEGES FOR ROLE "+rw+" GRANT SELECT ON SEQUENCES TO "+ro,
			"ALTER DEFAULT PRIVILEGES FOR ROLE "+rw+" GRANT USAGE ON SCHEMAS TO "+ro,
		)
	}); err != nil {
		return fmt.Errorf("setting up %s: %w", n.Database, err)
	}

	marker := readyMarker + createdSep + p.opts.Now().UTC().Format(time.RFC3339)
	if _, err := conn.Exec(ctx, "COMMENT ON DATABASE "+db+" IS "+literal(marker)); err != nil {
		return fmt.Errorf("marking %s ready: %w", n.Database, err)
	}
	log.Printf("provisioned workspace database %s", n.Database) // never the slug: see internal/naming
	return nil
}

// Lease is one issued credential.
type Lease struct {
	Database  string
	Username  string
	Password  string
	ExpiresAt time.Time
}

// Issue mints a login role for one lease: good for exactly ws's database, as its readwrite or
// read-only group, until ttl from now.
func (p *Provisioner) Issue(ctx context.Context, ws, leaseID string, readWrite bool, ttl time.Duration) (Lease, error) {
	if ttl <= 0 {
		return Lease{}, fmt.Errorf("ttl must be positive")
	}
	n, err := p.EnsureWorkspace(ctx, ws)
	if err != nil {
		return Lease{}, err
	}
	role, err := naming.LeaseRole(leaseID)
	if err != nil {
		return Lease{}, err
	}
	password, err := newPassword()
	if err != nil {
		return Lease{}, fmt.Errorf("generating password: %w", err)
	}
	verifier, err := scramVerifier(password)
	if err != nil {
		return Lease{}, err
	}
	expires := p.opts.Now().Add(ttl).UTC().Truncate(time.Millisecond)
	group := n.GroupFor(readWrite)

	stmts := []string{
		fmt.Sprintf("CREATE ROLE %s WITH LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS CONNECTION LIMIT %d VALID UNTIL %s PASSWORD %s IN ROLE %s",
			ident(role), p.opts.LeaseConnectionLimit, literal(expires.Format(time.RFC3339Nano)), literal(verifier), ident(group)),
		"GRANT CONNECT ON DATABASE " + ident(n.Database) + " TO " + ident(role),
		"ALTER ROLE " + ident(role) + " SET role = " + literal(group),
		"COMMENT ON ROLE " + ident(role) + " IS " + literal(leaseComment),
	}
	if !readWrite {
		// Belt and braces: the read-only group already has no write privilege anywhere.
		stmts = append(stmts, "ALTER ROLE "+ident(role)+" SET default_transaction_read_only = on")
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return Lease{}, fmt.Errorf("beginning lease transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			// The error text never includes the statement itself (which carries the verifier).
			return Lease{}, fmt.Errorf("creating lease role: %w", sanitize(err))
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Lease{}, fmt.Errorf("committing lease role: %w", sanitize(err))
	}
	return Lease{Database: n.Database, Username: role, Password: password, ExpiresAt: expires}, nil
}

// Reap ends every expired lease: stops new logins, terminates any session still open, and drops
// the role. Returns how many roles were dropped. A failure on one role is logged and retried on
// the next pass rather than stopping the rest.
func (p *Provisioner) Reap(ctx context.Context) (int, error) {
	rows, err := p.pool.Query(ctx, `
		SELECT r.rolname, COALESCE((
			SELECT g.rolname FROM pg_auth_members m JOIN pg_roles g ON g.oid = m.roleid
			WHERE m.member = r.oid LIMIT 1), '')
		FROM pg_roles r
		WHERE r.rolname LIKE $1 AND r.rolvaliduntil IS NOT NULL AND r.rolvaliduntil <= now()`,
		strings.ReplaceAll(naming.LeasePrefix, "_", `\_`)+"%")
	if err != nil {
		return 0, fmt.Errorf("listing expired leases: %w", err)
	}
	type expired struct{ role, group string }
	var todo []expired
	for rows.Next() {
		var e expired
		if err := rows.Scan(&e.role, &e.group); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	dropped := 0
	for _, e := range todo {
		if err := p.dropLease(ctx, e.role, e.group); err != nil {
			log.Printf("reaper: couldn't end expired lease role %s yet (will retry): %v", e.role, err)
			continue
		}
		dropped++
	}
	return dropped, nil
}

func (p *Provisioner) dropLease(ctx context.Context, role, group string) error {
	r := ident(role)
	if _, err := p.pool.Exec(ctx, "ALTER ROLE "+r+" NOLOGIN"); err != nil {
		return fmt.Errorf("disabling login: %w", err)
	}
	// pg_stat_activity.usename is the session (authenticated) user, so this catches a session
	// that has since SET ROLE'd to the group — which every lease session does at start.
	if _, err := p.pool.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE usename = $1 AND pid <> pg_backend_pid()", role); err != nil {
		return fmt.Errorf("terminating sessions: %w", err)
	}
	err := p.dropRole(ctx, role)
	if err == nil {
		return nil
	}
	// 2BP01 dependent_objects_still_exist: the lease somehow owns something inside its
	// workspace database (not possible by construction — see the package doc — but a role
	// that can't be dropped would be retried forever, so handle it rather than assume).
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "2BP01" {
		if db := naming.DatabaseForGroup(group); db != "" {
			if cerr := p.inDatabase(ctx, db, func(wc *pgx.Conn) error {
				return execAll(ctx, wc, "REASSIGN OWNED BY "+r+" TO "+ident(naming.ReadWriteGroup(db)), "DROP OWNED BY "+r)
			}); cerr != nil {
				return fmt.Errorf("reassigning objects out of lease: %w", cerr)
			}
			return p.dropRole(ctx, role)
		}
	}
	return err
}

func (p *Provisioner) dropRole(ctx context.Context, role string) error {
	// DROP OWNED here (in the maintenance database) revokes the lease's CONNECT grant on its
	// workspace database, which lives in the shared catalog.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "DROP OWNED BY "+ident(role)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DROP ROLE IF EXISTS "+ident(role)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RunReaper calls Reap every interval until ctx is done.
func (p *Provisioner) RunReaper(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if n, err := p.Reap(ctx); err != nil {
			if ctx.Err() == nil {
				log.Printf("reaper: %v", err)
			}
		} else if n > 0 {
			log.Printf("reaper: ended %d expired lease(s)", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Provisioner) inDatabase(ctx context.Context, database string, fn func(*pgx.Conn) error) error {
	cfg := p.connCfg.Copy()
	cfg.Database = database
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", database, err)
	}
	defer conn.Close(context.Background())
	return fn(conn)
}

func createRoleIfMissing(ctx context.Context, conn *pgx.Conn, role string) error {
	_, err := conn.Exec(ctx, "CREATE ROLE "+ident(role)+" NOLOGIN")
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "42710" { // duplicate_object
		return nil
	}
	if err != nil {
		return fmt.Errorf("creating role %s: %w", role, err)
	}
	return nil
}

func execAll(ctx context.Context, conn *pgx.Conn, stmts ...string) error {
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

// literal quotes a SQL string literal. Only ever used on values this package generated itself
// (timestamps, verifiers, fixed comments), never on caller input; standard_conforming_strings
// (on by default since PostgreSQL 9.1) means doubling single quotes is the complete escape.
func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// sanitize reduces a server error to its code and message. PgError's Error() doesn't include
// the query text today, but its Detail/Where fields are free-form server output; keeping only
// code and message means a statement carrying a verifier can never be echoed into a log line
// by a future driver or server change. Belt and braces for "a credential is never logged".
// Non-server errors (connection failures, context cancellation) never carry statement text.
func sanitize(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("postgres error %s: %s", pgErr.Code, pgErr.Message)
	}
	return err
}
