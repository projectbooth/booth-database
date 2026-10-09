package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/projectbooth/booth-database/internal/auth"
	"github.com/projectbooth/booth-database/internal/auth/authtest"
)

func TestVerifier(t *testing.T) {
	idp := authtest.New(t)
	other := authtest.New(t) // e.g. booth-core's workload issuer: a real, but untrusted, issuer
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	ctx := context.Background()

	lax, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database"})
	if err != nil {
		t.Fatal(err)
	}
	strict, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database", RequireAudience: true})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database", GroupsClaim: "roles"})
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name       string
		v          *auth.Verifier
		raw        string
		wantErr    bool
		wantGroups int
	}{
		{"valid", lax, idp.Mint(t, authtest.Token{Subject: "alice", Groups: []string{"/workspaces/acme/owner"}}), false, 1},
		{"expired", lax, idp.Mint(t, authtest.Token{Subject: "alice", Expiry: -time.Hour}), true, 0},
		{"signed by an unknown key", lax, idp.Mint(t, authtest.Token{Subject: "alice", SignWith: otherKey}), true, 0},
		{"claims another issuer", lax, idp.Mint(t, authtest.Token{Subject: "alice", Issuer: "https://evil.example"}), true, 0},
		// A genuine token from a different real issuer (as a workload token would be) is refused:
		// this API trusts the OIDC provider only.
		{"another real issuer", lax, other.Mint(t, authtest.Token{Subject: "job:1", Groups: []string{"/workspaces/acme/owner"}}), true, 0},
		{"missing subject", lax, idp.Mint(t, authtest.Token{}), true, 0},
		{"not a JWT", lax, "garbage", true, 0},
		{"audience required and matching", strict, idp.Mint(t, authtest.Token{Subject: "alice", Audience: "booth-database"}), false, 0},
		{"audience required but wrong", strict, idp.Mint(t, authtest.Token{Subject: "alice", Audience: "someone-else"}), true, 0},
		{"groups claim of the wrong shape fails closed", lax, idp.Mint(t, authtest.Token{Subject: "alice", Groups: "/workspaces/acme/owner"}), false, 0},
		{"configurable groups claim", custom, idp.Mint(t, authtest.Token{Subject: "alice", Groups: []string{"/workspaces/acme/owner"}, GroupsClaim: "roles"}), false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := tc.v.Verify(ctx, tc.raw)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err == nil && len(c.Groups) != tc.wantGroups {
				t.Errorf("groups = %v", c.Groups)
			}
		})
	}
}

func TestRoleInWorkspaceAndMemberships(t *testing.T) {
	groups := []string{"/workspaces/acme/viewer", "/workspaces/acme/owner", "/workspaces/globex/editor", "/workspaces/Bad/owner", "/other/x", "/workspaces/acme/admin"}
	if r := auth.RoleInWorkspace(groups, "acme"); r != auth.RoleOwner {
		t.Errorf("acme = %q, want the highest (owner)", r)
	}
	if r := auth.RoleInWorkspace(groups, "initech"); r != "" {
		t.Errorf("initech = %q, want none", r)
	}
	m := auth.Memberships(groups)
	if len(m) != 2 || m["acme"] != auth.RoleOwner || m["globex"] != auth.RoleEditor {
		t.Errorf("Memberships = %v", m)
	}
}

func TestMiddleware(t *testing.T) {
	idp := authtest.New(t)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database"})
	if err != nil {
		t.Fatal(err)
	}
	var got auth.Identity
	h := auth.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = auth.FromContext(r.Context())
	}))
	editor := idp.Mint(t, authtest.Token{Subject: "ed", Groups: []string{"/workspaces/acme/editor"}})
	owner := idp.Mint(t, authtest.Token{Subject: "ow", Groups: []string{"/workspaces/acme/owner"}})

	cases := []struct {
		name, token, workspace, forwardedRole string
		status                                int
		role                                  auth.Role
	}{
		{"no token", "", "acme", "", 401, ""},
		{"bad token", "x.y.z", "acme", "", 401, ""},
		{"no workspace header", owner, "", "", 400, ""},
		{"no role in that workspace", owner, "globex", "", 403, ""},
		{"token role used when no header", editor, "acme", "", 200, auth.RoleEditor},
		// ADR 0041: a header claiming more than the token grants is forged — rejected, not trimmed.
		{"forged owner header on an editor token", editor, "acme", "owner", 403, ""},
		{"header may narrow", owner, "acme", "viewer", 200, auth.RoleViewer},
		{"unknown header value grants nothing", owner, "acme", "superuser", 200, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got = auth.Identity{}
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			if tc.workspace != "" {
				req.Header.Set(auth.HeaderBoothWorkspace, tc.workspace)
			}
			if tc.forwardedRole != "" {
				req.Header.Set(auth.HeaderBoothRole, tc.forwardedRole)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if tc.status == 200 && got.Role != tc.role {
				t.Errorf("role = %q, want %q", got.Role, tc.role)
			}
		})
	}
}

func TestIsPlatformOperator(t *testing.T) {
	yes := auth.Identity{Groups: []string{"/workspaces/acme/viewer", auth.PlatformOperatorGroup}}
	if !yes.IsPlatformOperator() {
		t.Error("exact /platform/operator not recognised")
	}
	for _, groups := range [][]string{nil, {"/platform/operator/"}, {"/Platform/Operator"}, {"platform/operator"}, {"/platform/operators"}, {"/workspaces/platform/owner"}} {
		if (auth.Identity{Groups: groups}).IsPlatformOperator() {
			t.Errorf("%v counted as a platform operator", groups)
		}
	}
}

// ADR 0108's key-fetch override: keys from a plain-http JWKS URL, `iss` still checked exactly
// against the configured issuer, which is never contacted (here it isn't even reachable).
func TestVerifier_JWKSURLOverride(t *testing.T) {
	ctx := context.Background()
	keys := authtest.New(t) // serves the signing keys over plain http at keys.JWKSURL
	const issuer = "https://keycloak.unreachable.invalid/realms/booth"

	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: issuer, ClientID: "booth-database", JWKSURL: keys.JWKSURL})
	if err != nil {
		t.Fatalf("NewVerifier must not need the (unreachable) issuer: %v", err)
	}

	good := keys.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice", Groups: []string{"/workspaces/acme/owner"}})
	c, err := v.Verify(ctx, good)
	if err != nil || c.Subject != "alice" || len(c.Groups) != 1 {
		t.Fatalf("a valid token for the configured issuer: claims=%+v err=%v", c, err)
	}

	// Same key, different `iss`: still rejected. The override changes where keys come from, not
	// which issuer is trusted.
	for _, iss := range []string{keys.URL, issuer + "/", "https://keycloak.unreachable.invalid/realms/other"} {
		if _, err := v.Verify(ctx, keys.Mint(t, authtest.Token{Issuer: iss, Subject: "alice"})); err == nil {
			t.Errorf("token with iss=%q accepted", iss)
		}
	}
	// A key the JWKS doesn't publish: rejected.
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	if _, err := v.Verify(ctx, keys.Mint(t, authtest.Token{Issuer: issuer, Subject: "alice", SignWith: other})); err == nil {
		t.Error("token signed by an unpublished key accepted")
	}
	if n := keys.DiscoveryHits.Load(); n != 0 {
		t.Errorf("discovery contacted %d times with jwksUrl set", n)
	}
}

// With jwksUrl set, discovery is never contacted even when the issuer is reachable and serves it.
func TestVerifier_JWKSURLSkipsDiscovery(t *testing.T) {
	ctx := context.Background()
	idp := authtest.New(t)
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database", JWKSURL: idp.JWKSURL})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(ctx, idp.Mint(t, authtest.Token{Subject: "alice"})); err != nil {
		t.Fatalf("valid token: %v", err)
	}
	if n := idp.DiscoveryHits.Load(); n != 0 {
		t.Errorf("discovery contacted %d times with jwksUrl set", n)
	}

	// Unset: today's behaviour, discovery.
	if _, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database"}); err != nil {
		t.Fatal(err)
	}
	if idp.DiscoveryHits.Load() == 0 {
		t.Error("without jwksUrl, discovery should be used")
	}
}

func TestVerifier_JWKSURLRequiresIssuer(t *testing.T) {
	if _, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{JWKSURL: "http://keycloak.booth-system.svc:8080/realms/booth/protocol/openid-connect/certs"}); err == nil {
		t.Fatal("jwksUrl without an issuer accepted")
	}
}
