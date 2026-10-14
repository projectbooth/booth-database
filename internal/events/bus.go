// Package events holds booth-database's event-bus connection (NATS, ADR 0021), over which it
// publishes database.table.created/updated/deleted (ADR 0116) for booth-catalog to index.
//
// A copy of booth-streamlit's internal/events/bus.go (the fleet's Go publisher). Events reach it
// from internal/tables' reconcile loop, which records a change only after its JetStream ack.
package events

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrNotConnected: there is no live bus connection to publish on.
var ErrNotConnected = errors.New("event bus not connected")

// State is the bus connection's observable state, reported verbatim on /healthz.
type State string

const (
	// StateDisabled: no bus URL configured, so nothing will ever be published.
	StateDisabled State = "disabled"
	// StateConnecting: not yet connected, or reconnecting after a drop.
	StateConnecting State = "connecting"
	// StateConnected: a live, authenticated connection.
	StateConnected State = "connected"
)

// Config says where the bus is and how to authenticate to it.
type Config struct {
	URL string
	// CredentialsFile is booth-core's minted ".creds" file (ADR 0050). nats.go re-reads it on
	// every connect and reconnect, so core's in-place renewal of the mounted Secret is picked up.
	CredentialsFile string
	// Name is the client name the server logs; defaults to "booth-database".
	Name string
}

// Bus owns one NATS connection for the life of the process. The zero value is not usable; use
// New. It is safe for concurrent use.
type Bus struct {
	cfg Config

	mu    sync.Mutex
	state State
	nc    *nats.Conn
}

// New returns a Bus that has not connected yet. An empty URL yields a permanently disabled Bus.
func New(cfg Config) *Bus {
	if cfg.Name == "" {
		cfg.Name = "booth-database"
	}
	b := &Bus{cfg: cfg, state: StateConnecting}
	if cfg.URL == "" {
		b.state = StateDisabled
	}
	return b
}

// State reports the current connection state.
func (b *Bus) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

func (b *Bus) setState(s State) {
	b.mu.Lock()
	b.state = s
	b.mu.Unlock()
}

// Run connects and keeps the connection alive until ctx ends, then closes it. It never returns an
// error: a bus outage, or booth-core not having written the credential yet, must not keep the
// module from serving credentials or its API. The state is visible through State instead.
func (b *Bus) Run(ctx context.Context) {
	if b.cfg.URL == "" {
		log.Print("BOOTH_NATS_URL is not set: database.table.* events are disabled, so booth-catalog will never list this module's tables")
		return
	}
	opts := []nats.Option{
		nats.Name(b.cfg.Name),
		// Keep trying forever, including the very first connect: the bus may come up after us.
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * time.Second),
		nats.ConnectHandler(func(*nats.Conn) { b.setState(StateConnected); log.Print("event bus connected") }),
		nats.ReconnectHandler(func(*nats.Conn) { b.setState(StateConnected); log.Print("event bus reconnected") }),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			b.setState(StateConnecting)
			if err != nil {
				log.Printf("event bus disconnected: %v", err)
			}
		}),
	}
	if b.cfg.CredentialsFile != "" {
		opts = append(opts, nats.UserCredentials(b.cfg.CredentialsFile))
	}
	nc, err := nats.Connect(b.cfg.URL, opts...)
	if err != nil {
		// With RetryOnFailedConnect this only happens for a configuration error (a malformed URL,
		// an unreadable credentials file), which retrying cannot fix.
		log.Printf("event bus: %v", err)
		return
	}
	if nc.IsConnected() {
		b.setState(StateConnected)
	}
	b.mu.Lock()
	b.nc = nc
	b.mu.Unlock()

	<-ctx.Done()
	b.mu.Lock()
	b.nc = nil
	b.mu.Unlock()
	nc.Close()
}

// Connected reports whether there is a live connection to publish on.
func (b *Bus) Connected() bool {
	b.mu.Lock()
	nc := b.nc
	b.mu.Unlock()
	return nc != nil && nc.IsConnected()
}

// Publish publishes to JetStream and waits for its acknowledgement: the event is stored in
// booth-core's stream (ADR 0026) when this returns nil. msgID is JetStream's de-duplication id,
// so a republish of the same state inside the stream's duplicate window is stored once.
func (b *Bus) Publish(ctx context.Context, subject string, data []byte, msgID string) error {
	b.mu.Lock()
	nc := b.nc
	b.mu.Unlock()
	if nc == nil || !nc.IsConnected() {
		return ErrNotConnected
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	_, err = js.Publish(ctx, subject, data, jetstream.WithMsgID(msgID))
	return err
}
