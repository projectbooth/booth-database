package tables

// The database.table.* publisher (ADR 0116 §2). PostgreSQL has no commit hook, so this polls:
// every interval (default 5 minutes) and when someone opens the Tables view (Nudge), it lists a
// workspace database, diffs it against what was last acknowledged by the stream (Diff), and
// publishes only the changes. State is recorded per event after its JetStream ack, so delivery
// is at-least-once; a republish of the same state carries the same Nats-Msg-Id and is
// de-duplicated by the stream inside its duplicate window.
//
// Never deletes on doubt: a workspace whose listing fails, or whose database has gone, is
// skipped for the pass rather than having every table published as deleted.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"
)

// PublishedBy is the envelope's publishedBy: this module's id.
const PublishedBy = "database"

// Store is the reconcile loop's view of PostgreSQL (internal/provision).
type Store interface {
	KnownWorkspaces(ctx context.Context) ([]string, error)
	IntrospectWorkspace(ctx context.Context, ws string) ([]Relation, bool, error)
	PublishedTables(ctx context.Context, ws string) (map[Key]Published, error)
	RecordPublished(ctx context.Context, ws string, p Published) error
	ForgetPublished(ctx context.Context, ws string, k Key) error
	// WithEventsLock runs fn holding a per-workspace lock; it returns ErrBusy without running fn
	// if another pass holds it.
	WithEventsLock(ctx context.Context, ws string, fn func(context.Context) error) error
}

// Publisher is the event bus (internal/events.Bus).
type Publisher interface {
	Connected() bool
	Publish(ctx context.Context, subject string, data []byte, msgID string) error
}

// ErrBusNotConnected: nothing is published, and nothing recorded, without a live bus.
var ErrBusNotConnected = errors.New("event bus not connected")

// ErrBusy is what Store.WithEventsLock returns when another pass for the same workspace (another
// replica, or the loop overlapping an on-demand pass) holds the lock. The holder does the work.
var ErrBusy = errors.New("another reconcile pass for this workspace is running")

// Reconciler publishes database.table.* for every known workspace.
type Reconciler struct {
	Store Store
	Bus   Publisher
	// RowGap is DefaultRowCountRepublishGap unless set.
	RowGap time.Duration
	// NudgeGap rate-limits Nudge per workspace; 30s unless set.
	NudgeGap time.Duration
	Now      func() time.Time

	mu        sync.Mutex
	lastNudge map[string]time.Time
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Envelope is ADR 0026's common event envelope.
type Envelope struct {
	Workspace   string `json:"workspace"`
	EventType   string `json:"eventType"`
	PublishedAt string `json:"publishedAt"`
	PublishedBy string `json:"publishedBy"`
	Data        any    `json:"data"`
}

// DeletedData is a database.table.deleted payload: the relation's identity only.
type DeletedData struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// Subject is ADR 0026's subject for an event in a workspace.
func Subject(ws, eventType string) string { return "booth." + ws + "." + eventType }

// payload is the event's data: the full Relation for created/updated, DeletedData for deleted.
func payload(c Change) any {
	if c.Relation == nil {
		return DeletedData{Schema: c.Key.Schema, Name: c.Key.Name}
	}
	return c.Relation
}

// msgID is the same for the same state of the same relation, so a republish after a crash
// between ack and record is stored once. Hashed, because a PostgreSQL name may contain bytes a
// NATS header can't carry.
func msgID(ws string, c Change) string {
	h := sha256.New()
	for _, part := range []string{ws, c.Key.Schema, c.Key.Name, c.Type} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	if c.Relation != nil {
		h.Write([]byte(c.Relation.SchemaHash))
		h.Write([]byte{0})
		if c.Relation.ApproxRows != nil {
			h.Write([]byte(strconv.FormatInt(*c.Relation.ApproxRows, 10)))
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ReconcileWorkspace runs one pass for ws and returns what it published.
func (r *Reconciler) ReconcileWorkspace(ctx context.Context, ws string) ([]Change, error) {
	if r.Bus == nil || !r.Bus.Connected() {
		return nil, ErrBusNotConnected
	}
	var done []Change
	err := r.Store.WithEventsLock(ctx, ws, func(ctx context.Context) error {
		current, provisioned, err := r.Store.IntrospectWorkspace(ctx, ws)
		if err != nil {
			return fmt.Errorf("listing tables: %w", err)
		}
		if !provisioned {
			return nil // no database (yet, or any more): never publish deletes on doubt
		}
		published, err := r.Store.PublishedTables(ctx, ws)
		if err != nil {
			return fmt.Errorf("reading published state: %w", err)
		}
		gap := r.RowGap
		if gap <= 0 {
			gap = DefaultRowCountRepublishGap
		}
		for _, c := range Diff(published, current, r.now(), gap) {
			now := r.now().UTC()
			body, err := json.Marshal(Envelope{
				Workspace: ws, EventType: c.Type, PublishedAt: now.Format(time.RFC3339Nano),
				PublishedBy: PublishedBy, Data: payload(c),
			})
			if err != nil {
				return err
			}
			// Order is kept: the first failure stops this workspace's pass, and the rest is
			// retried next pass.
			if err := r.Bus.Publish(ctx, Subject(ws, c.Type), body, msgID(ws, c)); err != nil {
				return fmt.Errorf("publishing %s for %s: %w", c.Type, QualifiedName(c.Key.Schema, c.Key.Name), err)
			}
			if c.Relation == nil {
				err = r.Store.ForgetPublished(ctx, ws, c.Key)
			} else {
				err = r.Store.RecordPublished(ctx, ws, Published{Key: c.Key, SchemaHash: c.Relation.SchemaHash, ApproxRows: c.Relation.ApproxRows, PublishedAt: now})
			}
			if err != nil {
				return fmt.Errorf("recording %s for %s: %w", c.Type, QualifiedName(c.Key.Schema, c.Key.Name), err)
			}
			log.Printf("table events: published %s workspace=%s table=%s", c.Type, ws, QualifiedName(c.Key.Schema, c.Key.Name))
			done = append(done, c)
		}
		return nil
	})
	return done, err
}

// RunOnce reconciles every known workspace. One workspace's failure doesn't stop the others.
func (r *Reconciler) RunOnce(ctx context.Context) {
	if r.Bus == nil || !r.Bus.Connected() {
		return // the bus logs its own state; nothing to publish to
	}
	wss, err := r.Store.KnownWorkspaces(ctx)
	if err != nil {
		log.Printf("table events: listing known workspaces: %v", err)
		return
	}
	for _, ws := range wss {
		if _, err := r.ReconcileWorkspace(ctx, ws); err != nil && ctx.Err() == nil && !isBusy(err) {
			log.Printf("table events: pass failed for workspace=%s (retried next pass): %v", ws, err)
		}
	}
}

// Run calls RunOnce every interval until ctx ends, starting shortly after startup.
func (r *Reconciler) Run(ctx context.Context, interval time.Duration) {
	first := time.NewTimer(15 * time.Second) // let the bus connect first
	defer first.Stop()
	select {
	case <-ctx.Done():
		return
	case <-first.C:
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.RunOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Nudge starts a pass for ws in the background, at most once per NudgeGap per workspace: what
// opening the Tables view does, so a table created a minute ago reaches the Catalog without
// waiting for the loop.
func (r *Reconciler) Nudge(ws string) {
	gap := r.NudgeGap
	if gap <= 0 {
		gap = 30 * time.Second
	}
	now := r.now()
	r.mu.Lock()
	if r.lastNudge == nil {
		r.lastNudge = map[string]time.Time{}
	}
	if last, ok := r.lastNudge[ws]; ok && now.Sub(last) < gap {
		r.mu.Unlock()
		return
	}
	r.lastNudge[ws] = now
	r.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := r.ReconcileWorkspace(ctx, ws); err != nil && !errors.Is(err, ErrBusNotConnected) && !isBusy(err) {
			log.Printf("table events: on-demand pass failed for workspace=%s: %v", ws, err)
		}
	}()
}

func isBusy(err error) bool { return errors.Is(err, ErrBusy) }
