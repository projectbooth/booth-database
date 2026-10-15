package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/tables"
)

// The database.table.* reconcile loop's state (ADR 0116 §2, docs/decisions/0006 §1), in a private
// schema of the admin's own maintenance database (`postgres` on the bundled server): the
// workspaces it knows, and per (workspace, schema, name) what it last got a stream
// acknowledgement for.
//
// It is the one place this module records a workspace slug, which it otherwise never does
// (docs/decisions/0001 §2): events are addressed to a workspace by slug (ADR 0026), and a
// database name is a one-way hash of one. It is still kept out of the shared catalogs 0001 §2
// is about (these are rows in an ordinary table, not database, role or comment names), and the
// schema grants nothing to PUBLIC, so no lease can read it.
const stateDDL = `
CREATE SCHEMA IF NOT EXISTS booth_database;
REVOKE ALL ON SCHEMA booth_database FROM PUBLIC;
CREATE TABLE IF NOT EXISTS booth_database.workspaces (
  slug       text PRIMARY KEY,
  database   text NOT NULL,
  first_seen timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS booth_database.published_tables (
  workspace    text NOT NULL,
  schema_name  text NOT NULL,
  table_name   text NOT NULL,
  schema_hash  text NOT NULL,
  approx_rows  bigint,
  published_at timestamptz NOT NULL,
  PRIMARY KEY (workspace, schema_name, table_name)
);
CREATE TABLE IF NOT EXISTS booth_database.write_grants (
  group_role        text PRIMARY KEY,
  workspace         text NOT NULL,
  grant_id          text NOT NULL,
  tables            jsonb NOT NULL,
  last_issued       timestamptz NOT NULL,
  last_subject      text NOT NULL,
  last_on_behalf_of text NOT NULL
);
REVOKE ALL ON ALL TABLES IN SCHEMA booth_database FROM PUBLIC;`

// ensureState creates the state schema once per process. Serialized across replicas by a
// transaction-scoped advisory lock: concurrent CREATE ... IF NOT EXISTS can still collide.
func (p *Provisioner) ensureState(ctx context.Context) error {
	p.mu.Lock()
	done := p.stateReady
	p.mu.Unlock()
	if done {
		return nil
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext('booth-database/state'))"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, stateDDL); err != nil {
		return fmt.Errorf("creating the table-events state schema: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	p.mu.Lock()
	p.stateReady = true
	p.mu.Unlock()
	return nil
}

// RememberWorkspace records that ws exists, so the reconcile loop visits it. Called whenever a
// slug reaches this module (a credential request, the Tables view); cached in memory after the
// first success. Errors are the caller's to log: never a reason to fail the request.
func (p *Provisioner) RememberWorkspace(ctx context.Context, ws string) error {
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		return err
	}
	p.mu.Lock()
	known := p.remembered[ws]
	p.mu.Unlock()
	if known {
		return nil
	}
	if err := p.ensureState(ctx); err != nil {
		return err
	}
	if _, err := p.pool.Exec(ctx,
		"INSERT INTO booth_database.workspaces (slug, database) VALUES ($1, $2) ON CONFLICT (slug) DO NOTHING",
		ws, n.Database); err != nil {
		return fmt.Errorf("recording workspace: %w", err)
	}
	p.mu.Lock()
	p.remembered[ws] = true
	p.mu.Unlock()
	return nil
}

func (p *Provisioner) rememberQuietly(ctx context.Context, ws string) {
	if err := p.RememberWorkspace(ctx, ws); err != nil && ctx.Err() == nil {
		log.Printf("table events: couldn't record a workspace for the reconcile loop (will retry on its next request): %v", err)
	}
}

// KnownWorkspaces lists every workspace slug recorded by RememberWorkspace.
func (p *Provisioner) KnownWorkspaces(ctx context.Context) ([]string, error) {
	if err := p.ensureState(ctx); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, "SELECT slug FROM booth_database.workspaces ORDER BY slug")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// PublishedTables is what was last published for ws.
func (p *Provisioner) PublishedTables(ctx context.Context, ws string) (map[tables.Key]tables.Published, error) {
	if err := p.ensureState(ctx); err != nil {
		return nil, err
	}
	rows, err := p.pool.Query(ctx, `
		SELECT schema_name, table_name, schema_hash, approx_rows, published_at
		  FROM booth_database.published_tables WHERE workspace = $1`, ws)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[tables.Key]tables.Published{}
	for rows.Next() {
		var pt tables.Published
		if err := rows.Scan(&pt.Schema, &pt.Name, &pt.SchemaHash, &pt.ApproxRows, &pt.PublishedAt); err != nil {
			return nil, err
		}
		out[pt.Key] = pt
	}
	return out, rows.Err()
}

// RecordPublished stores pt as ws's last-published state for its relation.
func (p *Provisioner) RecordPublished(ctx context.Context, ws string, pt tables.Published) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO booth_database.published_tables
		       (workspace, schema_name, table_name, schema_hash, approx_rows, published_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (workspace, schema_name, table_name) DO UPDATE
		   SET schema_hash = EXCLUDED.schema_hash, approx_rows = EXCLUDED.approx_rows,
		       published_at = EXCLUDED.published_at`,
		ws, pt.Schema, pt.Name, pt.SchemaHash, pt.ApproxRows, pt.PublishedAt.UTC())
	return err
}

// ForgetPublished removes ws's state for k, after its delete event was acknowledged.
func (p *Provisioner) ForgetPublished(ctx context.Context, ws string, k tables.Key) error {
	_, err := p.pool.Exec(ctx,
		"DELETE FROM booth_database.published_tables WHERE workspace = $1 AND schema_name = $2 AND table_name = $3",
		ws, k.Schema, k.Name)
	return err
}

// WithEventsLock runs fn while holding a session-level advisory lock for ws's events, so two
// passes (two replicas, or the loop and an on-demand pass) never publish the same change twice.
func (p *Provisioner) WithEventsLock(ctx context.Context, ws string, fn func(context.Context) error) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := "booth-database/table-events/" + ws
	var got bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtext($1))", key).Scan(&got); err != nil {
		return err
	}
	if !got {
		return tables.ErrBusy
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(ctx, "SELECT pg_advisory_unlock(hashtext($1))", key)
	}()
	return fn(ctx)
}

// recordWriteGrant records a tablewrite issuance (ADR 0117): which grant id a write group is
// for (its name is a one-way hash of it), what was granted and who asked. For the Tables view
// and for audit only; PostgreSQL's ACLs are the authority. Best effort, like RememberWorkspace.
func (p *Provisioner) recordWriteGrant(ctx context.Context, ws, grant, group string, granted []GrantedTable, audit WriteAudit) {
	err := p.ensureState(ctx)
	if err == nil {
		var body []byte
		if body, err = json.Marshal(granted); err == nil {
			_, err = p.pool.Exec(ctx, `
				INSERT INTO booth_database.write_grants
				       (group_role, workspace, grant_id, tables, last_issued, last_subject, last_on_behalf_of)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (group_role) DO UPDATE SET tables = EXCLUDED.tables, last_issued = EXCLUDED.last_issued,
				       last_subject = EXCLUDED.last_subject, last_on_behalf_of = EXCLUDED.last_on_behalf_of`,
				group, ws, grant, body, p.opts.Now().UTC(), audit.Subject, audit.OnBehalfOf)
		}
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("write grants: couldn't record an issuance (the grant itself is in place): %v", err)
	}
}

func (p *Provisioner) forgetWriteGrant(ctx context.Context, group string) {
	err := p.ensureState(ctx)
	if err == nil {
		_, err = p.pool.Exec(ctx, "DELETE FROM booth_database.write_grants WHERE group_role = $1", group)
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("write grants: couldn't forget a dropped write group: %v", err)
	}
}

// writeGrantIDs maps write group role names to their recorded grant ids. Empty on error.
func (p *Provisioner) writeGrantIDs(ctx context.Context) map[string]string {
	out := map[string]string{}
	if err := p.ensureState(ctx); err != nil {
		return out
	}
	rows, err := p.pool.Query(ctx, "SELECT group_role, grant_id FROM booth_database.write_grants")
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var g, id string
		if rows.Scan(&g, &id) == nil {
			out[g] = id
		}
	}
	return out
}
