package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/api"
	"github.com/projectbooth/booth-database/internal/auth"
	"github.com/projectbooth/booth-database/internal/auth/authtest"
	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/provision"
)

type fakeSource struct {
	status      map[string]provision.Status
	list        []provision.Status
	err         error
	listCalls   int
	statusCalls int
}

func (f *fakeSource) WorkspaceStatus(_ context.Context, ws string) (provision.Status, bool, error) {
	f.statusCalls++
	if f.err != nil {
		return provision.Status{}, false, f.err
	}
	st, ok := f.status[ws]
	return st, ok, nil
}

func (f *fakeSource) ListDatabases(context.Context) ([]provision.Status, error) {
	f.listCalls++
	return f.list, f.err
}

func dbName(t *testing.T, ws string) string {
	n, err := naming.ForWorkspace(ws)
	if err != nil {
		t.Fatal(err)
	}
	return n.Database
}

type harness struct {
	idp *authtest.IdP
	h   http.Handler
	src *fakeSource
}

func newHarness(t *testing.T, src *fakeSource, operators ...string) harness {
	t.Helper()
	idp := authtest.New(t)
	v, err := auth.NewVerifier(context.Background(), auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database"})
	if err != nil {
		t.Fatal(err)
	}
	h := api.NewHandler(api.Deps{Verifier: func() auth.TokenVerifier { return v }, Source: src, OperatorWorkspaces: operators})
	return harness{idp, h, src}
}

func (hs harness) get(t *testing.T, path, workspace string, groups []string, forwardedRole string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+hs.idp.Mint(t, authtest.Token{Subject: "user-1", Groups: groups}))
	req.Header.Set(auth.HeaderBoothWorkspace, workspace)
	if forwardedRole != "" {
		req.Header.Set(auth.HeaderBoothRole, forwardedRole)
	}
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec, body
}

func TestStatus_OwnerOnly(t *testing.T) {
	created := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	src := &fakeSource{status: map[string]provision.Status{"acme": {Database: dbName(t, "acme"), CreatedAt: &created, SizeBytes: 8 << 20}}}
	hs := newHarness(t, src)

	for _, tc := range []struct {
		name, role, forwarded string
		status                int
	}{
		{"owner", "owner", "", 200},
		{"editor", "editor", "", 403},
		{"viewer", "viewer", "", 403},
		// ADR 0041: an editor's token with a forged owner header must not get in.
		{"editor with a forged owner header", "editor", "owner", 403},
		// The gateway may narrow an owner; the narrowed role is what counts.
		{"owner narrowed to editor by the gateway", "owner", "editor", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec, body := hs.get(t, "/api/status", "acme", []string{"/workspaces/acme/" + tc.role}, tc.forwarded)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			if rec.Header().Get("Cache-Control") != "no-store" {
				t.Error("missing Cache-Control: no-store")
			}
			if tc.status == 200 {
				if body["workspace"] != "acme" || body["provisioned"] != true || body["operator"] != false {
					t.Errorf("body = %v", body)
				}
				db := body["database"].(map[string]any)
				if db["database"] != dbName(t, "acme") || db["createdAt"] != "2026-09-30T12:00:00Z" || db["sizeBytes"] != float64(8<<20) {
					t.Errorf("database = %v", db)
				}
			}
		})
	}
}

// An owner only ever sees their own active workspace: the workspace comes from the verified
// request, and there's no parameter to ask about another one.
func TestStatus_OnlyTheActiveWorkspace(t *testing.T) {
	src := &fakeSource{status: map[string]provision.Status{"acme": {Database: dbName(t, "acme")}}}
	hs := newHarness(t, src)
	rec, body := hs.get(t, "/api/status?workspace=acme", "globex", []string{"/workspaces/globex/owner", "/workspaces/acme/viewer"}, "")
	if rec.Code != 200 || body["workspace"] != "globex" || body["provisioned"] != false || body["database"] != nil {
		t.Fatalf("%d %v", rec.Code, body)
	}
}

func TestDatabases_OperatorOnly(t *testing.T) {
	src := &fakeSource{list: []provision.Status{{Database: dbName(t, "acme")}, {Database: dbName(t, "globex")}, {Database: dbName(t, "initech")}}}
	hs := newHarness(t, src, "platform")

	// An ordinary owner can't list, and the listing isn't even read.
	rec, _ := hs.get(t, "/api/databases", "acme", []string{"/workspaces/acme/owner"}, "")
	if rec.Code != 403 || src.listCalls != 0 {
		t.Fatalf("non-operator owner: %d (list read %d times)", rec.Code, src.listCalls)
	}
	// Neither can a non-owner of the operator workspace.
	rec, _ = hs.get(t, "/api/databases", "platform", []string{"/workspaces/platform/editor"}, "")
	if rec.Code != 403 {
		t.Fatalf("operator-workspace editor: %d", rec.Code)
	}

	// An operator owner can; slugs appear only for workspaces the caller belongs to.
	groups := []string{"/workspaces/platform/owner", "/workspaces/acme/viewer"}
	rec, body := hs.get(t, "/api/databases", "platform", groups, "")
	if rec.Code != 200 {
		t.Fatalf("operator: %d %s", rec.Code, rec.Body)
	}
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("items = %v", items)
	}
	labelled := map[string]string{}
	for _, it := range items {
		m := it.(map[string]any)
		ws, _ := m["workspace"].(string)
		labelled[m["database"].(string)] = ws
	}
	if labelled[dbName(t, "acme")] != "acme" || labelled[dbName(t, "globex")] != "" || labelled[dbName(t, "initech")] != "" {
		t.Errorf("labels = %v: only the caller's own workspaces may be named", labelled)
	}
	if strings.Contains(rec.Body.String(), "globex") || strings.Contains(rec.Body.String(), "initech") {
		t.Error("the listing named a workspace the caller doesn't belong to")
	}

	// /api/status tells the UI it may offer the listing.
	_, st := hs.get(t, "/api/status", "platform", groups, "")
	if st["operator"] != true {
		t.Errorf("operator flag = %v", st["operator"])
	}
}

func TestNoVerifierMeansUnavailable(t *testing.T) {
	src := &fakeSource{}
	h := api.NewHandler(api.Deps{Verifier: func() auth.TokenVerifier { return nil }, Source: src})
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer whatever")
	req.Header.Set(auth.HeaderBoothWorkspace, "acme")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 || src.statusCalls != 0 {
		t.Fatalf("status %d, source read %d times: with no identity provider nothing may get through", rec.Code, src.statusCalls)
	}
}

func TestReadOnlyRoutes(t *testing.T) {
	hs := newHarness(t, &fakeSource{}, "platform")
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut} {
		for _, path := range []string{"/api/status", "/api/databases"} {
			req := httptest.NewRequest(method, path, nil)
			req.Header.Set("Authorization", "Bearer "+hs.idp.Mint(t, authtest.Token{Subject: "u", Groups: []string{"/workspaces/platform/owner"}}))
			req.Header.Set(auth.HeaderBoothWorkspace, "platform")
			rec := httptest.NewRecorder()
			hs.h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s = %d, want 405", method, path, rec.Code)
			}
		}
	}
}

func TestSourceErrorIsBadGateway(t *testing.T) {
	hs := newHarness(t, &fakeSource{err: errors.New("connection refused")})
	rec, _ := hs.get(t, "/api/status", "acme", []string{"/workspaces/acme/owner"}, "")
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "refused") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// Against a real PostgreSQL: the status the owner sees reflects the real server, and looking
// never provisions anything.
func TestStatus_RealPostgres(t *testing.T) {
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_ADMIN_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_POSTGRES") != "" {
			t.Fatal("BOOTH_TEST_POSTGRES_ADMIN_DSN is not set but BOOTH_TEST_REQUIRE_POSTGRES is")
		}
		t.Skip("BOOTH_TEST_POSTGRES_ADMIN_DSN not set; see hack/test-env.sh")
	}
	ctx := context.Background()
	prov, err := provision.New(ctx, dsn, provision.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	idp := authtest.New(t)
	v, _ := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database"})
	hs := harness{idp, api.NewHandler(api.Deps{Verifier: func() auth.TokenVerifier { return v }, Source: prov, OperatorWorkspaces: []string{"platform"}}), nil}

	ws := fmt.Sprintf("view-%d", time.Now().UnixNano())
	owner := []string{"/workspaces/" + ws + "/owner"}

	// Before any credential: not provisioned, and asking didn't create it.
	_, body := hs.get(t, "/api/status", ws, owner, "")
	if body["provisioned"] != false {
		t.Fatalf("fresh workspace: %v", body)
	}
	if _, provisioned, _ := prov.WorkspaceStatus(ctx, ws); provisioned {
		t.Fatal("viewing status provisioned a database")
	}

	// After a credential is issued and a table created.
	before := time.Now().Add(-time.Second)
	lease, err := prov.Issue(ctx, ws, uuid.NewString(), true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfgDSN := strings.Replace(dsn, "booth_admin:booth-admin-test", lease.Username+":"+lease.Password, 1)
	cfgDSN = strings.Replace(cfgDSN, "/postgres?", "/"+lease.Database+"?", 1)
	if err := execDSN(ctx, cfgDSN, "CREATE TABLE a (v int); CREATE TABLE b (v int)"); err != nil {
		t.Fatal(err)
	}
	rec, body := hs.get(t, "/api/status", ws, owner, "")
	if rec.Code != 200 || body["provisioned"] != true {
		t.Fatalf("%d %v", rec.Code, body)
	}
	db := body["database"].(map[string]any)
	created, err := time.Parse(time.RFC3339, db["createdAt"].(string))
	if err != nil || created.Before(before.Truncate(time.Second)) || created.After(time.Now().Add(time.Second)) {
		t.Errorf("createdAt = %v (%v)", db["createdAt"], err)
	}
	if db["tables"] != float64(2) || db["sizeBytes"].(float64) <= 0 || db["activeCredentials"].(float64) < 1 {
		t.Errorf("database = %v", db)
	}

	// The operator listing includes it — unlabelled, since the operator isn't a member.
	_, list := hs.get(t, "/api/databases", "platform", []string{"/workspaces/platform/owner"}, "")
	found := false
	for _, it := range list["items"].([]any) {
		m := it.(map[string]any)
		if m["database"] == lease.Database {
			found = true
			if _, named := m["workspace"]; named {
				t.Error("operator listing named a workspace the operator doesn't belong to")
			}
			if _, ok := m["tables"]; ok {
				t.Error("the cross-workspace listing connected into a workspace database")
			}
		}
	}
	if !found {
		t.Error("operator listing is missing the new database")
	}
}

func execDSN(ctx context.Context, dsn, sql string) error {
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer c.Close(ctx)
	_, err = c.Exec(ctx, sql)
	return err
}
