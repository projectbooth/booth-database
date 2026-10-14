// Package tables lists what is inside a workspace's database (ADR 0116): its user schemas'
// tables, views and materialized views, with their columns and an approximate row count. That
// listing backs the native view's Tables tab and the database.table.* events booth-catalog
// indexes (reconcile.go).
//
// Introspection reads the system catalogs only (pg_class, pg_namespace, pg_attribute), never a
// row of user data: the row count is the planner's estimate (pg_class.reltuples), not COUNT(*).
// It runs on the module's own admin connection into the workspace database, so it lists
// everything there; access to a workspace database is by workspace role, not per table, so
// there is nothing to filter (ADR 0116 "Risks and notes").
package tables

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Kind is a relation's kind as published and shown. Partitioned tables are "table"; their
// partitions are not listed separately.
type Kind string

const (
	KindTable            Kind = "table"
	KindView             Kind = "view"
	KindMaterializedView Kind = "materialized_view"
)

// Column is one column of a relation.
type Column struct {
	Name string `json:"name"`
	// Type is format_type()'s rendering, e.g. "integer", "character varying(20)", "numeric(10,2)".
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	// Comment is the column's COMMENT, or null.
	Comment *string `json:"comment"`
}

// Relation is one table, view or materialized view in a user schema.
type Relation struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   Kind   `json:"kind"`
	// Comment is the relation's COMMENT, or null.
	Comment *string  `json:"comment"`
	Columns []Column `json:"columns"`
	// ApproxRows is the planner's estimate: pg_class.reltuples, summed over leaf partitions for a
	// partitioned table. Null when PostgreSQL has no estimate (reltuples = -1: never vacuumed or
	// analyzed) and always null for a view. It can lag the real count; show it as "about N".
	ApproxRows *int64 `json:"approxRows"`
	// SchemaHash changes when the kind, the relation's comment, or any column's name, type,
	// nullability, comment or order changes. Not when rows change.
	SchemaHash string `json:"schemaHash"`
}

// Key identifies a relation inside one workspace database.
type Key struct {
	Schema string
	Name   string
}

func (r Relation) Key() Key { return Key{r.Schema, r.Name} }

// ExcludedSchemas are never listed, besides every schema whose name starts with "pg_" (which
// covers pg_catalog, pg_toast and the per-session pg_temp_N / pg_toast_temp_N). This module puts
// no schema of its own inside a workspace database, so there is no platform-internal schema to
// add today; one added later goes here.
var ExcludedSchemas = []string{"information_schema"}

// relationsQuery lists every table ('r'), partitioned table ('p'), view ('v') and materialized
// view ('m') outside the excluded schemas, skipping partitions themselves.
const relationsQuery = `
SELECT c.oid, n.nspname, c.relname, c.relkind::text, obj_description(c.oid, 'pg_class'),
       CASE
         WHEN c.relkind = 'v' THEN NULL
         WHEN c.relkind = 'p' THEN (
           SELECT sum(l.reltuples)::bigint FROM pg_partition_tree(c.oid) t
             JOIN pg_class l ON l.oid = t.relid
            WHERE t.isleaf AND l.reltuples >= 0)
         WHEN c.reltuples < 0 THEN NULL
         ELSE c.reltuples::bigint
       END
  FROM pg_class c
  JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'p', 'v', 'm')
   AND NOT c.relispartition
   AND n.nspname NOT LIKE 'pg\_%'
   AND n.nspname <> ALL($1)
 ORDER BY n.nspname, c.relname`

const columnsQuery = `
SELECT a.attrelid, a.attname, format_type(a.atttypid, a.atttypmod), NOT a.attnotnull,
       col_description(a.attrelid, a.attnum)
  FROM pg_attribute a
 WHERE a.attrelid = ANY($1) AND a.attnum > 0 AND NOT a.attisdropped
 ORDER BY a.attrelid, a.attnum`

// Introspect lists conn's database. conn must be connected to a workspace database.
func Introspect(ctx context.Context, conn *pgx.Conn) ([]Relation, error) {
	rows, err := conn.Query(ctx, relationsQuery, ExcludedSchemas)
	if err != nil {
		return nil, fmt.Errorf("listing relations: %w", err)
	}
	var rels []Relation
	var oids []uint32
	byOID := map[uint32]int{}
	for rows.Next() {
		var oid uint32
		var r Relation
		var kind string
		if err := rows.Scan(&oid, &r.Schema, &r.Name, &kind, &r.Comment, &r.ApproxRows); err != nil {
			rows.Close()
			return nil, err
		}
		r.Kind = kindOf(kind)
		r.Columns = []Column{}
		byOID[oid] = len(rels)
		oids = append(oids, oid)
		rels = append(rels, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing relations: %w", err)
	}
	if len(rels) == 0 {
		return []Relation{}, nil
	}

	rows, err = conn.Query(ctx, columnsQuery, oids)
	if err != nil {
		return nil, fmt.Errorf("listing columns: %w", err)
	}
	for rows.Next() {
		var oid uint32
		var c Column
		if err := rows.Scan(&oid, &c.Name, &c.Type, &c.Nullable, &c.Comment); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := byOID[oid]; ok {
			rels[i].Columns = append(rels[i].Columns, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing columns: %w", err)
	}
	for i := range rels {
		rels[i].SchemaHash = SchemaHash(rels[i])
	}
	return rels, nil
}

func kindOf(relkind string) Kind {
	switch relkind {
	case "v":
		return KindView
	case "m":
		return KindMaterializedView
	default: // 'r', 'p'
		return KindTable
	}
}

// SchemaHash is a hex SHA-256 over the parts of a relation that describe its shape: kind,
// comment, and the ordered columns. Schema and name are the relation's identity, not its shape
// (a rename is a delete and a create), and ApproxRows is deliberately left out.
func SchemaHash(r Relation) string {
	shape := struct {
		Kind    Kind     `json:"kind"`
		Comment *string  `json:"comment"`
		Columns []Column `json:"columns"`
	}{r.Kind, r.Comment, r.Columns}
	b, _ := json.Marshal(shape) // cannot fail: strings, bools and pointers only
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// QuoteIdent quotes one identifier for PostgreSQL: wrapped in double quotes, with embedded double
// quotes doubled. Every schema and table name a caller supplies reaches SQL only through this,
// never by concatenation of the raw value.
func QuoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// QualifiedName is schema.name, each part quoted.
func QualifiedName(schema, name string) string { return QuoteIdent(schema) + "." + QuoteIdent(name) }

// MaxIdentifierBytes is PostgreSQL's identifier limit (NAMEDATALEN - 1).
const MaxIdentifierBytes = 63

// ValidIdentifier reports whether s can name an existing schema or relation: non-empty, at most
// 63 bytes, and free of NUL (which PostgreSQL can't store in a name). Any other character is
// allowed, because QuoteIdent makes it safe; a name that doesn't exist simply isn't found.
func ValidIdentifier(s string) bool {
	return s != "" && len(s) <= MaxIdentifierBytes && !strings.ContainsRune(s, 0)
}
