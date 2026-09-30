package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/credentialbroker"
	"github.com/projectbooth/booth-database/internal/provision"
)

type fakePinger struct{ err error }

func (f fakePinger) Ping(context.Context) error { return f.err }

func get(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func TestHealth(t *testing.T) {
	var ready atomic.Bool
	down := NewRouter(Deps{DB: fakePinger{errors.New("down")}, Ready: &ready, Provider: http.NotFoundHandler()})
	if get(t, down, "/livez") != 200 {
		t.Error("/livez must not depend on the database")
	}
	if get(t, down, "/healthz") != 503 {
		t.Error("/healthz must report an unreachable database")
	}

	up := NewRouter(Deps{DB: fakePinger{}, Ready: &ready, Provider: http.NotFoundHandler()})
	if get(t, up, "/healthz") != 503 {
		t.Error("/healthz must report unready before startup preparation finishes")
	}
	ready.Store(true)
	if get(t, up, "/healthz") != 200 {
		t.Error("/healthz should be 200 once ready")
	}
}

// TestProviderPath_RealPostgres drives the real HTTP stack exactly as booth-core's broker would —
// its providerRequest JSON, its Bearer provider credential, on ProviderPath — backed by a real
// provisioner and PostgreSQL, then uses the credential that comes back the way a requester would.
// The piece not covered here is booth-core's own process (routing needs its CRD controller on a
// real cluster; see docs/decisions/0001 "How this was verified").
func TestProviderPath_RealPostgres(t *testing.T) {
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
	adminCfg, _ := pgx.ParseConfig(dsn)

	var ready atomic.Bool
	ready.Store(true)
	srv := httptest.NewServer(NewRouter(Deps{
		DB: prov, Ready: &ready,
		Provider: credentialbroker.NewHandler(credentialbroker.Deps{
			Credential: "bcbp.database.secret", Issuer: prov,
			Endpoint: credentialbroker.Endpoint{Host: adminCfg.Host, Port: int(adminCfg.Port), SSLMode: "disable"},
		}),
	}))
	defer srv.Close()

	ws := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{
		// What core actually sends: its 5-minute ceiling (ADR 0088). The provider floor lifts it.
		"kind": "postgres", "ttlSeconds": 300, "access": "readwrite",
		"scope":     map[string]any{"workspace": ws},
		"requester": map[string]any{"subject": "user-1", "workspace": ws, "role": "editor"},
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+credentialbroker.ProviderPath, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer bcbp.database.secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("provider returned %d", resp.StatusCode)
	}
	var out struct {
		ExpiresAt  time.Time `json:"expiresAt"`
		Credential struct {
			Host     string `json:"host"`
			Port     int    `json:"port"`
			Database string `json:"database"`
			Username string `json:"username"`
			Password string `json:"password"`
			SSLMode  string `json:"sslMode"`
		} `json:"credential"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	// ADR 0089 §3: clamped up to the one-hour floor, and the response tells the truth about it.
	if d := time.Until(out.ExpiresAt); d < 59*time.Minute || d > 61*time.Minute {
		t.Errorf("expiresAt %v is not ~1h out (the provider floor)", out.ExpiresAt)
	}
	// ...and PostgreSQL enforces exactly that expiry, not the 5 minutes the request asked for.
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	var validUntil time.Time
	if err := admin.QueryRow(ctx, "SELECT rolvaliduntil FROM pg_roles WHERE rolname = $1", out.Credential.Username).Scan(&validUntil); err != nil {
		t.Fatal(err)
	}
	if diff := validUntil.Sub(out.ExpiresAt); diff < -time.Second || diff > time.Second {
		t.Errorf("rolvaliduntil %v != response expiresAt %v", validUntil, out.ExpiresAt)
	}

	c := out.Credential
	conn, err := pgx.Connect(ctx, fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s sslmode=%s", c.Host, c.Port, c.Database, c.Username, c.Password, c.SSLMode))
	if err != nil {
		t.Fatalf("connecting with the issued credential: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE TABLE hello (v text); INSERT INTO hello VALUES ('world')"); err != nil {
		t.Fatal(err)
	}
}
