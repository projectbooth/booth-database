package events

import (
	"context"
	"net"
	"strconv"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// runServer starts a real nats-server in-process, on a fixed port when port > 0 so a test can
// stop and restart it at the same address.
func runServer(t *testing.T, port int) *natsserver.Server {
	t.Helper()
	srv, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true})
	if err != nil {
		t.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("nats-server did not start")
	}
	return srv
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, b *Bus, want State) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b.State() == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s", b.State(), want)
}

func TestBus_DisabledWithoutURL(t *testing.T) {
	b := New(Config{})
	b.Run(context.Background()) // returns immediately
	if b.State() != StateDisabled {
		t.Fatalf("state = %s, want disabled", b.State())
	}
}

func TestBus_ConnectsToARealServer(t *testing.T) {
	srv := runServer(t, -1)
	defer srv.Shutdown()

	b := New(Config{URL: srv.ClientURL()})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()

	waitFor(t, b, StateConnected)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
}

// The bus may come up after the module (booth-core installs NATS, and the module's credential, on
// its own schedule): the first connect must keep retrying rather than give up, and a later drop
// must show as "connecting" until the server is back.
func TestBus_WaitsForALateServerAndRecoversFromADrop(t *testing.T) {
	port := freePort(t)
	b := New(Config{URL: "nats://127.0.0.1:" + strconv.Itoa(port)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	time.Sleep(200 * time.Millisecond)
	if b.State() != StateConnecting {
		t.Fatalf("state with no server = %s, want connecting", b.State())
	}

	srv := runServer(t, port)
	waitFor(t, b, StateConnected)

	srv.Shutdown()
	waitFor(t, b, StateConnecting)

	srv = runServer(t, port)
	defer srv.Shutdown()
	waitFor(t, b, StateConnected)
}
