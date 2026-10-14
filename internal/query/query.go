// Package query runs the native view's table preview and read-only SQL console (ADR 0116 §3-4).
//
// Every query, preview or console, runs under all of these limits at once; none of them is
// trusted alone (ADR 0116 "Risks and notes": defence in depth):
//
//   - As a fresh read-only lease of the caller's own workspace database (`<db>_ro`: SELECT only,
//     default_transaction_read_only), whatever the caller's role, owners included: viewer-capped,
//     ADR 0104's pattern. The connection's database is the caller's workspace's, derived from the
//     verified token's workspace; nothing in a request can name another.
//   - Inside BEGIN READ ONLY, with a snapshot taken before the caller's statement (so it can't
//     switch the transaction to read-write), and always rolled back, never committed.
//   - statement_timeout (30s), backed by a hard context deadline a few seconds later that cancels
//     the statement and drops the lease, which terminates the session, whatever the statement
//     did to its own settings.
//   - Exactly one statement: the text goes through the extended protocol's Parse, which refuses
//     more than one command, so there is no client-side SQL splitting to get wrong.
//   - At most 1,000 rows and 5 MiB of rendered values; past either, reading stops, the statement
//     is cancelled and the result says truncated.
//   - One running query per user (Limiter), cancellable by that user (Cancel); a cancel, a
//     timeout or a client disconnect ends the statement, closes the connection and drops the
//     lease before the request returns.
//   - One audit line per query: user, workspace, duration, outcome and the query text.
package query

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/projectbooth/booth-database/internal/provision"
	"github.com/projectbooth/booth-database/internal/tables"
)

// Defaults (ADR 0116 §4).
const (
	DefaultStatementTimeout = 30 * time.Second
	DefaultMaxRows          = 1000
	DefaultMaxBytes         = 5 << 20 // 5 MiB
	// PreviewMaxRows is the preview's row limit; a caller's limit is clamped to it.
	PreviewMaxRows = 100
	// MaxSQLBytes bounds the console's input.
	MaxSQLBytes = 100 << 10
	// MaxValueBytes bounds one rendered text value; longer ones are cut and marked.
	MaxValueBytes = 64 << 10
	// MaxByteaBytes is how much of a bytea value is shown, as hex.
	MaxByteaBytes = 64
	// hardDeadlineGrace is how long past statement_timeout the hard deadline fires.
	hardDeadlineGrace = 5 * time.Second
)

// Sessions opens viewer-capped connections (internal/provision).
type Sessions interface {
	OpenViewerSession(ctx context.Context, ws string, ttl time.Duration) (*provision.ViewerSession, error)
}

// Caller is who is running a query, from the verified token (internal/auth).
type Caller struct {
	Subject   string
	Workspace string
}

// Column is one result column.
type Column struct {
	Name string `json:"name"`
	// Type is the PostgreSQL type name (format_type), or "" when it couldn't be resolved.
	Type string `json:"type"`
}

// Result is a query's JSON-safe result.
type Result struct {
	Columns []Column `json:"columns"`
	// Rows are rendered JSON-safe (see render): null, booleans, int2/int4/float as numbers,
	// everything else (timestamps, numeric, int8, json, arrays, ...) as PostgreSQL's own text.
	Rows [][]any `json:"rows"`
	// RowCount is len(Rows).
	RowCount int `json:"rowCount"`
	// Truncated is true when the result was cut at the row or byte cap, or a single value was
	// cut. TruncatedReason is "rows", "bytes" or "value".
	Truncated       bool   `json:"truncated"`
	TruncatedReason string `json:"truncatedReason,omitempty"`
	// CommandTag is PostgreSQL's completion tag (e.g. "SELECT 12"), empty when truncated.
	CommandTag string `json:"commandTag"`
	DurationMs int64  `json:"durationMs"`
}

// Error codes returned to the caller (Error.Code).
const (
	CodeEmpty              = "empty"
	CodeTooLarge           = "too_large"
	CodeMultipleStatements = "multiple_statements"
	CodeTimeout            = "timeout"
	CodeCancelled          = "cancelled"
	CodeBusy               = "busy"
	CodeNotProvisioned     = "not_provisioned"
	CodeNotFound           = "not_found"
	CodeInvalid            = "invalid"
	CodeReadOnly           = "read_only"
	CodeSQL                = "sql_error"
	CodeUnavailable        = "unavailable"
)

// Error is a query failure the caller may see: a PostgreSQL error's message, or one of ours.
type Error struct {
	Code    string
	Message string
	// SQLState is the PostgreSQL error code, when there is one.
	SQLState string
}

func (e *Error) Error() string { return e.Message }

// Executor runs previews and console queries.
type Executor struct {
	Sessions Sessions
	// Zero means the default for each.
	StatementTimeout time.Duration
	MaxRows          int
	MaxBytes         int

	limiter Limiter
}

func (e *Executor) timeout() time.Duration {
	if e.StatementTimeout > 0 {
		return e.StatementTimeout
	}
	return DefaultStatementTimeout
}

func (e *Executor) maxRows() int {
	if e.MaxRows > 0 {
		return e.MaxRows
	}
	return DefaultMaxRows
}

func (e *Executor) maxBytes() int {
	if e.MaxBytes > 0 {
		return e.MaxBytes
	}
	return DefaultMaxBytes
}

// WriteDeadline is how long a handler should allow for writing a query's response.
func (e *Executor) WriteDeadline() time.Duration {
	return e.timeout() + hardDeadlineGrace + 15*time.Second
}

// Console runs one caller-supplied statement.
func (e *Executor) Console(ctx context.Context, c Caller, sql string) (*Result, error) {
	if strings.TrimSpace(sql) == "" {
		return nil, &Error{Code: CodeEmpty, Message: "enter a SQL statement to run"}
	}
	if len(sql) > MaxSQLBytes {
		return nil, &Error{Code: CodeTooLarge, Message: fmt.Sprintf("the statement is longer than %d KiB", MaxSQLBytes>>10)}
	}
	if strings.ContainsRune(sql, 0) {
		return nil, &Error{Code: CodeInvalid, Message: "the statement contains a NUL character"}
	}
	return e.run(ctx, c, "console", sql, e.maxRows())
}

// Preview returns the first limit rows (clamped to 1..100) of schema.name. The names are
// validated and quoted as identifiers, never concatenated.
func (e *Executor) Preview(ctx context.Context, c Caller, schema, name string, limit int) (*Result, error) {
	if !tables.ValidIdentifier(schema) || !tables.ValidIdentifier(name) {
		return nil, &Error{Code: CodeInvalid, Message: "schema and table must each be 1-63 bytes with no NUL"}
	}
	if limit <= 0 || limit > PreviewMaxRows {
		limit = PreviewMaxRows
	}
	sql := "SELECT * FROM " + tables.QualifiedName(schema, name) + " LIMIT " + strconv.Itoa(limit)
	return e.run(ctx, c, "preview", sql, limit)
}

// Cancel cancels the caller's running query, if any, and waits (up to wait) until its
// connection is closed and its lease dropped. It reports whether there was one.
func (e *Executor) Cancel(subject string, wait time.Duration) bool {
	return e.limiter.cancel(subject, wait)
}

func (e *Executor) run(ctx context.Context, c Caller, kind, sql string, maxRows int) (res *Result, err error) {
	slot, ok := e.limiter.acquire(c.Subject)
	if !ok {
		return nil, &Error{Code: CodeBusy, Message: "you already have a query running; cancel it or wait for it to finish"}
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, e.timeout()+hardDeadlineGrace)
	slot.setCancel(cancel)
	defer func() {
		cancel()
		dur := time.Since(start)
		if res != nil {
			res.DurationMs = dur.Milliseconds()
		}
		e.audit(c, kind, sql, dur, res, err)
		slot.release()
	}()

	sess, err := e.Sessions.OpenViewerSession(ctx, c.Workspace, e.timeout()+2*time.Minute)
	if err != nil {
		if errors.Is(err, provision.ErrNotProvisioned) {
			return nil, &Error{Code: CodeNotProvisioned, Message: "this workspace has no database yet"}
		}
		if slot.cancelled() {
			return nil, &Error{Code: CodeCancelled, Message: "the query was cancelled"}
		}
		log.Printf("query: opening a read-only session for workspace %s: %v", c.Workspace, err)
		return nil, &Error{Code: CodeUnavailable, Message: "the database server couldn't be reached"}
	}
	// Closing drops the lease and terminates the session: a cancel or timeout releases both
	// before this returns.
	defer sess.Close()
	res, err = e.execute(ctx, sess, sql, maxRows)
	if err != nil {
		return nil, e.classify(ctx, err, slot)
	}
	return res, nil
}

func (e *Executor) execute(ctx context.Context, sess *provision.ViewerSession, sql string, maxRows int) (*Result, error) {
	pc := sess.Conn.PgConn()
	setup := fmt.Sprintf("BEGIN READ ONLY; SET LOCAL statement_timeout = %d; SET LOCAL idle_in_transaction_session_timeout = %d",
		e.timeout().Milliseconds(), (e.timeout() + time.Minute).Milliseconds())
	if err := pc.Exec(ctx, setup).Close(); err != nil {
		return nil, err
	}
	// Take the transaction's first snapshot now. After this, SET TRANSACTION READ WRITE fails
	// ("must be called before any query"), so a caller's statement can't make the transaction
	// writable.
	if _, err := pc.Exec(ctx, "SELECT 1").ReadAll(); err != nil {
		return nil, err
	}

	// Extended protocol, text results: Parse refuses a string with more than one command, and
	// every value comes back as PostgreSQL's own text rendering.
	rr := pc.ExecParams(ctx, sql, nil, nil, nil, nil)
	fields := rr.FieldDescriptions()
	res := &Result{Columns: make([]Column, len(fields)), Rows: [][]any{}}
	tm := pgtype.NewMap()
	for i, f := range fields {
		res.Columns[i] = Column{Name: f.Name, Type: typeName(tm, f.DataTypeOID)}
	}
	size := 0
	for rr.NextRow() {
		if len(res.Rows) >= maxRows {
			res.Truncated, res.TruncatedReason = true, "rows"
			break
		}
		vals := rr.Values()
		row := make([]any, len(vals))
		for i, v := range vals {
			var cut bool
			row[i], cut = render(fields[i].DataTypeOID, v)
			if cut && !res.Truncated {
				res.Truncated, res.TruncatedReason = true, "value"
			}
		}
		b, _ := json.Marshal(row)
		if size+len(b) > e.maxBytes() {
			res.Truncated, res.TruncatedReason = true, "bytes"
			break
		}
		size += len(b)
		res.Rows = append(res.Rows, row)
	}
	if res.Truncated && (res.TruncatedReason == "rows" || res.TruncatedReason == "bytes") {
		// Stop the statement rather than read (and discard) the rest of its output. The session
		// is dropped right after, so its state doesn't matter.
		_ = pc.CancelRequest(context.Background())
		_, _ = rr.Close()
		res.RowCount = len(res.Rows)
		return res, nil
	}
	tag, err := rr.Close()
	if err != nil {
		return nil, err
	}
	res.CommandTag = tag.String()
	res.RowCount = len(res.Rows)
	// Never commit. A statement that ended the transaction itself (COMMIT/ROLLBACK) leaves
	// nothing to roll back; the warning that produces is harmless.
	_ = pc.Exec(ctx, "ROLLBACK").Close()
	resolveTypes(ctx, sess, res, fields)
	return res, nil
}

// resolveTypes names columns whose type pgtype doesn't know (domains, enums, extension types),
// best effort, on the same connection after the rollback.
func resolveTypes(ctx context.Context, sess *provision.ViewerSession, res *Result, fields []pgconn.FieldDescription) {
	var missing []uint32
	for i, c := range res.Columns {
		if c.Type == "" {
			missing = append(missing, fields[i].DataTypeOID)
		}
	}
	if len(missing) == 0 {
		return
	}
	names := map[uint32]string{}
	rows, err := sess.Conn.Query(ctx, "SELECT oid, format_type(oid, NULL) FROM pg_type WHERE oid = ANY($1)", missing)
	if err != nil {
		return
	}
	for rows.Next() {
		var oid uint32
		var name string
		if rows.Scan(&oid, &name) == nil {
			names[oid] = name
		}
	}
	rows.Close()
	for i := range res.Columns {
		if res.Columns[i].Type == "" {
			res.Columns[i].Type = names[fields[i].DataTypeOID]
		}
	}
}

// typeName is the built-in type's SQL name, or "" for one pgtype doesn't know.
func typeName(m *pgtype.Map, oid uint32) string {
	if n, ok := builtinNames[oid]; ok {
		return n
	}
	if t, ok := m.TypeForOID(oid); ok {
		return t.Name
	}
	return ""
}

// builtinNames spells the common types the way format_type() does (and the Tables view shows).
var builtinNames = map[uint32]string{
	pgtype.BoolOID: "boolean", pgtype.Int2OID: "smallint", pgtype.Int4OID: "integer", pgtype.Int8OID: "bigint",
	pgtype.Float4OID: "real", pgtype.Float8OID: "double precision", pgtype.NumericOID: "numeric",
	pgtype.TextOID: "text", pgtype.VarcharOID: "character varying", pgtype.BPCharOID: "character",
	pgtype.DateOID: "date", pgtype.TimestampOID: "timestamp without time zone",
	pgtype.TimestamptzOID: "timestamp with time zone", pgtype.TimeOID: "time without time zone",
	pgtype.IntervalOID: "interval", pgtype.ByteaOID: "bytea", pgtype.JSONOID: "json", pgtype.JSONBOID: "jsonb",
	pgtype.UUIDOID: "uuid",
}

// render turns one text-format value into something JSON can carry without loss. cut reports a
// value shortened for display.
//
//   - NULL → null; boolean → true/false;
//   - smallint, integer, oid → a JSON number (always exact in a float64);
//   - real, double precision → a JSON number, or the text "NaN"/"Infinity"/"-Infinity";
//   - bigint and numeric → text, so no digit is lost to a JavaScript number;
//   - bytea → "\x" plus the first 64 bytes as hex, then "… (N bytes)" if longer;
//   - everything else (timestamps, dates, json, arrays, ...) → PostgreSQL's own text, cut at
//     64 KiB with a trailing "…".
func render(oid uint32, v []byte) (val any, cut bool) {
	if v == nil {
		return nil, false
	}
	s := string(v)
	switch oid {
	case pgtype.BoolOID:
		return s == "t", false
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.OIDOID:
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			return n, false
		}
	case pgtype.Float4OID, pgtype.Float8OID:
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsInf(f, 0) && !math.IsNaN(f) {
			return f, false
		}
		return s, false
	case pgtype.ByteaOID:
		raw, err := hex.DecodeString(strings.TrimPrefix(s, `\x`))
		if err != nil || len(raw) <= MaxByteaBytes {
			return s, false
		}
		return fmt.Sprintf(`\x%s… (%d bytes)`, hex.EncodeToString(raw[:MaxByteaBytes]), len(raw)), true
	}
	if len(s) > MaxValueBytes {
		end := MaxValueBytes
		for end > 0 && !utf8.RuneStart(s[end]) {
			end--
		}
		return s[:end] + "…", true
	}
	return s, false
}

func (e *Executor) classify(ctx context.Context, err error, slot *running) error {
	if slot.cancelled() {
		return &Error{Code: CodeCancelled, Message: "the query was cancelled"}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch {
		case pgErr.Code == "42601" && strings.Contains(pgErr.Message, "multiple commands"):
			return &Error{Code: CodeMultipleStatements, Message: "run one statement at a time", SQLState: pgErr.Code}
		case pgErr.Code == "57014":
			return &Error{Code: CodeTimeout, Message: fmt.Sprintf("the query ran longer than %s and was stopped", e.timeout()), SQLState: pgErr.Code}
		case pgErr.Code == "25006":
			return &Error{Code: CodeReadOnly, Message: "the SQL console is read-only: " + pgErr.Message, SQLState: pgErr.Code}
		case pgErr.Code == "42P01":
			return &Error{Code: CodeNotFound, Message: pgErr.Message, SQLState: pgErr.Code}
		}
		return &Error{Code: CodeSQL, Message: pgErr.Message, SQLState: pgErr.Code}
	}
	if ctx.Err() != nil {
		return &Error{Code: CodeTimeout, Message: fmt.Sprintf("the query ran longer than %s and was stopped", e.timeout())}
	}
	log.Printf("query: %v", err)
	return &Error{Code: CodeUnavailable, Message: "the query failed: the database connection was lost"}
}

// audit writes the one log line per query ADR 0116 §4 asks for (ADR 0067's logging policy).
func (e *Executor) audit(c Caller, kind, sql string, dur time.Duration, res *Result, err error) {
	outcome, rows := "ok", 0
	if res != nil {
		rows = res.RowCount
		if res.Truncated {
			outcome = "truncated:" + res.TruncatedReason
		}
	}
	var qe *Error
	if errors.As(err, &qe) {
		outcome = "error:" + qe.Code
	} else if err != nil {
		outcome = "error"
	}
	log.Printf("query audit: kind=%s sub=%s workspace=%s duration_ms=%d rows=%d outcome=%s sql=%q",
		kind, c.Subject, c.Workspace, dur.Milliseconds(), rows, outcome, sql)
}

// Limiter allows one running query per user.
type Limiter struct {
	mu      sync.Mutex
	running map[string]*running
}

type running struct {
	l       *Limiter
	subject string
	done    chan struct{}

	mu           sync.Mutex
	cancel       context.CancelFunc
	wasCancelled bool
}

func (l *Limiter) acquire(subject string) (*running, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.running == nil {
		l.running = map[string]*running{}
	}
	if _, busy := l.running[subject]; busy {
		return nil, false
	}
	r := &running{l: l, subject: subject, done: make(chan struct{})}
	l.running[subject] = r
	return r, true
}

func (l *Limiter) cancel(subject string, wait time.Duration) bool {
	l.mu.Lock()
	r := l.running[subject]
	l.mu.Unlock()
	if r == nil {
		return false
	}
	r.mu.Lock()
	r.wasCancelled = true
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	select {
	case <-r.done:
	case <-time.After(wait):
	}
	return true
}

func (r *running) setCancel(c context.CancelFunc) {
	r.mu.Lock()
	r.cancel = c
	already := r.wasCancelled
	r.mu.Unlock()
	if already {
		c()
	}
}

func (r *running) cancelled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wasCancelled
}

func (r *running) release() {
	r.l.mu.Lock()
	delete(r.l.running, r.subject)
	r.l.mu.Unlock()
	close(r.done)
}
