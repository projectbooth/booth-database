// Package api is booth-database's user-facing, read-only admin API (ADR 0093), reached through
// booth-core's gateway at /modules/database/api/*. Two routes, both GET, both owner-only
// (docs/decisions/0004 §2), both behind internal/auth's real token verification and ADR 0041
// role derivation:
//
//   - GET /api/status: the active workspace's own database, or "not provisioned yet".
//   - GET /api/databases: every workspace database on the server, for a platform operator
//     (ADR 0094: the verified token's groups claim contains /platform/operator) who also owns the
//     active workspace — ADR 0093's scoping is unchanged, only how an operator is identified
//     (docs/decisions/0005). Nobody else can call it.
//
// Nothing here writes anything: no route provisions, drops or alters a database (ADR 0093, and
// ADR 0089's accepted "deletion unhandled" call).
package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/projectbooth/booth-database/internal/auth"
	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/provision"
)

// StatusSource is the slice of internal/provision this API reads from.
type StatusSource interface {
	WorkspaceStatus(ctx context.Context, ws string) (provision.Status, bool, error)
	ListDatabases(ctx context.Context) ([]provision.Status, error)
}

// Deps is what the API needs.
type Deps struct {
	// Verifier returns the token verifier, or nil while it isn't available (no OIDC issuer
	// configured, or discovery hasn't succeeded yet). Nil makes every route answer 503 rather
	// than let anything through: the credential-broker path must not depend on the admin view's
	// identity provider, but the admin view must never run without one.
	Verifier func() auth.TokenVerifier
	Source   StatusSource
}

// NewHandler builds the /api/* routes.
func NewHandler(d Deps) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", d.status)
	mux.HandleFunc("GET /api/databases", d.databases)
	protected := d.requireOwner(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		v := d.Verifier()
		if v == nil {
			auth.WriteError(w, http.StatusServiceUnavailable, "the admin view isn't available: no identity provider is configured or reachable")
			return
		}
		auth.Middleware(v)(protected).ServeHTTP(w, r)
	})
}

// requireOwner runs after auth.Middleware: the view is for workspace owners only.
func (d Deps) requireOwner(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := auth.FromContext(r.Context())
		if !ok {
			auth.WriteError(w, http.StatusUnauthorized, "no identity")
			return
		}
		if !id.IsOwner() {
			auth.WriteError(w, http.StatusForbidden, "only a workspace owner can view its database status")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// isOperator: ADR 0094's /platform/operator claim, on top of this view's existing owner-only
// rule (ADR 0093 scoping unchanged). Both facts come from the verified token; with no operator
// claim the listing is refused (fail closed).
func (d Deps) isOperator(id auth.Identity) bool {
	return id.IsOwner() && id.IsPlatformOperator()
}

type statusResponse struct {
	Workspace   string            `json:"workspace"`
	Provisioned bool              `json:"provisioned"`
	Database    *provision.Status `json:"database"`
	// Operator tells the UI whether to offer the cross-workspace listing. The server enforces
	// it independently on /api/databases.
	Operator bool `json:"operator"`
}

func (d Deps) status(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	st, provisioned, err := d.Source.WorkspaceStatus(r.Context(), id.Workspace)
	if err != nil {
		log.Printf("admin api: status for workspace %s: %v", id.Workspace, err)
		auth.WriteError(w, http.StatusBadGateway, "the database server couldn't be reached")
		return
	}
	resp := statusResponse{Workspace: id.Workspace, Provisioned: provisioned, Operator: d.isOperator(id)}
	if provisioned {
		resp.Database = &st
	}
	writeJSON(w, resp)
}

type listedDatabase struct {
	provision.Status
	// Workspace is set only for a database belonging to a workspace the caller is a member of.
	// The module never stores slugs (internal/naming), so the rest stay identified by their
	// hashed name alone.
	Workspace string `json:"workspace,omitempty"`
}

func (d Deps) databases(w http.ResponseWriter, r *http.Request) {
	id, _ := auth.FromContext(r.Context())
	if !d.isOperator(id) {
		auth.WriteError(w, http.StatusForbidden, "listing every workspace's database is limited to platform operators")
		return
	}
	list, err := d.Source.ListDatabases(r.Context())
	if err != nil {
		log.Printf("admin api: listing databases: %v", err)
		auth.WriteError(w, http.StatusBadGateway, "the database server couldn't be reached")
		return
	}
	known := map[string]string{}
	for ws := range auth.Memberships(id.Groups) {
		if n, err := naming.ForWorkspace(ws); err == nil {
			known[n.Database] = ws
		}
	}
	items := make([]listedDatabase, 0, len(list))
	for _, s := range list {
		items = append(items, listedDatabase{Status: s, Workspace: known[s.Database]})
	}
	log.Printf("admin api: operator listing by sub=%s in workspace %s (%d databases)", id.Subject, id.Workspace, len(items))
	writeJSON(w, map[string]any{"items": items})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
