// Package credentialbroker is booth-database's provider side of the ADR 0080 credential broker
// (contracts/credential-broker.md, mechanism fixed by ADR 0088): booth-core routes an
// already-authorized `postgres`-kind request here, and this module mints a short-lived login
// role good for exactly the requester's workspace database (internal/provision) — never this
// module's own admin credential.
//
// Structure and authentication deliberately mirror booth-storage's s3-kind provider
// (booth-storage/internal/credentialbroker/provider.go), the fleet's first verified provider:
// core presents the finished credential it delivered as the
// booth-credential-broker-provider-credentials Secret, and this side does a constant-time
// comparison against its own copy — it never holds core's HMAC key.
//
// The requester's workspace comes from the broker-forwarded `requester.workspace` (already
// authorized by core), never from anything the requester chose: the `postgres` scope can name a
// workspace only to have it checked against that value (docs/decisions/0001 §5).
package credentialbroker

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/provision"
)

// ProviderPath is the fixed path every credential-broker provider implements
// (contracts/credential-broker.md; core's own credentialbroker.ProviderPath).
const ProviderPath = "/internal/credentials"

const (
	kindPostgres    = "postgres"
	accessRead      = "read"
	accessReadWrite = "readwrite"

	// maxRequestBytes bounds a provider request body; a legitimate one is a few short strings.
	maxRequestBytes = 16 << 10
)

// Issuer is what the handler needs from internal/provision. An interface so the handler's
// parsing, validation and response envelope are testable without a server; production always
// passes *provision.Provisioner.
type Issuer interface {
	Issue(ctx context.Context, ws, leaseID string, readWrite bool, ttl time.Duration) (provision.Lease, error)
}

// Endpoint is where a requester connects — the host/port a workload pod can reach, which may
// differ from the address this module's own admin connection uses (docs/decisions/0001 §6).
type Endpoint struct {
	Host    string
	Port    int
	SSLMode string
}

// Deps is what the provider handler needs.
type Deps struct {
	// Credential is this module's copy of the secret core presents as `Authorization: Bearer`.
	// Empty refuses every request: there is no "unconfigured, so allow" state for this route.
	Credential string
	Issuer     Issuer
	Endpoint   Endpoint
	// MinTTL is this provider's documented floor on a lease's lifetime (ADR 0089 §3): a request
	// asking for less — which, under the broker's 5-minute ceiling (ADR 0088), is every request —
	// is clamped UP to it. Same mechanism as booth-storage's MinIO provider (a 15-minute floor it
	// can't go below); here the floor is a usability limit rather than a technical one, because a
	// session dies when its credential expires (the reaper) and five minutes is shorter than a
	// real pipeline task or notebook work block. Response.ExpiresAt carries the real expiry, so
	// booth-core's audit trail records what was actually issued. <= 0 means DefaultMinTTL.
	MinTTL time.Duration
	// MaxTTL caps a lease's lifetime on this side, independently of the broker's ceiling —
	// defense in depth if core is ever configured looser. <= 0 means MinTTL (i.e. every lease
	// lives exactly the floor); a value below MinTTL is raised to it.
	MaxTTL time.Duration
	// NewLeaseID is overridden in tests.
	NewLeaseID func() string
}

// DefaultMinTTL is the lease-lifetime floor (docs/decisions/0003 for the reasoning): one hour —
// booth-pipeline's default task timeout (DEFAULT_TIMEOUT_SECONDS = 3600) and booth-notebooks'
// default idle-cull window (cullIdleSeconds: 3600), so a default-configured task can hold one
// connection for its whole run and a notebook connection lasts as long as the server it lives in
// would stay up idle.
//
// It is also the hard bound on any one session: the reaper ends a session when the lease it logged
// in with expires, even if a newer lease has since been issued (ADR 0095 fifth amendment upholds
// that). booth-core's credential sidecar derives its connection guarantee from this value (about
// half of it, once its half-lifetime renewal ships). DO NOT change this default without telling the
// architecture coordinator first: consumers' guarantees move with it.
const DefaultMinTTL = time.Hour

// NewHandler builds the provider endpoint. Not behind any OIDC middleware: the only caller is
// booth-core, authenticated by the shared provider credential, and it never comes through the
// gateway.
func NewHandler(deps Deps) http.Handler {
	if deps.MinTTL <= 0 {
		deps.MinTTL = DefaultMinTTL
	}
	if deps.MaxTTL < deps.MinTTL {
		deps.MaxTTL = deps.MinTTL
	}
	if deps.NewLeaseID == nil {
		deps.NewLeaseID = uuid.NewString
	}
	return &provider{deps}
}

type provider struct{ Deps }

// ---- wire shapes -------------------------------------------------------------------------
//
// Top-level fields mirror core's providerRequest/Response (ADR 0088). The postgres-kind
// scope/options/credential shapes are this module's to define (the contract leaves them opaque
// per kind), documented in README.md and docs/decisions/0001 §5.

type inboundRequest struct {
	Kind       string          `json:"kind"`
	TTLSeconds int             `json:"ttlSeconds"`
	Access     string          `json:"access"`
	Scope      json.RawMessage `json:"scope"`
	Options    json.RawMessage `json:"options,omitempty"`
	Requester  requester       `json:"requester"`
}

type requester struct {
	Subject   string `json:"subject"`
	Workspace string `json:"workspace"`
	Role      string `json:"role"`
}

// pgScope is the postgres kind's `scope`. A workspace database is the only thing there is to
// scope to in v0, and which workspace is already fixed by the broker, so Workspace is optional
// — present only so a caller can state what it expects and have a mismatch refused rather than
// silently served for a different workspace.
type pgScope struct {
	Workspace string `json:"workspace,omitempty"`
}

// pgScopeEcho is echoed back so a client can check it got what it asked for, the same
// "don't trust an unchecked echo" discipline booth-lakehouse's client applies to s3 grants.
type pgScopeEcho struct {
	Workspace string `json:"workspace"`
	Database  string `json:"database"`
	Access    string `json:"access"`
}

// pgCredential is the minted credential: everything a libpq-style client needs, and nothing
// about this module's own admin connection.
type pgCredential struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Database string `json:"database"`
	Username string `json:"username"`
	Password string `json:"password"`
	SSLMode  string `json:"sslMode"`
}

type outboundResponse struct {
	LeaseID    string       `json:"leaseId"`
	Kind       string       `json:"kind"`
	ExpiresAt  time.Time    `json:"expiresAt"`
	Scope      pgScopeEcho  `json:"scope"`
	Credential pgCredential `json:"credential"`
}

type errorBody struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

// ---- handler -------------------------------------------------------------------------

func (p *provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // a 2xx body carries a live credential

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "")
		return
	}
	if !p.authenticate(r) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}

	var req inboundRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid request body: "+err.Error())
		return
	}

	if req.Kind != kindPostgres {
		writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", fmt.Sprintf("this provider only issues %q credentials", kindPostgres))
		return
	}
	if req.Access != accessRead && req.Access != accessReadWrite {
		writeError(w, http.StatusBadRequest, "invalid_request", fmt.Sprintf("access must be %q or %q", accessRead, accessReadWrite))
		return
	}
	ws := req.Requester.Workspace
	if !naming.ValidWorkspace(ws) {
		writeError(w, http.StatusBadRequest, "invalid_request", "requester.workspace is missing or not a workspace slug")
		return
	}

	var scope pgScope
	if len(req.Scope) > 0 {
		sd := json.NewDecoder(strings.NewReader(string(req.Scope)))
		sd.DisallowUnknownFields()
		if err := sd.Decode(&scope); err != nil {
			// An unrecognised scope field might have been meant to narrow the grant (a schema, a
			// table): refuse rather than silently ignore it and hand out something broader.
			writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", "the postgres scope accepts only an optional \"workspace\": "+err.Error())
			return
		}
	}
	if scope.Workspace != "" && scope.Workspace != ws {
		writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", "scope.workspace must be the workspace the request was made in")
		return
	}
	if len(req.Options) > 0 && strings.TrimSpace(string(req.Options)) != "{}" && strings.TrimSpace(string(req.Options)) != "null" {
		writeError(w, http.StatusUnprocessableEntity, "scope_not_supported", "the postgres kind takes no options")
		return
	}

	ttl := time.Duration(req.TTLSeconds) * time.Second
	if ttl > p.MaxTTL {
		ttl = p.MaxTTL
	}
	if ttl < p.MinTTL { // also covers an absent/zero ttlSeconds
		ttl = p.MinTTL
	}

	leaseID := p.NewLeaseID()
	lease, err := p.Issuer.Issue(r.Context(), ws, leaseID, req.Access == accessReadWrite, ttl)
	if err != nil {
		log.Printf("credential broker: minting a %s postgres lease for workspace %s: %v", req.Access, ws, err)
		writeError(w, http.StatusBadGateway, "mint_failed", "the workspace database could not be reached to mint a credential")
		return
	}

	// This module's half of ADR 0080's audit requirement (core keeps the authoritative,
	// append-only trail): who, what, when, expiry — never the password. The lease role name is
	// derived from the lease id, so PostgreSQL's own logs join up with this line.
	log.Printf("audit: credential issued lease=%s kind=postgres subject=%s workspace=%s role=%s database=%s dbrole=%s access=%s expiresAt=%s",
		leaseID, req.Requester.Subject, ws, req.Requester.Role, lease.Database, lease.Username, req.Access, lease.ExpiresAt.UTC().Format(time.RFC3339))

	writeJSON(w, http.StatusCreated, outboundResponse{
		LeaseID: leaseID, Kind: kindPostgres, ExpiresAt: lease.ExpiresAt,
		Scope: pgScopeEcho{Workspace: ws, Database: lease.Database, Access: req.Access},
		Credential: pgCredential{
			Host: p.Endpoint.Host, Port: p.Endpoint.Port, Database: lease.Database,
			Username: lease.Username, Password: lease.Password, SSLMode: p.Endpoint.SSLMode,
		},
	})
}

func (p *provider) authenticate(r *http.Request) bool {
	presented := bearerToken(r)
	if presented == "" || p.Credential == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(p.Credential)) == 1
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}
