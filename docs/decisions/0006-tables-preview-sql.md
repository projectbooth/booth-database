# 0006: Tables, row preview and the read-only SQL console (ADR 0116)

Date: 2026-10-14

ADR 0116 lists a workspace database's tables in booth-catalog, from events this module publishes,
and adds a row preview and a read-only SQL console to this module's native view. These are the
judgment calls made building it. Code: `internal/tables` (introspection, diff, reconcile loop),
`internal/query` (preview and console), `internal/provision/tables.go` and `state.go`,
`internal/api/tables.go`, `web/src/`.

## §1 Where the last-published state lives

In a private schema, `booth_database`, of the admin role's own maintenance database (`postgres` on
the bundled server, `external.database` on an external one):

- `booth_database.workspaces (slug, database, first_seen)`: the workspaces the reconcile loop visits.
- `booth_database.published_tables (workspace, schema_name, table_name, schema_hash, approx_rows,
  published_at)`: per (workspace, schema, name), what the stream last acknowledged.

The schema grants nothing to PUBLIC, so no lease can read it. On the bundled server no lease can
even connect to that database (CONNECT is revoked from PUBLIC). On an external cluster with
`restrictMaintenanceAccess=false`, a lease can connect but sees only that the schema exists.

**This is a departure from 0001 §2 / 0004: the module now records workspace slugs.** It has to. An
event is addressed to a workspace by slug (ADR 0026's subject and envelope), and a database name is
a one-way hash of one. So a loop that runs with nobody signed in must have the slugs stored
somewhere. 0001 §2's concern was slugs in the *shared* catalogs (`pg_database`, `pg_roles`,
`pg_shdescription`), which every role on the server can read. These are rows in an ordinary table
nobody else can read, so that concern still holds. The operator listing (`GET /api/databases`)
still labels only the operator's own workspaces, and nothing reads this table to label it.

The module learns a slug when one reaches it: on each credential request (`provision.Issue`) and
when someone opens the Tables view. A workspace that existed before this change appears in the
Catalog after its next credential request or Tables visit. For notebooks and pipelines that is
usually within an hour, since the credential sidecar renews constantly.

Why not elsewhere:

- **booth-core's module database** (manifest `database:`). 0001 and the brief rule out declaring
  it for this module.
- **Inside each workspace database.** A workspace's own roles own those databases, and its code
  could read or forge the state.
- **In memory only.** A restart would re-announce every table, and could never publish a delete
  for a table dropped while the module was down.

The state is not in the nightly backup (`backup.sh` dumps globals and the workspace databases). If
it is lost, the next pass republishes every table as `created`. booth-catalog merges on
(workspace, schema, name) (ADR 0116 §5), so that is harmless. A table dropped while the state was
missing is never announced as deleted.

## §2 Reconcile interval and debouncing

- **A pass every `tableEvents.interval`, default 5 minutes** (ADR 0116's number; minimum 10s), over
  every known workspace. The first pass runs 15s after startup, giving the bus time to connect.
- **Opening the Tables view triggers a pass for that workspace**, at most once per 30s per
  workspace, in the background. The view itself never waits on the bus.
- **A schema change publishes on the next pass.** A change in the approximate row count alone
  republishes a table at most once an hour (`tables.DefaultRowCountRepublishGap`).
- **Never deletes on doubt.** If a listing fails, or the workspace's database is gone, that
  workspace is skipped for the pass.
- **State is recorded only after JetStream acknowledges each event.** The first failure stops that
  workspace's pass, so order is kept. Delivery is at-least-once: the `Nats-Msg-Id` is a hash of
  (workspace, schema, name, event type, schema hash, row count), so a republish of the same state
  within the stream's duplicate window is stored once.
- **A per-workspace advisory lock** (`pg_try_advisory_lock`) stops two replicas, or the loop and an
  on-demand pass, from publishing the same change twice.
- **Without a live bus (or with `eventBus.enabled=false`) nothing is published or recorded.** The
  Tables view, preview and console are unaffected.

## §3 Event shape (for `contracts/`)

Subject `booth.<workspace>.database.table.<created|updated|deleted>` on `BOOTH_EVENTS`. The
envelope is ADR 0026's: `workspace`, `eventType`, `publishedAt` (RFC 3339, UTC), `publishedBy:
"database"`, `data`. The manifest declares `events: {publish: [database.table.*]}`.

`created` and `updated` `data`:

| Field | Type | Meaning |
|---|---|---|
| `schema` | string | Schema name |
| `name` | string | Relation name |
| `kind` | `"table"` \| `"view"` \| `"materialized_view"` | Partitioned tables are `table`; partitions aren't listed |
| `comment` | string \| null | The relation's COMMENT |
| `columns` | array of `{name, type, nullable, comment}` | In column order; `type` is `format_type()` (e.g. `character varying(20)`); `comment` is string or null |
| `approxRows` | integer \| null | `pg_class.reltuples` (summed over leaf partitions); null when there is no estimate, and always for a view |
| `schemaHash` | string | Hex SHA-256 of kind, comment and columns; changes on any shape change, never on row changes |

`deleted` `data`: `{schema, name}` only.

## §4 How a query runs (preview and console)

Each query mints its **own lease**: a fresh login role in the workspace's `_ro` group, with
`default_transaction_read_only`. It runs as that lease whatever the caller's role, owners included
(ADR 0104's viewer cap). The lease is minted by this module directly, not through booth-core's
broker. Its password never leaves this process, it is used for one connection, and the role is
dropped as soon as the query ends. The caller has already been verified here (ADR 0041), and the
audit line records who ran what.

Its lifetime is the statement timeout plus two minutes, a crash backstop the reaper enforces. It
is not `leases.minTTL`: that floor is for credentials handed out through the broker, and
`DefaultMinTTL` is unchanged.

Then, on that connection:

1. `BEGIN READ ONLY; SET LOCAL statement_timeout = 30s`, then `SELECT 1`, which takes the
   transaction's first snapshot. After it, `SET TRANSACTION READ WRITE` is refused.
2. The caller's text goes through the extended protocol with text results (`pgconn.ExecParams`).
   Its Parse step refuses a string with more than one command. So "exactly one statement" is
   enforced by PostgreSQL's own parser, with no client-side splitting to get wrong around dollar
   quotes and comments.
3. Rows are read until 1,000 rows or 5 MiB of rendered JSON. Past either, reading stops, the
   server is sent a CancelRequest, and the result says `truncated` with a reason (`rows`, `bytes`,
   or `value` for a single value cut for display).
4. The transaction is always rolled back, never committed.
5. Closing the session drops the lease, which also terminates any backend still running on it.
   This is what makes cancel, timeout and a client disconnect all release both the connection and
   the lease before the request returns. A hard context deadline at timeout + 5s cancels the
   statement whatever it did to its own settings (tested with
   `set_config('statement_timeout', '0', …)`).

**Defence in depth.** Each guard works without the others. Writes fail on the read-only
transaction first; the `_ro` group has no write privilege at all (provision's tests already prove
that for read leases); and the lease can't `SET ROLE` to `_rw`.

**One running query per user**, by token `sub`, across workspaces. The preview shares the slot.
`DELETE /api/query` cancels the caller's own query and returns once it's released.

**Only the caller's own workspace database.** The workspace comes from the verified token and the
gateway header (ADR 0041). The request body accepts only `sql`; an unknown field such as
`database` is a 400. PostgreSQL itself refuses cross-database references.

**Preview:** `SELECT * FROM "<schema>"."<name>" LIMIT n`, with `n` clamped to 1..100. Names must be
1 to 63 bytes with no NUL, and are quoted as identifiers (embedded `"` doubled), never
concatenated raw.

**Audit:** one line per query, `query audit: kind=<preview|console> sub=… workspace=… duration_ms=…
rows=… outcome=… sql="…"`. The query text is logged as typed, so a literal in it is logged too.
ADR 0067's log policy applies.

**Write deadline.** The server's `WriteTimeout` is 30s, the same as the statement timeout, so query
routes extend their own write deadline (`http.ResponseController`).

## §5 Value rendering

Results use PostgreSQL's text format, rendered so JSON carries them without loss:

- NULL → `null`; boolean → `true`/`false`.
- smallint, integer, oid → JSON numbers. float4/float8 → numbers, except NaN and ±Infinity, which
  stay as text.
- **bigint and numeric → text.** A JavaScript number would lose digits past 2^53 or a numeric's
  scale.
- Timestamps, dates, intervals, json/jsonb, arrays, uuid and everything else → PostgreSQL's own
  text output, exactly as `psql` shows it.
- bytea → `\x` plus the first 64 bytes as hex, then `… (N bytes)` when longer.
- Any text value over 64 KiB is cut (on a UTF-8 boundary) with a trailing `…`.

Column types are named the way `format_type()` names them; unknown OIDs (enums, domains) are
looked up after the rollback.

## §6 UI and deep links

Tabs in the existing native view, each with a stable path (`web/src/navigation.ts`; the Catalog
links here):

| Path | Page |
|---|---|
| `/database` | Overview (the ADR 0093 status page, unchanged) |
| `/database/tables` | Tables tab: schema, name, kind, "about N rows", columns; search over names and column names |
| `/database/tables/<schema>/<name>` | One table: columns (type, nullable, comment) and a 100-row preview |
| `/database/sql` | SQL console |
| `/database/sql?schema=<schema>&name=<name>` | SQL console prefilled with `SELECT * FROM "<schema>"."<name>" LIMIT 100` |

`<schema>` and `<name>` are each `encodeURIComponent`-encoded. Counts always say "about"; no
estimate shows "Unknown"; a view shows "—".

## §7 Known limits

- A single very large value is read whole into memory before it is cut. The byte cap bounds the
  response and the rows held, not one value's read. PostgreSQL's own limit is 1 GB per field.
- `COPY … TO STDOUT` returns no rows and `COPY … FROM STDIN` fails. The console shows result sets
  only.
- Statements such as `SET …` or `COMMIT` "succeed" and change nothing (rolled back, and the session
  is dropped).
- Foreign tables and individual partitions are not listed.
- **Not exercised end to end in a cluster.** The kind integration installs this chart without
  booth-core: it checks the BoothModule (with `events`) passes core's CRD schema and the pod starts
  with a stand-in bus credential. Neither booth-core's gateway in front of these routes nor
  booth-catalog consuming the events has been tested together with this module. The events are
  tested against a real nats-server with JetStream and a `BOOTH_EVENTS` stream shaped like core's.
