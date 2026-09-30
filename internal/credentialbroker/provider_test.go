package credentialbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/projectbooth/booth-database/internal/provision"
)

const (
	testCredential = "bcbp.database.test-credential"
	testLeaseID    = "11111111-2222-3333-4444-555555555555"
	testPassword   = "s3cr3t-password-value"
)

type fakeIssuer struct {
	calls []issueCall
	err   error
}

type issueCall struct {
	ws, leaseID string
	readWrite   bool
	ttl         time.Duration
}

func (f *fakeIssuer) Issue(_ context.Context, ws, leaseID string, readWrite bool, ttl time.Duration) (provision.Lease, error) {
	f.calls = append(f.calls, issueCall{ws, leaseID, readWrite, ttl})
	if f.err != nil {
		return provision.Lease{}, f.err
	}
	return provision.Lease{
		Database: "bdb_ws_0123456789abcdef01234567", Username: "bdb_lease_11111111222233334444555555555555",
		Password: testPassword, ExpiresAt: time.Date(2026, 9, 29, 12, 5, 0, 0, time.UTC),
	}, nil
}

func newTestHandler(f *fakeIssuer, credential string) http.Handler {
	return NewHandler(Deps{
		Credential: credential, Issuer: f,
		Endpoint:   Endpoint{Host: "booth-database-postgres.booth-database.svc", Port: 5432, SSLMode: "disable"},
		NewLeaseID: func() string { return testLeaseID },
	})
}

// validBody is exactly what booth-core's credentialbroker.Service sends a provider (its
// providerRequest), for a postgres request.
func validBody(overrides map[string]any) []byte {
	body := map[string]any{
		"kind": "postgres", "ttlSeconds": 300, "access": "readwrite",
		"scope":     map[string]any{"workspace": "acme"},
		"requester": map[string]any{"subject": "user-123", "workspace": "acme", "role": "editor"},
	}
	for k, v := range overrides {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	b, _ := json.Marshal(body)
	return b
}

func do(t *testing.T, h http.Handler, bearer string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, ProviderPath, bytes.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body isn't JSON: %s", rec.Body)
	}
	return e.Error
}

func TestAuthentication(t *testing.T) {
	cases := []struct {
		name, configured, presented string
	}{
		{"no bearer", testCredential, ""},
		{"wrong bearer", testCredential, "bcbp.database.wrong"},
		{"another module's credential", testCredential, "bcbp.storage.test-credential"},
		// An unconfigured provider must never match on two empty strings.
		{"unconfigured provider, empty bearer", "", ""},
		{"unconfigured provider, any bearer", "", "anything"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeIssuer{}
			rec := do(t, newTestHandler(f, tc.configured), tc.presented, validBody(nil))
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status %d, want 401", rec.Code)
			}
			if len(f.calls) != 0 {
				t.Fatal("an unauthenticated request reached the issuer")
			}
		})
	}
}

func TestSuccess_ResponseShape(t *testing.T) {
	f := &fakeIssuer{}
	rec := do(t, newTestHandler(f, testCredential), testCredential, validBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("a response carrying a live credential must be Cache-Control: no-store")
	}

	// Decode into exactly the fields core's credentialbroker.Response requires (leaseId,
	// expiresAt, credential non-empty — core rejects the response otherwise).
	var resp struct {
		LeaseID    string            `json:"leaseId"`
		Kind       string            `json:"kind"`
		ExpiresAt  time.Time         `json:"expiresAt"`
		Scope      map[string]string `json:"scope"`
		Credential map[string]any    `json:"credential"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.LeaseID != testLeaseID || resp.Kind != "postgres" || resp.ExpiresAt.IsZero() {
		t.Fatalf("envelope = %+v", resp)
	}
	wantScope := map[string]string{"workspace": "acme", "database": "bdb_ws_0123456789abcdef01234567", "access": "readwrite"}
	for k, v := range wantScope {
		if resp.Scope[k] != v {
			t.Errorf("scope.%s = %q, want %q", k, resp.Scope[k], v)
		}
	}
	for _, k := range []string{"host", "port", "database", "username", "password", "sslMode"} {
		if _, ok := resp.Credential[k]; !ok {
			t.Errorf("credential is missing %q", k)
		}
	}
	if resp.Credential["password"] != testPassword || resp.Credential["host"] != "booth-database-postgres.booth-database.svc" {
		t.Errorf("credential = %v", resp.Credential)
	}

	if len(f.calls) != 1 {
		t.Fatalf("issuer called %d times", len(f.calls))
	}
	if c := f.calls[0]; c.ws != "acme" || c.leaseID != testLeaseID || !c.readWrite || c.ttl != 5*time.Minute {
		t.Errorf("issuer called with %+v", c)
	}
}

func TestAccessAndTTLPassThrough(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		readWrite bool
		ttl       time.Duration
	}{
		{"read", map[string]any{"access": "read"}, false, 5 * time.Minute},
		{"short ttl honoured exactly", map[string]any{"ttlSeconds": 30}, true, 30 * time.Second},
		{"over the provider's own cap is clamped", map[string]any{"ttlSeconds": 3600}, true, 5 * time.Minute},
		{"absent ttl gets the cap", map[string]any{"ttlSeconds": nil}, true, 5 * time.Minute},
		{"scope may omit workspace", map[string]any{"scope": map[string]any{}}, true, 5 * time.Minute},
		{"empty options are fine", map[string]any{"options": map[string]any{}}, true, 5 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeIssuer{}
			rec := do(t, newTestHandler(f, testCredential), testCredential, validBody(tc.overrides))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status %d: %s", rec.Code, rec.Body)
			}
			if c := f.calls[0]; c.readWrite != tc.readWrite || c.ttl != tc.ttl {
				t.Errorf("issuer got readWrite=%v ttl=%v, want %v %v", c.readWrite, c.ttl, tc.readWrite, tc.ttl)
			}
		})
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name      string
		overrides map[string]any
		status    int
		code      string
	}{
		{"wrong kind", map[string]any{"kind": "s3"}, 422, "scope_not_supported"},
		{"bad access", map[string]any{"access": "admin"}, 400, "invalid_request"},
		{"missing workspace", map[string]any{"requester": map[string]any{"subject": "u", "role": "owner"}}, 400, "invalid_request"},
		{"malformed workspace", map[string]any{"requester": map[string]any{"subject": "u", "workspace": "../x", "role": "owner"}}, 400, "invalid_request"},
		// The request names another workspace than the one core authorized it in.
		{"scope for another workspace", map[string]any{"scope": map[string]any{"workspace": "globex"}}, 422, "scope_not_supported"},
		// An unknown scope field might have been meant to narrow; never silently ignore it.
		{"narrower scope than supported", map[string]any{"scope": map[string]any{"workspace": "acme", "schema": "reporting"}}, 422, "scope_not_supported"},
		{"unknown option", map[string]any{"options": map[string]any{"readReplica": true}}, 422, "scope_not_supported"},
		{"unknown top-level field", map[string]any{"extra": 1}, 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeIssuer{}
			rec := do(t, newTestHandler(f, testCredential), testCredential, validBody(tc.overrides))
			if rec.Code != tc.status || errorCode(t, rec) != tc.code {
				t.Fatalf("got %d %s, want %d %s: %s", rec.Code, errorCode(t, rec), tc.status, tc.code, rec.Body)
			}
			if len(f.calls) != 0 {
				t.Fatal("a refused request still reached the issuer")
			}
		})
	}
}

func TestMintFailureIsBadGateway(t *testing.T) {
	f := &fakeIssuer{err: errors.New("connection refused")}
	rec := do(t, newTestHandler(f, testCredential), testCredential, validBody(nil))
	if rec.Code != http.StatusBadGateway || errorCode(t, rec) != "mint_failed" {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Error("internal error detail leaked to the requester")
	}
}

func TestMethodNotAllowed(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, ProviderPath, nil)
	req.Header.Set("Authorization", "Bearer "+testCredential)
	rec := httptest.NewRecorder()
	newTestHandler(&fakeIssuer{}, testCredential).ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET got %d", rec.Code)
	}
}

func TestAuditLogNeverContainsTheCredential(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	rec := do(t, newTestHandler(&fakeIssuer{}, testCredential), testCredential, validBody(nil))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status %d", rec.Code)
	}
	out := buf.String()
	if !strings.Contains(out, "audit: credential issued lease="+testLeaseID) || !strings.Contains(out, "subject=user-123") {
		t.Errorf("audit line missing or incomplete: %q", out)
	}
	if strings.Contains(out, testPassword) || strings.Contains(out, testCredential) {
		t.Errorf("a secret reached the log: %q", out)
	}
}
