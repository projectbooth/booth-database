package provision

// Table-scoped write leases (ADR 0117 point 4, "Reconciled design" item 6; docs/decisions/0008).
//
// A tablewrite lease is an ordinary bdb_lease_ role whose one group is a per-grant write group,
// bdb_w_<db hash>_<grant hash> (naming.WriteGroup). The group is a member of nothing (never _rw,
// never _ro) and holds, per declared table: SELECT; DELETE if declared; INSERT (cols) and
// UPDATE (cols) as column-level grants on the declared writable columns; USAGE on the table's
// schema and on the sequences its columns draw from. Nothing else: no TRUNCATE, REFERENCES or
// TRIGGER, no CREATE anywhere, no TEMPORARY on the database.
//
// The privileges live on the group, not the lease, and are re-derived on every issuance: revoke
// everything the group holds, then grant exactly the requested set, in one transaction under a
// per-group advisory lock. So a dropped table loses its grants with it, a recreated one gets them
// at the next issuance, a column the owner adds is not writable until declared, and narrowing a
// grant applies to every live lease of it at its next statement.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/tables"
)

// Write operations a grant may declare per table.
const (
	OpInsert = "insert"
	OpUpdate = "update"
	OpDelete = "delete"
)

const (
	// MaxWriteTables bounds one grant's table list.
	MaxWriteTables = 50
	// MaxGrantIDLen bounds a grant id (booth-api's own id for the grant, e.g. "api:<id>").
	MaxGrantIDLen = 128
	// DefaultWriteGroupIdle is how long a write group with no members is kept after its last
	// issuance before the reaper drops it. Long enough to cover an issuance in progress (the
	// group is set up before the lease that joins it is created).
	DefaultWriteGroupIdle = 10 * time.Minute

	writeGroupComment = "booth-database write group; issued "
)

// writeLeaseSettings are a write lease's per-role session defaults. UPDATE and DELETE are enough
// to LOCK TABLE ... ACCESS EXCLUSIVE, so these keep a stuck client from blocking the owner's own
// pipeline for long. They are defaults for booth-api's sessions, not enforcement: a session can
// SET its own. The lease's expiry is the enforced bound.
var writeLeaseSettings = []string{
	"statement_timeout = '30s'",
	"lock_timeout = '5s'",
	"idle_in_transaction_session_timeout = '60s'",
}

// writeExcludedSchemas can never hold a granted table: the system schemas (plus every "pg_*"),
// and this module's own schema name.
var writeExcludedSchemas = append(slices.Clone(tables.ExcludedSchemas), "booth_database")

// WriteTable is one declared table in a tablewrite scope.
type WriteTable struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	Operations []string `json:"operations"`
	Columns    []string `json:"columns"`
}

// WriteScope is a tablewrite request's scope.write.
type WriteScope struct {
	Grant  string
	Tables []WriteTable
}

// GrantedTable is what was actually granted for one table (the echo).
type GrantedTable struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	Operations []string `json:"operations"`
	Columns    []string `json:"columns"`
	// Sequences are the sequences granted USAGE for this table, each "schema.name" (quoted
	// where PostgreSQL would quote it).
	Sequences []string `json:"sequences"`
}

// WriteAudit is who asked, recorded with the grant for the Tables view and the audit line. Never
// used to authorize anything.
type WriteAudit struct {
	Subject    string
	OnBehalfOf string
}

// ScopeError is a request this module refuses to scope (ADR 0088's "refuse rather than widen"):
// the provider answers 422 scope_not_supported with its message.
type ScopeError struct{ msg string }

func (e *ScopeError) Error() string { return e.msg }

// NewScopeError is a ScopeError with msg, for callers (and tests) outside this package.
func NewScopeError(msg string) error { return &ScopeError{msg: msg} }

func scopeErrorf(format string, args ...any) error {
	return &ScopeError{msg: fmt.Sprintf(format, args...)}
}

// ValidGrantID reports whether g can be a grant id: 1 to 128 bytes of valid UTF-8 with no NUL
// or other control character.
func ValidGrantID(g string) bool {
	if g == "" || len(g) > MaxGrantIDLen || !utf8.ValidString(g) {
		return false
	}
	return !strings.ContainsFunc(g, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ValidateWriteScope checks a scope's shape, before anything touches PostgreSQL.
func ValidateWriteScope(s WriteScope) error {
	if !ValidGrantID(s.Grant) {
		return scopeErrorf("scope.write.grant must be 1 to %d characters with no control characters", MaxGrantIDLen)
	}
	if len(s.Tables) == 0 {
		return scopeErrorf("scope.write.tables must name at least one table (an empty list is only accepted with options.revoke)")
	}
	if len(s.Tables) > MaxWriteTables {
		return scopeErrorf("scope.write.tables names %d tables; at most %d", len(s.Tables), MaxWriteTables)
	}
	seen := map[tables.Key]bool{}
	for _, t := range s.Tables {
		q := tables.QualifiedName(t.Schema, t.Name)
		if !tables.ValidIdentifier(t.Schema) || !tables.ValidIdentifier(t.Name) {
			return scopeErrorf("table %s: schema and name must each be 1 to 63 bytes with no NUL", q)
		}
		k := tables.Key{Schema: t.Schema, Name: t.Name}
		if seen[k] {
			return scopeErrorf("table %s is listed twice", q)
		}
		seen[k] = true
		if len(t.Operations) == 0 {
			return scopeErrorf("table %s: operations must name at least one of insert, update, delete", q)
		}
		ops := map[string]bool{}
		for _, op := range t.Operations {
			if op != OpInsert && op != OpUpdate && op != OpDelete {
				return scopeErrorf("table %s: unknown operation %q (insert, update or delete)", q, op)
			}
			if ops[op] {
				return scopeErrorf("table %s: operation %q is listed twice", q, op)
			}
			ops[op] = true
		}
		writesColumns := ops[OpInsert] || ops[OpUpdate]
		if writesColumns && len(t.Columns) == 0 {
			return scopeErrorf("table %s: insert and update need at least one writable column", q)
		}
		if !writesColumns && len(t.Columns) > 0 {
			return scopeErrorf("table %s: columns are only meaningful with insert or update", q)
		}
		cols := map[string]bool{}
		for _, c := range t.Columns {
			if !tables.ValidIdentifier(c) {
				return scopeErrorf("table %s: column names must be 1 to 63 bytes with no NUL", q)
			}
			if cols[c] {
				return scopeErrorf("table %s: column %s is listed twice", q, tables.QuoteIdent(c))
			}
			cols[c] = true
		}
	}
	return nil
}

func excludedForWrites(schema string) bool {
	return strings.HasPrefix(schema, "pg_") || slices.Contains(writeExcludedSchemas, schema)
}

// IssueWrite mints a tablewrite lease for scope in ws's database: it (re)sets the grant's write
// group to exactly scope, then creates a lease role in that group. A table that can't be granted
// is a *ScopeError, and nothing changes.
func (p *Provisioner) IssueWrite(ctx context.Context, ws, leaseID string, scope WriteScope, ttl time.Duration, audit WriteAudit) (Lease, []GrantedTable, error) {
	if ttl <= 0 {
		return Lease{}, nil, fmt.Errorf("ttl must be positive")
	}
	if err := ValidateWriteScope(scope); err != nil {
		return Lease{}, nil, err
	}
	if _, err := naming.LeaseRole(leaseID); err != nil {
		return Lease{}, nil, err
	}
	n, err := p.EnsureWorkspace(ctx, ws)
	if err != nil {
		return Lease{}, nil, err
	}
	p.rememberQuietly(ctx, ws)
	group := n.WriteGroup(scope.Grant)

	// The group first, stamped as issued now, so the reaper's sweep leaves it alone while the
	// lease that will join it is being created.
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return Lease{}, nil, err
	}
	err = createRoleIfMissing(ctx, conn.Conn(), group)
	if err == nil {
		_, err = conn.Exec(ctx, "COMMENT ON ROLE "+ident(group)+" IS "+literal(writeGroupComment+p.opts.Now().UTC().Format(time.RFC3339)))
	}
	if err == nil {
		err = p.revokeMemberships(ctx, conn.Conn(), group)
	}
	conn.Release()
	if err != nil {
		return Lease{}, nil, fmt.Errorf("setting up write group: %w", err)
	}

	var granted []GrantedTable
	if err := p.inDatabase(ctx, n.Database, func(c *pgx.Conn) error {
		var gerr error
		granted, gerr = p.applyWriteGrants(ctx, c, n, group, scope.Tables)
		return gerr
	}); err != nil {
		return Lease{}, nil, err
	}

	lease, err := p.createLease(ctx, n, leaseID, group, writeLeaseSettings, ttl)
	if err != nil {
		return Lease{}, nil, err
	}
	p.recordWriteGrant(ctx, ws, scope.Grant, group, granted, audit)
	return lease, granted, nil
}

// revokeMemberships makes sure the write group is a member of no role. Nothing this module does
// adds one; this undoes anything added by hand, so the group can never reach _rw or _ro.
func (p *Provisioner) revokeMemberships(ctx context.Context, c *pgx.Conn, group string) error {
	rows, err := c.Query(ctx, `
		SELECT g.rolname FROM pg_auth_members m
		  JOIN pg_roles g ON g.oid = m.roleid JOIN pg_roles r ON r.oid = m.member
		 WHERE r.rolname = $1`, group)
	if err != nil {
		return err
	}
	parents, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, parent := range parents {
		if _, err := c.Exec(ctx, "REVOKE "+ident(parent)+" FROM "+ident(group)); err != nil {
			return fmt.Errorf("removing write group from %s: %w", parent, err)
		}
	}
	return nil
}

// applyWriteGrants, inside the workspace database: verify every table, revoke everything the
// group holds, grant exactly the scope. One transaction, as the workspace's _rw (the owner of
// everything granted, so grants are recorded as the owner's and revokes find them).
func (p *Provisioner) applyWriteGrants(ctx context.Context, c *pgx.Conn, n naming.Workspace, group string, want []WriteTable) ([]GrantedTable, error) {
	tx, err := c.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "booth-database/write-group/"+group); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+ident(n.ReadWrite)); err != nil {
		return nil, fmt.Errorf("acting as the workspace owner: %w", err)
	}

	type resolved struct {
		t         WriteTable
		sequences [][2]string // schema, name
	}
	var plan []resolved
	for _, t := range want {
		r, err := p.resolveWriteTable(ctx, tx, n, t)
		if err != nil {
			return nil, err
		}
		plan = append(plan, resolved{t: t, sequences: r})
	}

	if err := revokeAllFrom(ctx, tx, n, group); err != nil {
		return nil, err
	}

	g := ident(group)
	var out []GrantedTable
	for _, r := range plan {
		t := r.t
		q := tables.QualifiedName(t.Schema, t.Name)
		stmts := []string{
			"GRANT USAGE ON SCHEMA " + tables.QuoteIdent(t.Schema) + " TO " + g,
			"GRANT SELECT ON TABLE " + q + " TO " + g,
		}
		cols := make([]string, len(t.Columns))
		for i, col := range t.Columns {
			cols[i] = tables.QuoteIdent(col)
		}
		colList := "(" + strings.Join(cols, ", ") + ")"
		ops := slices.Clone(t.Operations)
		slices.Sort(ops)
		for _, op := range ops {
			switch op {
			case OpInsert:
				stmts = append(stmts, "GRANT INSERT "+colList+" ON TABLE "+q+" TO "+g)
			case OpUpdate:
				stmts = append(stmts, "GRANT UPDATE "+colList+" ON TABLE "+q+" TO "+g)
			case OpDelete:
				stmts = append(stmts, "GRANT DELETE ON TABLE "+q+" TO "+g)
			}
		}
		seqs := []string{}
		for _, s := range r.sequences {
			stmts = append(stmts,
				"GRANT USAGE ON SCHEMA "+tables.QuoteIdent(s[0])+" TO "+g,
				"GRANT USAGE ON SEQUENCE "+tables.QualifiedName(s[0], s[1])+" TO "+g)
			seqs = append(seqs, tables.QualifiedName(s[0], s[1]))
		}
		for _, s := range stmts {
			if _, err := tx.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("granting on %s: %w", q, err)
			}
		}
		out = append(out, GrantedTable{Schema: t.Schema, Name: t.Name, Operations: ops, Columns: slices.Clone(t.Columns), Sequences: seqs})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}

// resolveWriteTable checks one declared table against the live database and returns the
// sequences its columns draw from. Every refusal is a *ScopeError naming the table.
func (p *Provisioner) resolveWriteTable(ctx context.Context, tx pgx.Tx, n naming.Workspace, t WriteTable) ([][2]string, error) {
	q := tables.QualifiedName(t.Schema, t.Name)
	if excludedForWrites(t.Schema) {
		return nil, scopeErrorf("table %s is in a system schema and can never be granted", q)
	}
	if p.opts.WriteDeny.Ignored(t.Schema, t.Name) {
		return nil, scopeErrorf("table %s matches tableEvents.ignore and can never be granted", q)
	}
	var (
		oid         uint32
		kind, owner string
		partition   bool
	)
	err := tx.QueryRow(ctx, `
		SELECT c.oid, c.relkind::text, c.relispartition, pg_get_userbyid(c.relowner)
		  FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		 WHERE ns.nspname = $1 AND c.relname = $2`, t.Schema, t.Name).Scan(&oid, &kind, &partition, &owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, scopeErrorf("table %s does not exist in the workspace database", q)
	}
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", q, err)
	}
	if kind != "r" && kind != "p" {
		return nil, scopeErrorf("%s is not a table (only tables and partitioned tables can be granted)", q)
	}
	if partition {
		return nil, scopeErrorf("table %s is a partition; grant its partitioned table instead", q)
	}
	if owner != n.ReadWrite {
		return nil, scopeErrorf("table %s is not owned by the workspace and can't be granted", q)
	}

	rows, err := tx.Query(ctx, `
		SELECT attname, attgenerated::text, attidentity::text FROM pg_attribute
		 WHERE attrelid = $1 AND attnum > 0 AND NOT attisdropped`, oid)
	if err != nil {
		return nil, err
	}
	type col struct{ generated, identity string }
	have := map[string]col{}
	for rows.Next() {
		var name string
		var c col
		if err := rows.Scan(&name, &c.generated, &c.identity); err != nil {
			rows.Close()
			return nil, err
		}
		have[name] = c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, name := range t.Columns {
		c, ok := have[name]
		switch {
		case !ok:
			return nil, scopeErrorf("table %s has no column %s", q, tables.QuoteIdent(name))
		case c.generated != "":
			return nil, scopeErrorf("column %s of %s is generated and can't be written", tables.QuoteIdent(name), q)
		case c.identity == "a":
			return nil, scopeErrorf("column %s of %s is GENERATED ALWAYS AS IDENTITY and can't be written", tables.QuoteIdent(name), q)
		}
	}

	// Sequences the table's columns draw from: owned (serial) and identity sequences, plus any
	// sequence a column default names.
	rows, err = tx.Query(ctx, `
		SELECT sn.nspname, s.relname FROM pg_depend d
		  JOIN pg_class s ON s.oid = d.objid AND s.relkind = 'S'
		  JOIN pg_namespace sn ON sn.oid = s.relnamespace
		 WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
		   AND d.refobjid = $1 AND d.deptype IN ('a', 'i')
		UNION
		SELECT sn.nspname, s.relname FROM pg_attrdef ad
		  JOIN pg_depend d ON d.classid = 'pg_attrdef'::regclass AND d.objid = ad.oid AND d.refclassid = 'pg_class'::regclass
		  JOIN pg_class s ON s.oid = d.refobjid AND s.relkind = 'S'
		  JOIN pg_namespace sn ON sn.oid = s.relnamespace
		 WHERE ad.adrelid = $1
		ORDER BY 1, 2`, oid)
	if err != nil {
		return nil, err
	}
	var seqs [][2]string
	for rows.Next() {
		var s [2]string
		if err := rows.Scan(&s[0], &s[1]); err != nil {
			rows.Close()
			return nil, err
		}
		if excludedForWrites(s[0]) {
			rows.Close()
			return nil, scopeErrorf("table %s draws from a sequence in a system schema", q)
		}
		seqs = append(seqs, s)
	}
	rows.Close()
	return seqs, rows.Err()
}

// heldBy lists every relation and schema in the current database on which role holds any
// privilege, table-level or column-level, as ready-to-use REVOKE targets.
const heldByQuery = `
WITH g AS (SELECT oid FROM pg_roles WHERE rolname = $1)
SELECT CASE WHEN c.relkind = 'S' THEN 'SEQUENCE ' ELSE 'TABLE ' END || format('%I.%I', ns.nspname, c.relname)
  FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace, g
 WHERE EXISTS (SELECT 1 FROM aclexplode(c.relacl) a WHERE a.grantee = g.oid)
    OR EXISTS (SELECT 1 FROM pg_attribute at, aclexplode(at.attacl) a
                WHERE at.attrelid = c.oid AND a.grantee = g.oid)
UNION ALL
SELECT 'SCHEMA ' || format('%I', ns.nspname)
  FROM pg_namespace ns, g
 WHERE EXISTS (SELECT 1 FROM aclexplode(ns.nspacl) a WHERE a.grantee = g.oid)`

func heldBy(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, role string) ([]string, error) {
	rows, err := q.Query(ctx, heldByQuery, role)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// revokeAllFrom revokes every privilege group holds in this database (relations, their
// columns, schemas, the database itself), then checks nothing is left: a privilege that
// survives (granted by someone the owner can't revoke for) fails the issuance rather than
// being carried into it.
func revokeAllFrom(ctx context.Context, tx pgx.Tx, n naming.Workspace, group string) error {
	targets, err := heldBy(ctx, tx, group)
	if err != nil {
		return fmt.Errorf("listing the write group's privileges: %w", err)
	}
	g := ident(group)
	for _, target := range targets {
		// REVOKE ALL on a table also revokes its column privileges.
		if _, err := tx.Exec(ctx, "REVOKE ALL ON "+target+" FROM "+g); err != nil {
			return fmt.Errorf("revoking from the write group: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, "REVOKE ALL ON DATABASE "+ident(n.Database)+" FROM "+g); err != nil {
		return fmt.Errorf("revoking from the write group: %w", err)
	}
	left, err := heldBy(ctx, tx, group)
	if err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("the write group still holds privileges on %s that the workspace owner can't revoke", strings.Join(left, ", "))
	}
	return nil
}

// RevokeWrite ends a grant (the provider's revoke request): it revokes every privilege of the
// grant's write group, which bites on the next statement of every live lease of it, then ends
// those leases' sessions, drops the leases and drops the group. It returns the workspace's
// database name. A grant that was never issued, or a workspace with no database, is not an
// error: there is nothing to revoke.
func (p *Provisioner) RevokeWrite(ctx context.Context, ws, grant string, audit WriteAudit) (string, error) {
	if !ValidGrantID(grant) {
		return "", scopeErrorf("scope.write.grant must be 1 to %d characters with no control characters", MaxGrantIDLen)
	}
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		return "", err
	}
	group := n.WriteGroup(grant)
	if err := p.dropWriteGroup(ctx, n.Database, group); err != nil {
		return "", err
	}
	p.forgetWriteGrant(ctx, group)
	return n.Database, nil
}

// dropWriteGroup is RevokeWrite's and the sweep's teardown: privileges first (so access ends at
// once), then the member leases, then the group itself.
func (p *Provisioner) dropWriteGroup(ctx context.Context, database, group string) error {
	var exists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", group).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var dbExists bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", database).Scan(&dbExists); err != nil {
		return err
	}
	if dbExists {
		if err := p.inDatabase(ctx, database, func(c *pgx.Conn) error {
			tx, err := c.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "booth-database/write-group/"+group); err != nil {
				return err
			}
			// DROP OWNED revokes every privilege granted to the group in this database, whoever
			// granted it. The group owns nothing (it never logs in, and leases create as it
			// only through DML), so nothing else is dropped.
			if _, err := tx.Exec(ctx, "DROP OWNED BY "+ident(group)); err != nil {
				return fmt.Errorf("revoking the write group's privileges: %w", err)
			}
			return tx.Commit(ctx)
		}); err != nil {
			return err
		}
	}

	rows, err := p.pool.Query(ctx, `
		SELECT r.rolname FROM pg_auth_members m
		  JOIN pg_roles r ON r.oid = m.member JOIN pg_roles g ON g.oid = m.roleid
		 WHERE g.rolname = $1`, group)
	if err != nil {
		return err
	}
	members, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, m := range members {
		if !strings.HasPrefix(m, naming.LeasePrefix) {
			// Not one of ours: just take it out of the group.
			if _, err := p.pool.Exec(ctx, "REVOKE "+ident(group)+" FROM "+ident(m)); err != nil {
				return err
			}
			continue
		}
		if err := p.dropLease(ctx, m, group); err != nil {
			return fmt.Errorf("ending write lease %s: %w", m, err)
		}
	}
	if err := p.dropRole(ctx, group); err != nil {
		return fmt.Errorf("dropping write group: %w", err)
	}
	return nil
}

// SweepWriteGroups drops every write group that has no members and was last issued more than
// idle ago, so no grant outlives its last lease. Returns how many it dropped.
func (p *Provisioner) SweepWriteGroups(ctx context.Context, idle time.Duration) (int, error) {
	if idle <= 0 {
		idle = DefaultWriteGroupIdle
	}
	rows, err := p.pool.Query(ctx, `
		SELECT r.rolname, COALESCE(shobj_description(r.oid, 'pg_authid'), '')
		  FROM pg_roles r
		 WHERE r.rolname LIKE $1
		   AND NOT EXISTS (SELECT 1 FROM pg_auth_members m WHERE m.roleid = r.oid)`,
		strings.ReplaceAll(naming.WriteGroupPrefix, "_", `\_`)+"%")
	if err != nil {
		return 0, fmt.Errorf("listing write groups: %w", err)
	}
	type cand struct{ group, comment string }
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.group, &c.comment); err != nil {
			rows.Close()
			return 0, err
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	now := p.opts.Now()
	dropped := 0
	for _, c := range cands {
		db := naming.DatabaseForWriteGroup(c.group)
		if db == "" || (p.sweepOnly != nil && !p.sweepOnly(c.group)) {
			continue
		}
		if ts, ok := strings.CutPrefix(c.comment, writeGroupComment); ok {
			if issued, err := time.Parse(time.RFC3339, ts); err == nil && now.Sub(issued) < idle {
				continue
			}
		}
		if err := p.dropWriteGroup(ctx, db, c.group); err != nil {
			log.Printf("reaper: couldn't drop idle write group %s yet (will retry): %v", c.group, err)
			continue
		}
		p.forgetWriteGrant(ctx, c.group)
		dropped++
	}
	return dropped, nil
}

// TableWriteAccess is one write group's live privileges on one table, read from the database's
// own ACLs (so the Tables view shows what PostgreSQL will allow, not what was asked for).
type TableWriteAccess struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	// Grant is the grant id recorded when the group was last issued; empty if unknown.
	Grant      string   `json:"grant"`
	Operations []string `json:"operations"`
	Columns    []string `json:"columns"`
}

// liveWriteQuery lists, per (table, write group), the privileges the group holds on it:
// table-level privilege types, and column-level INSERT/UPDATE with the column.
const liveWriteQuery = `
SELECT ns.nspname, c.relname, r.rolname, a.privilege_type, NULL::text
  FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace,
       aclexplode(c.relacl) a JOIN pg_roles r ON r.oid = a.grantee
 WHERE c.relkind IN ('r', 'p') AND r.rolname LIKE $1
UNION ALL
SELECT ns.nspname, c.relname, r.rolname, a.privilege_type, at.attname::text
  FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
  JOIN pg_attribute at ON at.attrelid = c.oid AND at.attnum > 0 AND NOT at.attisdropped,
       aclexplode(at.attacl) a JOIN pg_roles r ON r.oid = a.grantee
 WHERE c.relkind IN ('r', 'p') AND r.rolname LIKE $1`

// WriteAccess lists every table in ws's database that some write group can change. Empty, with
// no error, for a workspace that has no database yet.
func (p *Provisioner) WriteAccess(ctx context.Context, ws string) ([]TableWriteAccess, error) {
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		return nil, err
	}
	if ok, err := p.provisioned(ctx, n); err != nil || !ok {
		return nil, err
	}
	type key struct{ schema, name, group string }
	type acc struct {
		ops  map[string]bool
		cols map[string]bool
	}
	found := map[key]*acc{}
	var order []key
	if err := p.inDatabase(ctx, n.Database, func(c *pgx.Conn) error {
		rows, err := c.Query(ctx, liveWriteQuery, strings.ReplaceAll(naming.WriteGroupPrefix, "_", `\_`)+"%")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k key
			var priv string
			var col *string
			if err := rows.Scan(&k.schema, &k.name, &k.group, &priv, &col); err != nil {
				return err
			}
			if naming.DatabaseForWriteGroup(k.group) != n.Database {
				continue
			}
			a := found[k]
			if a == nil {
				a = &acc{ops: map[string]bool{}, cols: map[string]bool{}}
				found[k] = a
				order = append(order, k)
			}
			switch priv {
			case "INSERT", "UPDATE", "DELETE":
				a.ops[strings.ToLower(priv)] = true
				if col != nil && priv != "DELETE" {
					a.cols[*col] = true
				}
			}
		}
		return rows.Err()
	}); err != nil {
		return nil, err
	}
	grants := p.writeGrantIDs(ctx)
	var out []TableWriteAccess
	for _, k := range order {
		a := found[k]
		if len(a.ops) == 0 {
			continue // SELECT only: nothing writable
		}
		ops := sortedKeys(a.ops)
		cols := sortedKeys(a.cols)
		out = append(out, TableWriteAccess{Schema: k.schema, Name: k.name, Grant: grants[k.group], Operations: ops, Columns: cols})
	}
	return out, nil
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
