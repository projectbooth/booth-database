// Package auth verifies callers of booth-database's user-facing admin API (ADR 0093) — the first
// route in this module reached with a person's token rather than booth-core's provider credential.
//
// Deliberately a copy of booth-storage's internal/auth, the fleet's reference implementation of
// ADR 0041, rather than a fresh design: the bearer token is re-verified here against the same OIDC
// provider booth-core uses (signature via JWKS, issuer, expiry, and audience when configured), and
// the caller's role is re-derived from the verified token's own groups claim (ADR 0025). The
// gateway-forwarded X-Booth-Role header can only narrow that role, never widen it, and a header
// claiming more than the token grants is rejected outright with 403.
//
// One deliberate difference from booth-storage: this package trusts the OIDC issuer ONLY, never
// booth-core's workload-token issuer (ADR 0056). This API is an admin view for people; a pipeline
// run's workload token can legitimately carry the owner role and has no business opening it.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	oidc "github.com/coreos/go-oidc/v3/oidc"
)

// Header names the gateway forwards to a backing module (ADR 0025).
const (
	HeaderBoothWorkspace = "X-Booth-Workspace"
	HeaderBoothRole      = "X-Booth-Role"
)

// Role is a caller's workspace role (ADR 0025).
type Role string

const (
	RoleOwner  Role = "owner"
	RoleEditor Role = "editor"
	RoleViewer Role = "viewer"
)

// Claims is the subset of a verified token this module cares about.
type Claims struct {
	Subject string
	// Groups is the token's workspace-membership claim (ADR 0025), e.g. "/workspaces/acme/owner".
	Groups []string
}

// TokenVerifier verifies a raw bearer token. *Verifier is the production implementation; the
// interface exists so the HTTP layer can be tested without a live provider.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (*Claims, error)
}

// OIDCConfig is the identity-provider configuration — the same shape as booth-core's, so every
// module verifies against the same provider.
type OIDCConfig struct {
	IssuerURL       string
	ClientID        string
	RequireAudience bool
	// GroupsClaim names the claim carrying workspace memberships; must match booth-core's.
	// Empty means "groups".
	GroupsClaim string
}

// DefaultGroupsClaim matches booth-core's default (ADR 0025).
const DefaultGroupsClaim = "groups"

// Verifier verifies bearer tokens against booth-core's OIDC provider.
type Verifier struct {
	verifier    *oidc.IDTokenVerifier
	groupsClaim string
}

// NewVerifier runs OIDC discovery against the issuer.
func NewVerifier(ctx context.Context, cfg OIDCConfig) (*Verifier, error) {
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery against %s: %w", cfg.IssuerURL, err)
	}
	claim := cfg.GroupsClaim
	if claim == "" {
		claim = DefaultGroupsClaim
	}
	return &Verifier{
		verifier:    provider.Verifier(&oidc.Config{SkipClientIDCheck: !cfg.RequireAudience, ClientID: cfg.ClientID}),
		groupsClaim: claim,
	}, nil
}

// Verify checks signature, issuer, expiry and (when configured) audience, then reads the groups
// claim. A token from any other issuer — including booth-core's workload issuer — fails here.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Claims, error) {
	idToken, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("token verification failed: %w", err)
	}
	if idToken.Subject == "" {
		return nil, fmt.Errorf("token missing subject")
	}
	var raw map[string]json.RawMessage
	if err := idToken.Claims(&raw); err != nil {
		return nil, fmt.Errorf("reading token claims: %w", err)
	}
	var groups []string
	if g, ok := raw[v.groupsClaim]; ok {
		// A claim of the wrong shape is "no groups" (fail closed), not an error.
		_ = json.Unmarshal(g, &groups)
	}
	return &Claims{Subject: idToken.Subject, Groups: groups}, nil
}

// groupRE is ADR 0025's workspace-membership group shape.
var groupRE = regexp.MustCompile(`^/workspaces/([a-z0-9-]+)/(owner|editor|viewer)$`)

func rank(r Role) int {
	switch r {
	case RoleOwner:
		return 3
	case RoleEditor:
		return 2
	case RoleViewer:
		return 1
	}
	return 0
}

// RoleInWorkspace returns the highest role the groups grant in workspace, or "" if none.
func RoleInWorkspace(groups []string, workspace string) Role {
	var best Role
	for _, g := range groups {
		m := groupRE.FindStringSubmatch(g)
		if m == nil || m[1] != workspace {
			continue
		}
		if r := Role(m[2]); rank(r) > rank(best) {
			best = r
		}
	}
	return best
}

// Memberships returns every workspace the groups name, with the highest role in each. Used only
// to label an operator's cross-workspace listing with slugs the caller already belongs to — the
// module never stores slugs itself (internal/naming).
func Memberships(groups []string) map[string]Role {
	out := map[string]Role{}
	for _, g := range groups {
		if m := groupRE.FindStringSubmatch(g); m != nil && rank(Role(m[2])) > rank(out[m[1]]) {
			out[m[1]] = Role(m[2])
		}
	}
	return out
}

// EffectiveRole is never stronger than the token's grant, nor than the forwarded role (a gateway
// may narrow, never widen). An absent forwarded role means "use the token's"; an unrecognized one
// yields "" — no access.
func EffectiveRole(forwarded, granted Role) Role {
	if forwarded == "" {
		return granted
	}
	if rank(forwarded) == 0 {
		return ""
	}
	if rank(forwarded) < rank(granted) {
		return forwarded
	}
	return granted
}

// Identity is the caller identity attached to a request's context by Middleware.
type Identity struct {
	Subject   string
	Workspace string
	Role      Role
	// Groups is kept for Memberships; never used to widen Role.
	Groups []string
}

// IsOwner reports whether the caller owns the active workspace — this module's bar for the admin
// view (docs/decisions/0004 §2).
func (i Identity) IsOwner() bool { return i.Role == RoleOwner }

// PlatformOperatorGroup is ADR 0094's platform-operator claim: a property of the person, read from
// the same verified groups claim as workspace roles, never from a forwarded header.
const PlatformOperatorGroup = "/platform/operator"

// IsPlatformOperator reports whether the verified token's groups contain exactly
// PlatformOperatorGroup. Exact match only: no prefix, case or trailing-slash variants.
func (i Identity) IsPlatformOperator() bool {
	for _, g := range i.Groups {
		if g == PlatformOperatorGroup {
			return true
		}
	}
	return false
}

type contextKey struct{}

// FromContext returns the identity Middleware attached, if any.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(contextKey{}).(Identity)
	return id, ok
}

// WithIdentity attaches an identity the way Middleware does — for handler tests.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// Middleware verifies the bearer token, reads the gateway-forwarded workspace, and derives the
// caller's role there from the token itself (ADR 0041).
func Middleware(verifier TokenVerifier) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := bearerToken(r)
			if token == "" {
				WriteError(w, http.StatusUnauthorized, "missing bearer token")
				return
			}
			claims, err := verifier.Verify(r.Context(), token)
			if err != nil {
				WriteError(w, http.StatusUnauthorized, "invalid token")
				return
			}
			workspace := r.Header.Get(HeaderBoothWorkspace)
			if workspace == "" {
				WriteError(w, http.StatusBadRequest, "missing "+HeaderBoothWorkspace+" header")
				return
			}
			granted := RoleInWorkspace(claims.Groups, workspace)
			if granted == "" {
				WriteError(w, http.StatusForbidden, "your token grants no role in this workspace")
				return
			}
			forwarded := Role(r.Header.Get(HeaderBoothRole))
			if rank(forwarded) > rank(granted) {
				log.Printf("auth: rejected: forwarded role %q exceeds token-derived role %q for sub=%s workspace=%s", forwarded, granted, claims.Subject, workspace)
				WriteError(w, http.StatusForbidden, "the forwarded role exceeds what your token grants in this workspace")
				return
			}
			next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), Identity{
				Subject: claims.Subject, Workspace: workspace, Role: EffectiveRole(forwarded, granted), Groups: claims.Groups,
			})))
		})
	}
}

// WriteError writes the JSON error body used across this module's user-facing API.
func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) < len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}
