package provision

// These tests run against a real PostgreSQL server, not a mock: the brief (agent-briefs/
// database.md) singles out workspace isolation and credential expiry as security properties to
// prove against a real instance rather than infer from the provisioning SQL. They skip without
// BOOTH_TEST_POSTGRES_ADMIN_DSN, and CI sets BOOTH_TEST_REQUIRE_POSTGRES=1 so a missing server
// fails the build instead of skipping green. Local: `docker compose -f hack/docker-compose.yml up
// -d --wait` then `eval "$(sh hack/test-env.sh)"`.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/projectbooth/booth-database/internal/naming"
)

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_ADMIN_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_POSTGRES") != "" {
			t.Fatal("BOOTH_TEST_POSTGRES_ADMIN_DSN is not set but BOOTH_TEST_REQUIRE_POSTGRES is: refusing to skip")
		}
		t.Skip("BOOTH_TEST_POSTGRES_ADMIN_DSN not set; see hack/test-env.sh")
	}
	return dsn
}

func newProvisioner(t *testing.T) *Provisioner {
	t.Helper()
	p, err := New(context.Background(), adminDSN(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	if err := p.RestrictMaintenanceAccess(context.Background()); err != nil {
		t.Fatal(err)
	}
	return p
}

// uniqueWorkspace gives each test its own workspace, so tests never depend on each other's
// leftovers on a shared server.
func uniqueWorkspace(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
}

func issue(t *testing.T, p *Provisioner, ws string, readWrite bool, ttl time.Duration) Lease {
	t.Helper()
	l, err := p.Issue(context.Background(), ws, uuid.NewString(), readWrite, ttl)
	if err != nil {
		t.Fatalf("Issue(%s, rw=%v): %v", ws, readWrite, err)
	}
	return l
}

// connectAs opens a connection with a lease's credential, to database db (normally the lease's
// own), using the admin DSN's host and port — i.e. exactly what a requester would do.
func connectAs(t *testing.T, l Lease, db string) (*pgx.Conn, error) {
	t.Helper()
	cfg, err := pgx.ParseConfig(adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.User, cfg.Password, cfg.Database = l.Username, l.Password, db
	return pgx.ConnectConfig(context.Background(), cfg)
}

func mustConnect(t *testing.T, l Lease) *pgx.Conn {
	t.Helper()
	c, err := connectAs(t, l, l.Database)
	if err != nil {
		t.Fatalf("connecting as lease %s: %v", l.Username, err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func exec(t *testing.T, c *pgx.Conn, sql string) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestIssue_ReadWriteLeaseWorksAndOwnsNothing(t *testing.T) {
	p := newProvisioner(t)
	ws := uniqueWorkspace("rw")
	l := issue(t, p, ws, true, time.Minute)

	c := mustConnect(t, l)
	exec(t, c, "CREATE TABLE sales (id int primary key, amount numeric)")
	exec(t, c, "INSERT INTO sales VALUES (1, 9.5)")

	// The session runs as the workspace's group role, so the table belongs to the group and
	// outlives this lease.
	var owner, current string
	if err := c.QueryRow(context.Background(), "SELECT tableowner, current_user FROM pg_tables WHERE tablename = 'sales'").Scan(&owner, &current); err != nil {
		t.Fatal(err)
	}
	names, _ := naming.ForWorkspace(ws)
	if owner != names.ReadWrite || current != names.ReadWrite {
		t.Fatalf("table owner %q, current_user %q; want both %q", owner, current, names.ReadWrite)
	}
}

func TestIssue_WorkspacesCannotSeeEachOther(t *testing.T) {
	p := newProvisioner(t)
	wsA, wsB := uniqueWorkspace("iso-a"), uniqueWorkspace("iso-b")
	a := issue(t, p, wsA, true, time.Minute)
	b := issue(t, p, wsB, true, time.Minute)

	ca := mustConnect(t, a)
	exec(t, ca, "CREATE TABLE secret_a (v text)")
	exec(t, ca, "INSERT INTO secret_a VALUES ('only for A')")

	// B's own database doesn't contain A's table at all.
	cb := mustConnect(t, b)
	var n int
	if err := cb.QueryRow(context.Background(), "SELECT count(*) FROM information_schema.tables WHERE table_name = 'secret_a'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("workspace B's database lists workspace A's table")
	}

	// B's credential can't even open a session on A's database (CONNECT is revoked from PUBLIC,
	// granted only to A's groups and A's leases).
	if c, err := connectAs(t, b, a.Database); err == nil {
		c.Close(context.Background())
		t.Fatal("workspace B's credential connected to workspace A's database")
	} else if sqlState(err) != "42501" {
		t.Fatalf("connecting B -> A: want insufficient_privilege (42501), got %v", err)
	}

	// Nor to the maintenance databases, from which it could at least browse shared catalogs.
	for _, db := range []string{"postgres", "template1"} {
		if c, err := connectAs(t, b, db); err == nil {
			c.Close(context.Background())
			t.Fatalf("a lease connected to maintenance database %s", db)
		}
	}

	// And it can't borrow A's group role from inside its own session.
	namesA, _ := naming.ForWorkspace(wsA)
	if _, err := cb.Exec(context.Background(), "SET ROLE "+ident(namesA.ReadWrite)); err == nil {
		t.Fatal("workspace B's session assumed workspace A's group role")
	}
}

func TestIssue_DatabaseNamesDontRevealWorkspaces(t *testing.T) {
	p := newProvisioner(t)
	ws := uniqueWorkspace("secret-project")
	l := issue(t, p, ws, false, time.Minute)
	c := mustConnect(t, l)

	// pg_database, pg_roles and pg_shdescription are readable by any session on the server.
	var leaked int
	err := c.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM pg_database WHERE datname LIKE '%secret-project%' OR datname LIKE '%secret_project%')
		     + (SELECT count(*) FROM pg_roles WHERE rolname LIKE '%secret%')
		     + (SELECT count(*) FROM pg_shdescription WHERE description LIKE '%secret%')`).Scan(&leaked)
	if err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatalf("workspace slug appears in %d shared-catalog rows", leaked)
	}
}

func TestIssue_ReadLeaseCannotWrite(t *testing.T) {
	p := newProvisioner(t)
	ws := uniqueWorkspace("ro")
	writer := mustConnect(t, issue(t, p, ws, true, time.Minute))
	exec(t, writer, "CREATE TABLE facts (v int)")
	exec(t, writer, "INSERT INTO facts VALUES (42)")
	exec(t, writer, "CREATE SCHEMA reporting")
	exec(t, writer, "CREATE TABLE reporting.daily (v int)")

	reader := mustConnect(t, issue(t, p, ws, false, time.Minute))
	var v int
	if err := reader.QueryRow(context.Background(), "SELECT v FROM facts").Scan(&v); err != nil || v != 42 {
		t.Fatalf("read lease reading an existing table: v=%d err=%v", v, err)
	}
	// A table in a schema created after the fact is readable too (default privileges).
	if _, err := reader.Exec(context.Background(), "SELECT * FROM reporting.daily"); err != nil {
		t.Fatalf("read lease reading a table in a later-created schema: %v", err)
	}

	for _, stmt := range []string{
		"INSERT INTO facts VALUES (1)",
		"CREATE TABLE nope (v int)",
		"CREATE TEMP TABLE nope (v int)",
		// Turning off the per-role read-only default doesn't help: the group has no write grant.
		"SET default_transaction_read_only = off; INSERT INTO facts VALUES (1)",
	} {
		if _, err := reader.Exec(context.Background(), stmt); err == nil {
			t.Errorf("read lease succeeded at %q", stmt)
		}
	}
}

// A session that drops its default group role (SET ROLE NONE — RESET ROLE only returns to the
// per-role default, which is the group again) falls back to the bare lease role, which must have
// no privileges of its own — otherwise it could create objects owned by a role that's about to
// be dropped, or a read lease could escape its group.
func TestIssue_BareLeaseRoleGainsNothing(t *testing.T) {
	p := newProvisioner(t)
	ws := uniqueWorkspace("reset")
	rw := mustConnect(t, issue(t, p, ws, true, time.Minute))
	exec(t, rw, "CREATE TABLE t (v int)")

	for _, readWrite := range []bool{true, false} {
		l := issue(t, p, ws, readWrite, time.Minute)
		c := mustConnect(t, l)
		exec(t, c, "SET ROLE NONE")
		var current string
		if err := c.QueryRow(context.Background(), "SELECT current_user").Scan(&current); err != nil || current != l.Username {
			t.Fatalf("after SET ROLE NONE, current_user = %q (err %v), want the lease role %q", current, err, l.Username)
		}
		for _, stmt := range []string{"SELECT * FROM t", "CREATE TABLE u (v int)", "CREATE TEMP TABLE u (v int)"} {
			if _, err := c.Exec(context.Background(), stmt); err == nil {
				t.Errorf("rw=%v: bare lease role succeeded at %q", readWrite, stmt)
			}
		}
	}
}

func TestIssue_ExpiredCredentialStopsWorking(t *testing.T) {
	p := newProvisioner(t)
	ws := uniqueWorkspace("ttl")
	l := issue(t, p, ws, true, 2*time.Second)

	live := mustConnect(t, l) // opened while valid
	exec(t, live, "CREATE TABLE survives (v int)")

	time.Sleep(3 * time.Second)

	// 1. PostgreSQL itself refuses a new login past VALID UNTIL.
	if c, err := connectAs(t, l, l.Database); err == nil {
		c.Close(context.Background())
		t.Fatal("expired credential could still open a new connection")
	} else if sqlState(err) != "28P01" {
		t.Fatalf("new login after expiry: want invalid_password (28P01), got %v", err)
	}

	// 2. The reaper ends the session that was already open, and drops the role.
	if _, err := p.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	var err error
	for i := 0; i < 20; i++ { // termination is asynchronous
		if _, err = live.Exec(context.Background(), "SELECT 1"); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err == nil {
		t.Fatal("a session opened before expiry kept working after the reaper ran")
	}
	var exists bool
	admin, _ := pgx.Connect(context.Background(), adminDSN(t))
	defer admin.Close(context.Background())
	if err := admin.QueryRow(context.Background(), "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", l.Username).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("reaper left the expired lease role in place")
	}

	// 3. What the lease created belongs to the workspace, not the lease: still there for the next one.
	next := mustConnect(t, issue(t, p, ws, false, time.Minute))
	if _, err := next.Exec(context.Background(), "SELECT * FROM survives"); err != nil {
		t.Fatalf("table created under an expired lease is gone or unreadable: %v", err)
	}
}

func TestReap_LeavesLiveLeasesAlone(t *testing.T) {
	p := newProvisioner(t)
	l := issue(t, p, uniqueWorkspace("live"), true, time.Minute)
	if _, err := p.Reap(context.Background()); err != nil {
		t.Fatal(err)
	}
	c := mustConnect(t, l)
	exec(t, c, "SELECT 1")
}

func TestEnsureWorkspace_ConcurrentFirstUse(t *testing.T) {
	ws := uniqueWorkspace("race")
	// Separate provisioners model separate replicas: no shared in-process cache.
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p, err := New(context.Background(), adminDSN(t), Options{})
			if err != nil {
				errs[i] = err
				return
			}
			defer p.Close()
			_, errs[i] = p.Issue(context.Background(), ws, uuid.NewString(), true, time.Minute)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent issuer %d: %v", i, err)
		}
	}
}

func TestIssue_RejectsBadInput(t *testing.T) {
	p := newProvisioner(t)
	ctx := context.Background()
	if _, err := p.Issue(ctx, "Not_A_Slug", uuid.NewString(), true, time.Minute); err == nil {
		t.Error("accepted an invalid workspace slug")
	}
	if _, err := p.Issue(ctx, "ok", "not-a-uuid", true, time.Minute); err == nil {
		t.Error("accepted a non-UUID lease id")
	}
	if _, err := p.Issue(ctx, "ok", uuid.NewString(), true, 0); err == nil {
		t.Error("accepted a zero TTL")
	}
}
