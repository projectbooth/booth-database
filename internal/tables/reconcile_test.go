package tables_test

// The database.table.* events end to end: a real PostgreSQL (the workspace database and the
// state store), a real in-process nats-server with JetStream and a BOOTH_EVENTS stream shaped
// like booth-core's (ADR 0026), and the real events.Bus. Creating, altering and dropping a table
// must produce exactly created, updated and deleted, with the payload booth-catalog reads.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jackc/pgx/v5"

	"github.com/projectbooth/booth-database/internal/events"
	"github.com/projectbooth/booth-database/internal/provision"
	"github.com/projectbooth/booth-database/internal/tables"
)

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("BOOTH_TEST_POSTGRES_ADMIN_DSN")
	if dsn == "" {
		if os.Getenv("BOOTH_TEST_REQUIRE_POSTGRES") != "" {
			t.Fatal("BOOTH_TEST_POSTGRES_ADMIN_DSN is not set but BOOTH_TEST_REQUIRE_POSTGRES is")
		}
		t.Skip("BOOTH_TEST_POSTGRES_ADMIN_DSN not set; see hack/test-env.sh")
	}
	return dsn
}

// startBus runs nats-server with JetStream and booth-core's stream, and returns a connected Bus
// and a pull consumer reading everything published.
func startBus(t *testing.T) (*events.Bus, jetstream.Consumer) {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir(), NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats-server did not start")
	}
	t.Cleanup(srv.Shutdown)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, _ := jetstream.New(nc)
	ctx := context.Background()
	stream, err := js.CreateStream(ctx, jetstream.StreamConfig{Name: "BOOTH_EVENTS", Subjects: []string{"booth.>"}})
	if err != nil {
		t.Fatal(err)
	}
	cons, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{AckPolicy: jetstream.AckExplicitPolicy})
	if err != nil {
		t.Fatal(err)
	}

	bus := events.New(events.Config{URL: srv.ClientURL()})
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bus.Run(runCtx)
	deadline := time.Now().Add(10 * time.Second)
	for !bus.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("bus never connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return bus, cons
}

type received struct {
	Subject string
	Env     map[string]any
}

// drain returns every message currently in the stream that this consumer hasn't seen.
func drain(t *testing.T, c jetstream.Consumer) []received {
	t.Helper()
	var out []received
	for {
		batch, err := c.FetchNoWait(100)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for m := range batch.Messages() {
			n++
			var env map[string]any
			if err := json.Unmarshal(m.Data(), &env); err != nil {
				t.Fatal(err)
			}
			out = append(out, received{m.Subject(), env})
			_ = m.Ack()
		}
		if n == 0 {
			return out
		}
	}
}

func TestReconcile_CreateAlterDrop(t *testing.T) {
	dsn := adminDSN(t)
	ctx := context.Background()
	prov, err := provision.New(ctx, dsn, provision.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	bus, cons := startBus(t)

	ws := fmt.Sprintf("events-%s", strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
	lease, err := prov.Issue(ctx, ws, uuid.NewString(), true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := pgx.ParseConfig(dsn)
	cfg.User, cfg.Password, cfg.Database = lease.Username, lease.Password, lease.Database
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	exec := func(sql string) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Issuing a credential recorded the workspace for the loop.
	known, err := prov.KnownWorkspaces(ctx)
	if err != nil || !contains(known, ws) {
		t.Fatalf("workspace not recorded on credential issue: %v %v", known, err)
	}

	now := time.Now()
	r := &tables.Reconciler{Store: prov, Bus: bus, Now: func() time.Time { return now }}
	pass := func() []received {
		t.Helper()
		if _, err := r.ReconcileWorkspace(ctx, ws); err != nil {
			t.Fatal(err)
		}
		return drain(t, cons)
	}
	subject := func(event string) string { return "booth." + ws + "." + event }

	// Empty database: nothing.
	if got := pass(); len(got) != 0 {
		t.Fatalf("empty database published %v", got)
	}

	// CREATE → created, with the full payload.
	exec("CREATE SCHEMA nfl_data; CREATE TABLE nfl_data.players (id int NOT NULL, name text); COMMENT ON COLUMN nfl_data.players.name IS 'Full name'")
	got := pass()
	if len(got) != 1 || got[0].Subject != subject(tables.EventCreated) {
		t.Fatalf("create: %v", got)
	}
	env := got[0].Env
	if env["workspace"] != ws || env["eventType"] != tables.EventCreated || env["publishedBy"] != "database" || env["publishedAt"] == "" {
		t.Fatalf("envelope: %v", env)
	}
	if _, err := time.Parse(time.RFC3339Nano, env["publishedAt"].(string)); err != nil {
		t.Fatalf("publishedAt: %v", err)
	}
	data := env["data"].(map[string]any)
	wantKeys := []string{"approxRows", "columns", "comment", "kind", "name", "schema", "schemaHash"}
	if k := sortedKeys(data); strings.Join(k, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("payload fields %v, want %v", k, wantKeys)
	}
	if data["schema"] != "nfl_data" || data["name"] != "players" || data["kind"] != "table" || data["approxRows"] != nil {
		t.Fatalf("payload: %v", data)
	}
	col := data["columns"].([]any)[1].(map[string]any)
	if strings.Join(sortedKeys(col), ",") != "comment,name,nullable,type" || col["comment"] != "Full name" || col["type"] != "text" {
		t.Fatalf("column: %v", col)
	}
	firstHash := data["schemaHash"]

	// No change → nothing.
	if got := pass(); len(got) != 0 {
		t.Fatalf("unchanged republished: %v", got)
	}

	// ALTER → updated, with a new hash.
	exec("ALTER TABLE nfl_data.players ADD COLUMN team text")
	got = pass()
	if len(got) != 1 || got[0].Subject != subject(tables.EventUpdated) {
		t.Fatalf("alter: %v", got)
	}
	upd := got[0].Env["data"].(map[string]any)
	if upd["schemaHash"] == firstHash || len(upd["columns"].([]any)) != 3 {
		t.Fatalf("updated payload: %v", upd)
	}

	// Row drift alone: not within the hour, then once after it.
	exec("INSERT INTO nfl_data.players SELECT g, 'p', 't' FROM generate_series(1, 300) g; ANALYZE nfl_data.players")
	if got := pass(); len(got) != 0 {
		t.Fatalf("row drift republished within the hour: %v", got)
	}
	now = now.Add(61 * time.Minute)
	got = pass()
	if len(got) != 1 || got[0].Subject != subject(tables.EventUpdated) || got[0].Env["data"].(map[string]any)["approxRows"] != float64(300) {
		t.Fatalf("row drift after the hour: %v", got)
	}

	// DROP → deleted, identity only; a view is created alongside.
	exec("DROP TABLE nfl_data.players; CREATE VIEW nfl_data.v AS SELECT 1 AS one")
	got = pass()
	if len(got) != 2 || got[0].Subject != subject(tables.EventDeleted) || got[1].Subject != subject(tables.EventCreated) {
		t.Fatalf("drop + view: %v", got)
	}
	del := got[0].Env["data"].(map[string]any)
	if len(del) != 2 || del["schema"] != "nfl_data" || del["name"] != "players" {
		t.Fatalf("deleted payload: %v", del)
	}
	if got[1].Env["data"].(map[string]any)["kind"] != "view" {
		t.Fatalf("view: %v", got[1].Env)
	}
	state, err := prov.PublishedTables(ctx, ws)
	if err != nil || len(state) != 1 {
		t.Fatalf("state after drop: %v %v", state, err)
	}
	if _, ok := state[tables.Key{Schema: "nfl_data", Name: "v"}]; !ok {
		t.Fatalf("state: %v", state)
	}
}

// Without a live bus nothing is published and nothing recorded, so the next pass with one still
// publishes everything.
func TestReconcile_NoBusRecordsNothing(t *testing.T) {
	dsn := adminDSN(t)
	ctx := context.Background()
	prov, err := provision.New(ctx, dsn, provision.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer prov.Close()
	ws := fmt.Sprintf("nobus-%s", strings.ReplaceAll(uuid.NewString(), "-", "")[:12])
	if _, err := prov.Issue(ctx, ws, uuid.NewString(), true, time.Hour); err != nil {
		t.Fatal(err)
	}
	r := &tables.Reconciler{Store: prov, Bus: events.New(events.Config{})}
	if _, err := r.ReconcileWorkspace(ctx, ws); err != tables.ErrBusNotConnected {
		t.Fatalf("err = %v", err)
	}
	if st, _ := prov.PublishedTables(ctx, ws); len(st) != 0 {
		t.Fatalf("state recorded without a bus: %v", st)
	}
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
