package api_test

// ADR 0116's limits, each proved against a real PostgreSQL through the real HTTP handler, the
// real token verifier and the real provisioner. Every console call here is made as a workspace
// OWNER, the strongest role, to show the viewer cap holds regardless of role.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/api"
	"github.com/projectbooth/booth-database/internal/auth"
	"github.com/projectbooth/booth-database/internal/auth/authtest"
	"github.com/projectbooth/booth-database/internal/naming"
	"github.com/projectbooth/booth-database/internal/provision"
	"github.com/projectbooth/booth-database/internal/query"
)

type pgHarness struct {
	t    *testing.T
	dsn  string
	prov *provision.Provisioner
	idp  *authtest.IdP
	h    http.Handler
	exec *query.Executor
}

func newPGHarness(t *testing.T, timeout time.Duration) *pgHarness {
	t.Helper()
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
	t.Cleanup(prov.Close)
	if err := prov.RestrictMaintenanceAccess(ctx); err != nil {
		t.Fatal(err)
	}
	idp := authtest.New(t)
	v, err := auth.NewVerifier(ctx, auth.OIDCConfig{IssuerURL: idp.URL, ClientID: "booth-database"})
	if err != nil {
		t.Fatal(err)
	}
	exec := &query.Executor{Sessions: prov, StatementTimeout: timeout}
	h := api.NewHandler(api.Deps{Verifier: func() auth.TokenVerifier { return v }, Source: prov, Tables: prov, Query: exec})
	return &pgHarness{t: t, dsn: dsn, prov: prov, idp: idp, h: h, exec: exec}
}

// workspace provisions a fresh workspace and runs setup in it as a read-write lease, which is
// how a workspace's own code creates tables.
func (hs *pgHarness) workspace(prefix, setup string) string {
	hs.t.Helper()
	ws := fmt.Sprintf("%s-%s", prefix, strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
	if setup != "" {
		hs.asWriter(ws, setup)
	}
	return ws
}

func (hs *pgHarness) asWriter(ws, sql string) {
	hs.t.Helper()
	ctx := context.Background()
	l, err := hs.prov.Issue(ctx, ws, uuid.NewString(), true, time.Hour)
	if err != nil {
		hs.t.Fatal(err)
	}
	c := hs.connect(l.Username, l.Password, l.Database)
	defer c.Close(ctx)
	if _, err := c.Exec(ctx, sql); err != nil {
		hs.t.Fatalf("setup %q: %v", sql, err)
	}
}

func (hs *pgHarness) connect(user, password, database string) *pgx.Conn {
	hs.t.Helper()
	cfg, err := pgx.ParseConfig(hs.dsn)
	if err != nil {
		hs.t.Fatal(err)
	}
	if user != "" {
		cfg.User, cfg.Password = user, password
	}
	cfg.Database = database
	c, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		hs.t.Fatal(err)
	}
	return c
}

// admin runs a query on the admin connection in ws's database and returns the first column of
// the first row.
func (hs *pgHarness) adminScalar(ws, sql string, args ...any) any {
	hs.t.Helper()
	c := hs.connect("", "", dbName(hs.t, ws))
	defer c.Close(context.Background())
	var v any
	if err := c.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		hs.t.Fatalf("%s: %v", sql, err)
	}
	return v
}

type resp struct {
	Code int
	Body map[string]any
}

func (hs *pgHarness) do(method, path, ws, role, subject string, body any) resp {
	hs.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+hs.idp.Mint(hs.t, authtest.Token{Subject: subject, Groups: []string{"/workspaces/" + ws + "/" + role}}))
	req.Header.Set(auth.HeaderBoothWorkspace, ws)
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	out := resp{Code: rec.Code, Body: map[string]any{}}
	_ = json.Unmarshal(rec.Body.Bytes(), &out.Body)
	return out
}

func (hs *pgHarness) console(ws, sql string) resp {
	return hs.do(http.MethodPost, "/api/query", ws, "owner", "owner-"+ws, map[string]string{"sql": sql})
}

func (hs *pgHarness) preview(ws, schema, name string) resp {
	q := url.Values{"schema": {schema}, "name": {name}}
	return hs.do(http.MethodGet, "/api/tables/preview?"+q.Encode(), ws, "owner", "owner-"+ws, nil)
}

func (hs *pgHarness) tableExists(ws, schema, name string) bool {
	return hs.adminScalar(ws, "SELECT to_regclass($1) IS NOT NULL", fmt.Sprintf("%s.%s", pgx.Identifier{schema}.Sanitize(), pgx.Identifier{name}.Sanitize())).(bool)
}

// leaseRolesFor counts lease roles that are members of ws's read-only group: the console's.
func (hs *pgHarness) consoleLeases(ws string) int64 {
	n, _ := naming.ForWorkspace(ws)
	c := hs.connect("", "", "postgres")
	defer c.Close(context.Background())
	var count int64
	if err := c.QueryRow(context.Background(), `
		SELECT count(*) FROM pg_roles r JOIN pg_auth_members m ON m.member = r.oid
		  JOIN pg_roles g ON g.oid = m.roleid WHERE g.rolname = $1`, n.ReadOnly).Scan(&count); err != nil {
		hs.t.Fatal(err)
	}
	return count
}

func (hs *pgHarness) sessionsOn(ws string) int64 {
	c := hs.connect("", "", "postgres")
	defer c.Close(context.Background())
	var count int64
	if err := c.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_stat_activity WHERE datname = $1 AND application_name = 'booth-database query'", dbName(hs.t, ws)).Scan(&count); err != nil {
		hs.t.Fatal(err)
	}
	return count
}

func wantCode(t *testing.T, r resp, status int, code string) {
	t.Helper()
	if r.Code != status || (code != "" && r.Body["code"] != code) {
		t.Fatalf("got %d %v, want %d code=%s", r.Code, r.Body, status, code)
	}
}

// LIMIT: an owner's console cannot write. CREATE, INSERT and DROP all fail, and so do the
// tricks that would make the transaction writable or leave it.
func TestConsole_OwnerCannotWrite(t *testing.T) {
	hs := newPGHarness(t, 0)
	ws := hs.workspace("ro", "CREATE TABLE public.t (v int); INSERT INTO public.t VALUES (1); CREATE SEQUENCE public.s")
	for _, sql := range []string{
		"CREATE TABLE public.u (v int)",
		"CREATE TABLE public.u AS SELECT 1",
		"INSERT INTO public.t VALUES (2)",
		"UPDATE public.t SET v = 3",
		"DELETE FROM public.t",
		"TRUNCATE public.t",
		"DROP TABLE public.t",
		"ALTER TABLE public.t ADD COLUMN w int",
		"CREATE SCHEMA evil",
		"CREATE TEMP TABLE tmp (v int)",
		"SELECT nextval('public.s')",
		"SET TRANSACTION READ WRITE",
		"WITH x AS (INSERT INTO public.t VALUES (4) RETURNING 1) SELECT * FROM x",
		"DO $$ BEGIN INSERT INTO public.t VALUES (5); END $$",
		"SET ROLE " + pgx.Identifier{naming.ReadWriteGroup(dbName(t, ws))}.Sanitize(),
		"COPY public.t FROM STDIN",
		"GRANT SELECT ON public.t TO PUBLIC",
	} {
		t.Run(sql, func(t *testing.T) {
			r := hs.console(ws, sql)
			if r.Code == http.StatusOK {
				t.Fatalf("succeeded: %v", r.Body)
			}
		})
	}
	// Some statements that "succeed" harmlessly: they change nothing, and are rolled back.
	if r := hs.console(ws, "SET search_path = evil"); r.Code != http.StatusOK {
		t.Logf("SET: %d %v", r.Code, r.Body)
	}
	// COPY TO STDOUT isn't a result set the console can show; it must end, not hang.
	if r := hs.console(ws, "COPY public.t TO STDOUT"); r.Code == http.StatusOK && len(r.Body["rows"].([]any)) != 0 {
		t.Fatalf("COPY TO STDOUT: %v", r.Body)
	}
	if r := hs.console(ws, "COMMIT"); r.Code == http.StatusOK && hs.tableExists(ws, "public", "u") {
		t.Fatal("COMMIT let something through")
	}
	if !hs.tableExists(ws, "public", "t") || hs.tableExists(ws, "public", "u") {
		t.Fatal("the table set changed")
	}
	if n := hs.adminScalar(ws, "SELECT count(*) FROM public.t"); n != int64(1) {
		t.Fatalf("rows changed: %v", n)
	}
	// And it can read.
	r := hs.console(ws, "SELECT v, current_user FROM public.t")
	wantCode(t, r, 200, "")
	row := r.Body["rows"].([]any)[0].([]any)
	if row[0] != float64(1) || row[1] != dbName(t, ws)+"_ro" {
		t.Fatalf("row %v: the console must run as the read-only group", row)
	}
	if hs.consoleLeases(ws) != 0 || hs.sessionsOn(ws) != 0 {
		t.Fatal("a console lease or session outlived its query")
	}
}

// LIMIT: statement_timeout. pg_sleep beyond it is cancelled, and so is a statement that tries to
// lift its own timeout (the hard deadline and the dropped lease end it).
func TestConsole_TimeoutCancelsSleep(t *testing.T) {
	hs := newPGHarness(t, time.Second)
	ws := hs.workspace("timeout", "CREATE TABLE t (v int)")
	start := time.Now()
	r := hs.console(ws, "SELECT pg_sleep(20)")
	wantCode(t, r, http.StatusUnprocessableEntity, query.CodeTimeout)
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %s with a 1s timeout", d)
	}

	start = time.Now()
	r = hs.console(ws, "SELECT set_config('statement_timeout', '0', false), pg_sleep(30)")
	if r.Code == http.StatusOK {
		t.Fatalf("a self-lifted timeout ran to completion: %v", r.Body)
	}
	if d := time.Since(start); d > 12*time.Second {
		t.Fatalf("self-lifted timeout took %s; the hard deadline is timeout+5s", d)
	}
	if hs.consoleLeases(ws) != 0 || hs.sessionsOn(ws) != 0 {
		t.Fatal("a timed-out query left its lease or session behind")
	}
}

// LIMIT: exactly one statement.
func TestConsole_RejectsMultipleStatements(t *testing.T) {
	hs := newPGHarness(t, 0)
	ws := hs.workspace("multi", "CREATE TABLE victim (v int)")
	for _, sql := range []string{
		"SELECT 1; SELECT 2",
		"SELECT 1; DROP TABLE victim",
		"SELECT 1;DROP TABLE victim;",
		"/* x */ SELECT 1; -- y\n DROP TABLE victim",
	} {
		wantCode(t, hs.console(ws, sql), http.StatusBadRequest, query.CodeMultipleStatements)
	}
	if !hs.tableExists(ws, "public", "victim") {
		t.Fatal("victim was dropped")
	}
	// One statement with a trailing semicolon, or one containing semicolons in a string, is one.
	wantCode(t, hs.console(ws, "SELECT 'a;b'; "), 200, "")
	wantCode(t, hs.console(ws, "   "), http.StatusBadRequest, query.CodeEmpty)
}

// LIMIT: the 1,000-row cap and the 5 MiB byte cap, each truncating with a flag.
func TestConsole_RowAndByteCaps(t *testing.T) {
	hs := newPGHarness(t, 0)
	ws := hs.workspace("caps", "CREATE TABLE t (v int)")

	r := hs.console(ws, "SELECT g FROM generate_series(1, 50000) g")
	wantCode(t, r, 200, "")
	if r.Body["truncated"] != true || r.Body["truncatedReason"] != "rows" || len(r.Body["rows"].([]any)) != query.DefaultMaxRows {
		t.Fatalf("row cap: truncated=%v reason=%v rows=%d", r.Body["truncated"], r.Body["truncatedReason"], len(r.Body["rows"].([]any)))
	}

	r = hs.console(ws, "SELECT repeat('x', 60000) FROM generate_series(1, 500)") // ~30 MB
	wantCode(t, r, 200, "")
	rows := r.Body["rows"].([]any)
	if r.Body["truncated"] != true || r.Body["truncatedReason"] != "bytes" || len(rows) == 0 || len(rows) >= 500 {
		t.Fatalf("byte cap: truncated=%v reason=%v rows=%d", r.Body["truncated"], r.Body["truncatedReason"], len(rows))
	}
	b, _ := json.Marshal(rows)
	if len(b) > query.DefaultMaxBytes {
		t.Fatalf("rows are %d bytes, over the %d cap", len(b), query.DefaultMaxBytes)
	}

	r = hs.console(ws, "SELECT g FROM generate_series(1, 10) g")
	if r.Body["truncated"] != false || r.Body["commandTag"] != "SELECT 10" {
		t.Fatalf("small result: %v", r.Body)
	}
}

// LIMIT: only the caller's own workspace database.
func TestConsole_CannotReachAnotherWorkspace(t *testing.T) {
	hs := newPGHarness(t, 0)
	mine := hs.workspace("mine", "CREATE TABLE mine (v int)")
	theirs := hs.workspace("theirs", "CREATE TABLE secret (v text); INSERT INTO secret VALUES ('theirs')")
	theirDB := dbName(t, theirs)

	r := hs.console(mine, "SELECT current_database()")
	if r.Code != 200 || r.Body["rows"].([]any)[0].([]any)[0] != dbName(t, mine) {
		t.Fatalf("connected to the wrong database: %v", r.Body)
	}
	for _, sql := range []string{
		"SELECT * FROM " + pgx.Identifier{theirDB, "public", "secret"}.Sanitize(),
		"SELECT * FROM secret",
		"SELECT * FROM dblink('dbname=" + theirDB + "', 'SELECT * FROM secret') AS t(v text)",
	} {
		if r := hs.console(mine, sql); r.Code == 200 {
			t.Fatalf("%s: %v", sql, r.Body)
		}
	}
	wantCode(t, hs.preview(mine, "public", "secret"), http.StatusNotFound, query.CodeNotFound)

	// No request field can name another database...
	r = hs.do(http.MethodPost, "/api/query", mine, "owner", "u", map[string]string{"sql": "SELECT 1", "database": theirDB})
	wantCode(t, r, http.StatusBadRequest, query.CodeInvalid)
	// ...and the workspace comes from the verified token: a member of `mine` asking for `theirs`
	// is refused before anything runs.
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"sql":"SELECT * FROM secret"}`))
	req.Header.Set("Authorization", "Bearer "+hs.idp.Mint(t, authtest.Token{Subject: "u", Groups: []string{"/workspaces/" + mine + "/owner"}}))
	req.Header.Set(auth.HeaderBoothWorkspace, theirs)
	rec := httptest.NewRecorder()
	hs.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-workspace header: %d %s", rec.Code, rec.Body)
	}
}

// LIMIT: a preview of a hostile table (and schema) name cannot inject: the names are quoted as
// identifiers and the right table is read.
func TestPreview_HostileNamesCannotInject(t *testing.T) {
	hs := newPGHarness(t, 0)
	evilTable := `x"; DROP TABLE public.victim; --`
	evilSchema := `s" CASCADE; --`
	ws := hs.workspace("inject", fmt.Sprintf(`
		CREATE TABLE public.victim (v int);
		CREATE TABLE public.%s (v text); INSERT INTO public.%s VALUES ('hostile ok');
		CREATE SCHEMA %s; CREATE TABLE %s.t (v text); INSERT INTO %s.t VALUES ('schema ok')`,
		pgx.Identifier{evilTable}.Sanitize(), pgx.Identifier{evilTable}.Sanitize(),
		pgx.Identifier{evilSchema}.Sanitize(), pgx.Identifier{evilSchema}.Sanitize(), pgx.Identifier{evilSchema}.Sanitize()))

	r := hs.preview(ws, "public", evilTable)
	wantCode(t, r, 200, "")
	if r.Body["rows"].([]any)[0].([]any)[0] != "hostile ok" {
		t.Fatalf("read the wrong thing: %v", r.Body)
	}
	r = hs.preview(ws, evilSchema, "t")
	wantCode(t, r, 200, "")
	if r.Body["rows"].([]any)[0].([]any)[0] != "schema ok" {
		t.Fatalf("read the wrong thing: %v", r.Body)
	}
	for _, name := range []string{
		`victim; DROP TABLE public.victim`,
		`victim" ; DROP TABLE public.victim; --`,
		`victim"--`,
		`pg_sleep(10)`,
	} {
		if r := hs.preview(ws, "public", name); r.Code == 200 {
			t.Fatalf("%q previewed something: %v", name, r.Body)
		}
	}
	wantCode(t, hs.preview(ws, "public", ""), http.StatusBadRequest, query.CodeInvalid)
	wantCode(t, hs.preview(ws, "public", strings.Repeat("x", 64)), http.StatusBadRequest, query.CodeInvalid)
	if !hs.tableExists(ws, "public", "victim") || !hs.tableExists(ws, evilSchema, "t") {
		t.Fatal("an injection got through")
	}
}

// The preview's limit is clamped to 100 and its values are rendered JSON-safe.
func TestPreview_LimitAndRendering(t *testing.T) {
	hs := newPGHarness(t, 0)
	ws := hs.workspace("preview", `
		CREATE TABLE t (id bigint, n numeric(30,10), at timestamptz, b bytea, ok boolean, j jsonb, x text);
		INSERT INTO t SELECT g, 12345678901234567890.0123456789, '2026-10-14 12:00:00+00', decode(repeat('ab', 200), 'hex'), true, '{"a":1}', NULL
		  FROM generate_series(1, 150) g`)
	r := hs.do(http.MethodGet, "/api/tables/preview?schema=public&name=t&limit=1000", ws, "viewer", "v", nil)
	wantCode(t, r, 200, "")
	rows := r.Body["rows"].([]any)
	if len(rows) != 100 {
		t.Fatalf("limit not clamped to 100: %d", len(rows))
	}
	row := rows[0].([]any)
	if row[0] != "1" || row[1] != "12345678901234567890.0123456789" || !strings.HasPrefix(row[2].(string), "2026-10-14 12:00:00") ||
		!strings.HasSuffix(row[3].(string), "(200 bytes)") || row[4] != true || row[5] != `{"a": 1}` || row[6] != nil {
		t.Fatalf("rendering: %#v", row)
	}
	cols := r.Body["columns"].([]any)
	if cols[0].(map[string]any)["type"] != "bigint" || cols[1].(map[string]any)["type"] != "numeric" {
		t.Fatalf("columns: %v", cols)
	}
	r = hs.do(http.MethodGet, "/api/tables/preview?schema=public&name=t&limit=3", ws, "viewer", "v", nil)
	if len(r.Body["rows"].([]any)) != 3 {
		t.Fatal("a smaller limit wasn't honoured")
	}
}

// Cancel releases the connection and the lease, and one query runs per user at a time.
func TestConsole_CancelReleasesConnectionAndLease(t *testing.T) {
	hs := newPGHarness(t, 0)
	ws := hs.workspace("cancel", "CREATE TABLE t (v int)")
	subject := "owner-" + ws

	var wg sync.WaitGroup
	var r resp
	wg.Add(1)
	go func() {
		defer wg.Done()
		r = hs.console(ws, "SELECT pg_sleep(25)")
	}()
	deadline := time.Now().Add(10 * time.Second)
	for hs.sessionsOn(ws) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the query never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if hs.consoleLeases(ws) != 1 {
		t.Fatal("expected exactly one live console lease while running")
	}
	// One running query per user.
	wantCode(t, hs.do(http.MethodPost, "/api/query", ws, "owner", subject, map[string]string{"sql": "SELECT 1"}), http.StatusConflict, query.CodeBusy)
	// Someone else can't cancel it.
	wantCode(t, hs.do(http.MethodDelete, "/api/query", ws, "owner", "someone-else", nil), http.StatusNotFound, query.CodeNotFound)

	start := time.Now()
	if c := hs.do(http.MethodDelete, "/api/query", ws, "owner", subject, nil); c.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %v", c.Code, c.Body)
	}
	// By the time DELETE returns, both are gone.
	if n := hs.sessionsOn(ws); n != 0 {
		t.Fatalf("%d session(s) still open after cancel", n)
	}
	if n := hs.consoleLeases(ws); n != 0 {
		t.Fatalf("%d console lease(s) still exist after cancel", n)
	}
	wg.Wait()
	if time.Since(start) > 5*time.Second {
		t.Fatal("cancel was slow")
	}
	wantCode(t, r, http.StatusUnprocessableEntity, query.CodeCancelled)
	// The slot is free again.
	wantCode(t, hs.do(http.MethodPost, "/api/query", ws, "owner", subject, map[string]string{"sql": "SELECT 1"}), 200, "")
	wantCode(t, hs.do(http.MethodDelete, "/api/query", ws, "owner", subject, nil), http.StatusNotFound, query.CodeNotFound)
}

// GET /api/tables lists user schemas only, with columns, kinds and approximate counts, for any
// member, and never provisions.
func TestTables_Listing(t *testing.T) {
	hs := newPGHarness(t, 0)
	fresh := hs.workspace("fresh", "")
	r := hs.do(http.MethodGet, "/api/tables", fresh, "viewer", "v", nil)
	if r.Code != 200 || r.Body["provisioned"] != false || len(r.Body["tables"].([]any)) != 0 {
		t.Fatalf("fresh: %d %v", r.Code, r.Body)
	}
	if _, ok, _ := hs.prov.WorkspaceStatus(context.Background(), fresh); ok {
		t.Fatal("listing tables provisioned a database")
	}

	ws := hs.workspace("list", `
		CREATE SCHEMA nfl_data;
		CREATE TABLE nfl_data.players (id int NOT NULL, name text);
		COMMENT ON TABLE nfl_data.players IS 'One row per player';
		COMMENT ON COLUMN nfl_data.players.name IS 'Full name';
		INSERT INTO nfl_data.players SELECT g, 'p' || g FROM generate_series(1, 500) g;
		ANALYZE nfl_data.players;
		CREATE VIEW nfl_data.v AS SELECT id FROM nfl_data.players;
		CREATE MATERIALIZED VIEW public.mv AS SELECT 1 AS one;
		CREATE TABLE public.never_analyzed (v int);
		CREATE TABLE public.parted (k int, v int) PARTITION BY RANGE (k);
		CREATE TABLE public.parted_1 PARTITION OF public.parted FOR VALUES FROM (0) TO (100)`)
	r = hs.do(http.MethodGet, "/api/tables", ws, "viewer", "v", nil)
	wantCode(t, r, 200, "")
	got := map[string]map[string]any{}
	for _, it := range r.Body["tables"].([]any) {
		m := it.(map[string]any)
		got[m["schema"].(string)+"."+m["name"].(string)] = m
		if strings.HasPrefix(m["schema"].(string), "pg_") || m["schema"] == "information_schema" {
			t.Errorf("system schema listed: %v", m["schema"])
		}
	}
	if len(got) != 5 {
		t.Fatalf("want players, v, mv, never_analyzed, parted (not the partition): %v", keys(got))
	}
	p := got["nfl_data.players"]
	if p["kind"] != "table" || p["approxRows"] != float64(500) || p["comment"] != "One row per player" || p["schemaHash"] == "" {
		t.Errorf("players: %v", p)
	}
	cols := p["columns"].([]any)
	if c := cols[0].(map[string]any); c["name"] != "id" || c["type"] != "integer" || c["nullable"] != false || c["comment"] != nil {
		t.Errorf("id column: %v", c)
	}
	if c := cols[1].(map[string]any); c["nullable"] != true || c["comment"] != "Full name" {
		t.Errorf("name column: %v", c)
	}
	if got["nfl_data.v"]["kind"] != "view" || got["nfl_data.v"]["approxRows"] != nil {
		t.Errorf("view: %v", got["nfl_data.v"])
	}
	if got["public.mv"]["kind"] != "materialized_view" {
		t.Errorf("matview: %v", got["public.mv"])
	}
	if got["public.never_analyzed"]["approxRows"] != nil {
		t.Errorf("no estimate must be null, not 0 or -1: %v", got["public.never_analyzed"]["approxRows"])
	}
}

// tableEvents.ignore filters events only: the Tables API lists a migration table the default
// ignore list keeps out of the Catalog, and it can be previewed.
func TestTables_ListsIgnoredTables(t *testing.T) {
	hs := newPGHarness(t, 0)
	ws := hs.workspace("ign", `
		CREATE TABLE public.schema_migrations (version bigint PRIMARY KEY);
		INSERT INTO public.schema_migrations VALUES (20261014);
		CREATE TABLE public.players (id int)`)
	r := hs.do(http.MethodGet, "/api/tables", ws, "viewer", "v", nil)
	wantCode(t, r, 200, "")
	got := map[string]bool{}
	for _, it := range r.Body["tables"].([]any) {
		m := it.(map[string]any)
		got[m["schema"].(string)+"."+m["name"].(string)] = true
	}
	if !got["public.schema_migrations"] || !got["public.players"] {
		t.Fatalf("listing: %v", got)
	}
	r = hs.do(http.MethodGet, "/api/tables/preview?schema=public&name=schema_migrations", ws, "viewer", "v", nil)
	wantCode(t, r, 200, "")
	if rows := r.Body["rows"].([]any); len(rows) != 1 {
		t.Fatalf("preview: %v", r.Body)
	}
}

func keys(m map[string]map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}
