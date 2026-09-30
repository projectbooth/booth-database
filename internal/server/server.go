// Package server assembles booth-database's HTTP surface. It is deliberately small: the only way
// anything reaches a workspace database is a credential minted through booth-core's broker (the
// provider path), plus the read-only admin API behind the native view (ADR 0093, /api/*), which
// reports status and never changes anything.
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/projectbooth/booth-database/internal/credentialbroker"
)

// Pinger is the slice of the provisioner /healthz needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Deps is what the router needs.
type Deps struct {
	DB Pinger
	// Ready flips true once startup preparation (e.g. RestrictMaintenanceAccess) has succeeded;
	// until then /healthz reports unready so a half-prepared server is never routed to.
	Ready    *atomic.Bool
	Provider http.Handler
	// API is the read-only admin API (ADR 0093, internal/api), mounted at /api/.
	API http.Handler
}

// NewRouter builds the HTTP handler.
func NewRouter(d Deps) http.Handler {
	mux := http.NewServeMux()

	// /livez never touches the database: restarting the pod can't fix a Postgres outage.
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	// /healthz is readiness and what booth-core polls (the manifest's healthCheckPath).
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := d.DB.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "reason": "database unreachable"})
			return
		}
		if !d.Ready.Load() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "starting"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.Handle(credentialbroker.ProviderPath, d.Provider)
	if d.API != nil {
		mux.Handle("/api/", d.API)
	}
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
