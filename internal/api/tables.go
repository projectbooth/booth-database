package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/projectbooth/booth-database/internal/auth"
	"github.com/projectbooth/booth-database/internal/provision"
	"github.com/projectbooth/booth-database/internal/query"
	"github.com/projectbooth/booth-database/internal/tables"
)

// TablesSource is the slice of internal/provision the Tables routes read from.
type TablesSource interface {
	IntrospectWorkspace(ctx context.Context, ws string) ([]tables.Relation, bool, error)
	RememberWorkspace(ctx context.Context, ws string) error
	// WriteAccess is every table some tablewrite grant (ADR 0117) can change, from the
	// database's live ACLs.
	WriteAccess(ctx context.Context, ws string) ([]provision.TableWriteAccess, error)
}

// QueryRunner is internal/query's Executor.
type QueryRunner interface {
	Preview(ctx context.Context, c query.Caller, schema, name string, limit int) (*query.Result, error)
	Console(ctx context.Context, c query.Caller, sql string) (*query.Result, error)
	Cancel(subject string, wait time.Duration) bool
	WriteDeadline() time.Duration
}

// requireMember runs after auth.Middleware: any role in the active workspace. Middleware already
// refuses a token with no role there; this also refuses a forwarded role it didn't recognize.
func (d Deps) requireMember(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.FromContext(r.Context())
		if !ok || id.Role == "" {
			auth.WriteError(w, http.StatusForbidden, "you have no role in this workspace")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type tablesResponse struct {
	Workspace   string           `json:"workspace"`
	Provisioned bool             `json:"provisioned"`
	Tables      []listedRelation `json:"tables"`
}

// listedRelation is a Relation as the Tables view lists it: the Catalog's fields, plus which
// APIs can write to it. The extra field is the view's only: events carry tables.Relation alone.
type listedRelation struct {
	tables.Relation
	WritableThrough []writeAccess `json:"writableThrough,omitempty"`
}

// writeAccess is one tablewrite grant's live privileges on a table, for the read-only "Writable
// through an API" badge. API is the grant id's part after "api:" when it has that form
// (booth-api's grant ids), so the badge can name it; empty otherwise.
type writeAccess struct {
	Grant      string   `json:"grant"`
	API        string   `json:"api,omitempty"`
	Operations []string `json:"operations"`
	Columns    []string `json:"columns"`
}

func (d Deps) listTables(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	rels, provisioned, err := d.Tables.IntrospectWorkspace(r.Context(), id.Workspace)
	if err != nil {
		log.Printf("api: listing tables for workspace %s: %v", id.Workspace, err)
		auth.WriteError(w, http.StatusBadGateway, "the database server couldn't be reached")
		return
	}
	listed := make([]listedRelation, len(rels))
	for i, rel := range rels {
		listed[i].Relation = rel
	}
	if provisioned {
		// Which tables an API can write to (ADR 0117). Best effort: the listing is still right
		// without it, so a failure here only drops the badges.
		access, err := d.Tables.WriteAccess(r.Context(), id.Workspace)
		if err != nil {
			log.Printf("api: reading write grants for workspace %s: %v", id.Workspace, err)
		}
		byKey := map[tables.Key][]writeAccess{}
		for _, a := range access {
			api, _ := strings.CutPrefix(a.Grant, "api:")
			if api == a.Grant {
				api = ""
			}
			k := tables.Key{Schema: a.Schema, Name: a.Name}
			byKey[k] = append(byKey[k], writeAccess{Grant: a.Grant, API: api, Operations: a.Operations, Columns: a.Columns})
		}
		for i := range listed {
			listed[i].WritableThrough = byKey[listed[i].Relation.Key()]
		}
	}
	if provisioned {
		// Opening the Tables view is also a reconcile trigger (ADR 0116 §2), so a table created
		// a minute ago reaches the Catalog without waiting for the loop.
		if err := d.Tables.RememberWorkspace(r.Context(), id.Workspace); err != nil {
			log.Printf("api: recording workspace %s for table events: %v", id.Workspace, err)
		}
		if d.Nudge != nil {
			d.Nudge(id.Workspace)
		}
	}
	writeJSON(w, tablesResponse{Workspace: id.Workspace, Provisioned: provisioned, Tables: listed})
}

func (d Deps) previewTable(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	q := r.URL.Query()
	limit := query.PreviewMaxRows
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			writeQueryError(w, &query.Error{Code: query.CodeInvalid, Message: "limit must be a number"})
			return
		}
		limit = n // clamped to 1..100 by Preview
	}
	extendWriteDeadline(w, d.Query.WriteDeadline())
	res, err := d.Query.Preview(r.Context(), caller(id), q.Get("schema"), q.Get("name"), limit)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	writeJSON(w, res)
}

type queryRequest struct {
	SQL string `json:"sql"`
}

func (d Deps) runQuery(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	var req queryRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, query.MaxSQLBytes+4096))
	dec.DisallowUnknownFields() // e.g. a "database" field is refused, never silently ignored
	if err := dec.Decode(&req); err != nil {
		writeQueryError(w, &query.Error{Code: query.CodeInvalid, Message: `the body must be {"sql": "<one statement>"}`})
		return
	}
	extendWriteDeadline(w, d.Query.WriteDeadline())
	res, err := d.Query.Console(r.Context(), caller(id), req.SQL)
	if err != nil {
		writeQueryError(w, err)
		return
	}
	writeJSON(w, res)
}

// cancelQuery cancels the caller's own running query (there is at most one, query.Limiter) and
// returns once its connection is closed and its lease dropped.
func (d Deps) cancelQuery(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if !d.Query.Cancel(id.Subject, 15*time.Second) {
		writeQueryError(w, &query.Error{Code: query.CodeNotFound, Message: "you have no running query"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func caller(id auth.Identity) query.Caller {
	return query.Caller{Subject: id.Subject, Workspace: id.Workspace}
}

// extendWriteDeadline lets a query response outlive the server's default WriteTimeout (30s, the
// same as the statement timeout). Unsupported writers (tests) are fine to ignore.
func extendWriteDeadline(w http.ResponseWriter, d time.Duration) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(d))
}

type queryErrorBody struct {
	Error    string `json:"error"`
	Code     string `json:"code"`
	SQLState string `json:"sqlState,omitempty"`
}

func writeQueryError(w http.ResponseWriter, err error) {
	var qe *query.Error
	if !errors.As(err, &qe) {
		qe = &query.Error{Code: query.CodeUnavailable, Message: "the query failed"}
	}
	status := http.StatusUnprocessableEntity // a SQL error, timeout or cancel: the query's outcome
	switch qe.Code {
	case query.CodeEmpty, query.CodeInvalid, query.CodeTooLarge, query.CodeMultipleStatements:
		status = http.StatusBadRequest
	case query.CodeBusy:
		status = http.StatusConflict
	case query.CodeNotProvisioned, query.CodeNotFound:
		status = http.StatusNotFound
	case query.CodeUnavailable:
		status = http.StatusBadGateway
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(queryErrorBody{Error: qe.Message, Code: qe.Code, SQLState: qe.SQLState})
}
