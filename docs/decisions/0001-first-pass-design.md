# 0001: First-pass design, and the judgment calls it made

Status: **implemented, awaiting coordinator ratification** (2026-09-29). Everything below marked
**Judgment call** was decided in this module's own session because the brief (`agent-briefs/database.md`)
or ADR 0081 explicitly left it open, or because building surfaced it. None of them is a
cross-cutting change; each is flagged so it can be ratified or overturned, not quietly assumed.

## What was built

- **`booth-database` service** (Go, `cmd/database`): the `postgres`-kind provider for the ADR 0080
  credential broker at the fixed `POST /internal/credentials` path, authenticated exactly like
  `booth-storage`'s `s3` provider (constant-time compare against the delivered
  `booth-credential-broker-provider-credentials` Secret). Plus `/healthz` (readiness, what core
  polls) and `/livez`. No other routes.
- **Provisioning** (`internal/provision`): one database per workspace, created on first use;
  a fresh login role per lease; a reaper that ends expired leases.
- **Helm chart**: `mode: bundled` (its own `postgres:16-alpine` StatefulSet — not booth-core's
  instance — plus a nightly backup CronJob) or `mode: external` (operator-supplied host/role/Secret).
  Manifest: `id: database`, `hasOwnUi: false`, `providesCredentials: {kinds: [postgres]}`, and
  deliberately **no** `database:` field (that's ADR 0053's module-internal Postgres).
- **Python client** (`client/`, package `booth-database-client`, import `booth_database`):
  `booth_database.connect()`, `connect(read_only=True)`, `url()`, `engine()`, `credentials()`.
  **Note (2026-10-06):** no longer the in-cluster path. Since ADR 0095, notebooks and pipeline
  tasks use booth-core's credential sidecar (`DATABASE_URL`), and their images don't ship this
  client. Access there follows the workspace role at start, with no `read_only` switch. See the
  README. Whether `client/` stays is a pending architecture decision.

## §1 Judgment call: database per workspace, not schema per workspace

**Chosen: one database per workspace.** The brief's own framing of the trade-off holds up, and
building it found a deciding reason the brief didn't name:

- In PostgreSQL, `pg_class`/`pg_namespace`/`pg_proc` are per-database catalogs readable by every
  connected role. With schema-per-workspace in one shared database, workspace A's credential could
  list workspace B's table and column names (not data) just by querying the catalog. That's a real
  cross-tenant disclosure, and there is no supported way to hide those catalogs from a role.
  Database-per-workspace moves the boundary to connection time: B's credential can't open a session
  on A's database at all (CONNECT is revoked from PUBLIC; verified, SQLSTATE 42501).
- Per-workspace backup/restore is a plain `pg_dump`/`pg_restore -C` of one database (verified).
- Extensions, search paths and ownership are per database, so one workspace can't affect another's.
- Cost: connection pools are per database, and a server with thousands of workspaces carries
  thousands of databases. Fine at this platform's scale, and databases are only created for
  workspaces that actually ask for one (§4).

## §2 Judgment call: database names are a hash of the slug, not the slug

`bdb_ws_<first 24 hex of sha256("booth-database/workspace/" + slug)>`. `pg_database`, `pg_roles` and
`pg_shdescription` are shared catalogs visible from *any* database on the server, so slug-named
databases would let any workspace's credential list every other workspace on the deployment. That's
the same enumeration ADR 0052 already rules out for the user directory. The slug is never written
into PostgreSQL at all (tested). What's still visible: *how many* workspaces have a database.
Operators map a slug to its database with
`printf 'booth-database/workspace/%s' <slug> | sha256sum | cut -c1-24`. The hash is unkeyed, so
someone who already has a list of candidate slugs can confirm which ones have a database. Accepted
for v0; a keyed HMAC would close that, at the cost of a key that becomes as critical as the data.

## §3 The access model (how "short-lived, workspace-scoped" is actually enforced)

- Per workspace: NOLOGIN groups `<db>_rw` (owns the database and everything created in it) and
  `<db>_ro` (SELECT on everything `_rw` creates, via default privileges, including schemas created
  later).
- Per lease: a fresh `LOGIN NOINHERIT` role named `bdb_lease_<leaseId>` (it joins with core's audit
  trail and this module's audit log line), member of exactly one group, with CONNECT granted
  directly, `CONNECTION LIMIT 10`, `VALID UNTIL` = expiry, and a per-role default `role = <group>`, so
  every session runs as the group and everything it creates is owned by the group, not the lease.
  `SET ROLE NONE` drops to the bare lease role, which has no privileges at all (tested). A read lease
  also defaults to `default_transaction_read_only`, belt and braces over the group simply having no
  write grants.
- The password is sent to PostgreSQL only as a precomputed **SCRAM-SHA-256 verifier**, never as
  plaintext. `CREATE ROLE ... PASSWORD '<plaintext>'` would otherwise land in any statement log,
  `pg_stat_statements`, or an operator's audit extension, breaking the contract's "never written
  to any log". The plaintext exists only in the broker response. Backups use
  `--no-role-passwords`, so verifiers don't end up there either.
- **Expiry is enforced twice**: PostgreSQL itself refuses a new login once `VALID UNTIL` has passed
  (tested: 28P01), and a reaper (every `leases.reapInterval`, default 10s) terminates any session
  still open on an expired lease and drops the role (tested, in-process and on a deployed cluster).

**Judgment call, with a real product consequence to flag:** a session opened under a credential dies
within one reap interval of that credential's expiry. With ADR 0088's 5-minute broker ceiling, **no
single database session can run longer than ~5 minutes**, including a long query, a long
transaction, or an idle notebook connection. This is the literal reading of ADR 0080 ("an issued
credential actually expires and stops working after its TTL", and the brief's testing ask), and the
safer default. The client works around it where it can: every `connect()` gets a fresh credential,
and `engine()` recycles pooled connections before expiry. But a single long-running query can't be
worked around client-side. Options if this bites (not built, for the coordinator): (a) let expiry
only block *new* logins and leave open sessions alone (PostgreSQL's native `VALID UNTIL`
semantics), (b) a longer postgres-kind TTL ceiling in core, (c) a provider-side
`options.sessionGrace`. **I'd recommend (a) or (b) be decided explicitly, not discovered by a
pipeline author.**

**Resolved (ADR 0089 §3, built in 0003):** neither (a) nor (b). The provider clamps every lease up to
its own one-hour floor, so sessions can run up to an hour, with enforcement unchanged.

## §4 Judgment call: provisioning is lazy, on a workspace's first credential request

No workspace-lifecycle event or listing exists for a module to act on (workspaces are just ADR 0025
group-claim values), so a workspace's database is created the first time anyone in it asks for a
credential, idempotently and safely under concurrency (session advisory lock; tested with six
simultaneous "replicas"). A half-finished provision (crash between steps) is completed on the next
request (a ready marker is written last). **Not handled:** workspace *deletion*. Nothing tells this
module a workspace is gone, so its database stays. Same gap as every module holding per-workspace
state; flagged, not solved here.

## §5 The postgres-kind wire shape (this module's to define; the contract leaves it opaque)

```
scope:   {"workspace": "<slug>"}   # optional; if present must equal the broker-forwarded workspace
options: none                       # any non-empty options -> 422 scope_not_supported
201 scope echo: {"workspace", "database", "access"}
credential:     {"host", "port", "database", "username", "password", "sslMode"}
```

The workspace always comes from core's `requester.workspace`, never from the scope. The scope may
only restate it. **Unknown scope fields are refused (422), not ignored**, because a field this
version doesn't understand (say, a future `schema` or `table`) might have been meant to narrow the
grant, and ignoring it would hand out something broader than asked ("refuse rather than widen").
The client checks the echo (workspace, access, database consistency) and refuses a mismatched
grant, following booth-lakehouse's client.

## §6 Judgment calls: networking and TLS

- `credential.host` is the bundled Service's cluster DNS name, or `external.host`. Either can be
  overridden with `client.host`/`client.port`/`client.sslMode` for when workloads reach the server
  at a different address than the module does.
- **The bundled server has no TLS** (`sslMode: disable`): in-cluster traffic only, SCRAM
  authentication (so no password crosses the wire in plaintext), but the query traffic itself is
  unencrypted. Same posture as booth-core's bundled Postgres. External mode defaults to
  `sslMode: require`. Worth tying into ARCHITECTURE §7 item 37's production-readiness list.
- ~~No NetworkPolicy shipped.~~ **Superseded by 0002 §1**: the bundled server now ships one
  (booth-core's `database-client` namespace-label pattern), per the coordinator's review.

## §7 Answers to the brief's three open questions

1. **Isolation unit:** database per workspace (§1).
2. **Connection pooling:** **no PgBouncer in v0.** The per-lease-role model is fundamentally at odds
   with a pooler: PgBouncer pools per (user, database), and every lease is a new user, so it would
   pool nothing while adding an auth layer (`auth_query`) that has to see every lease's verifier.
   Transaction pooling would also break session state notebook users rely on. Load is bounded
   instead: `leases.connectionLimit` (10) per credential, `bundled.maxConnections` (200) per server.
   Revisit if concurrent-connection load actually approaches that. The realistic path then is
   pooling per *group* role with the lease authenticating to the pooler, which is real design work.
3. **Backup/restore:** mirrors ADR 0054, bundled mode only. A nightly CronJob runs
   `pg_dumpall --globals-only --no-role-passwords` plus one `pg_dump -Fc` per workspace database into
   a timestamped directory, marked complete only once every dump has finished, with 7-day retention.
   External clusters are the operator's responsibility. **Verified:** the script against real
   Postgres; a drop-and-`pg_restore -C` restore round trip (data, the no-PUBLIC ACL, and the ready
   marker all survive); a CronJob-triggered run on a kind cluster. **Not done:** an S3 destination
   (booth-core's backup has one; this only writes to a PVC on the same storage as the database, so it
   doesn't survive losing that node). Restore runbook: README.

## §8 Judgment call: no UI and no user-facing API in v0

**Superseded (ADR 0093, built in 0004):** a read-only native admin view and its owner-only API now exist.

The brief places this under Manage, but nothing in the v0 definition of done needs a view, and the
only access path is the broker. So `hasOwnUi: false` (as booth-lakehouse did) and no `/api/*` routes
at all. A side effect: this module never has to verify an end-user OIDC token, since the one
privileged route is authenticated by core's provider credential. An obvious later admin view
(per-workspace size, drop a workspace's database) is left open.

## §9 Smaller calls

- **`credentialBroker.enabled` defaults to `true`**, unlike booth-storage's opt-in, because here the
  broker is the *only* access path; off means installed but useless.
- **The client is its own package (`booth_database`), not `booth.database` inside booth-notebooks'
  `booth` package.** The brief's `booth.database.connect()` was "e.g."; `booth` belongs to
  booth-notebooks, and booth-lakehouse set the precedent of a separate client that reuses
  `booth.platform_token` when present. Adding a `booth.database` shim is a small, routed item for
  booth-notebooks once this client is published (ARCHITECTURE §7 item 43's distribution question
  applies here too: this client isn't installable from anywhere yet).
- **The client never caches a credential.** A fresh one per `connect()` gives each connection the
  longest possible life under §3's expiry model, and leaves no stale secret in memory.
- **Default access is read-write**; a viewer gets a clear "use read_only=True" error from the 403.
- ~~Provider-side TTL cap of 5m, no provider floor.~~ **Superseded by 0003**: a one-hour floor
  (`leases.minTTL`), with the cap defaulting to it.
- **Bundled admin Secret is `lookup`-preserved and `resource-policy: keep`** (verified across a real
  `helm upgrade`), because the data PVC outlives an uninstall and a regenerated password would lock
  the module out of its own data.
- **The backup volume is a `volumeClaimTemplate` on the Postgres StatefulSet**, not a release PVC.
  The first real `helm install --wait` on kind hung on a PVC nobody mounts (WaitForFirstConsumer).
  booth-core hit the same issue and creates its PVC through the Kubernetes API at runtime; this module
  has no API access, so the claim is bound through the StatefulSet instead.

## Open items for booth-core / the coordinator

1. ~~**ADR 0083 exposure.**~~ **Resolved by ADR 0090, built in 0002 §2** (duplicate core's pin). The bundled StatefulSet uses the cluster-default storage class (local-path
   on k3s) exactly as booth-core's did before ADR 0083. On `node_count=2` it has the same exposure.
   booth-core fixed its own with a runtime node-affinity patch (`internal/dbprov.EnsureNodeAffinity`)
   that this chart doesn't replicate (it has no Kubernetes API access). `bundled.nodeSelector`/`affinity`
   passthroughs exist for an operator to pin it. Should this be a shared mechanism, or is a chart-level
   pin acceptable here?
2. **§3's session-lifetime consequence** needs an explicit ruling before pipelines adopt the client.
3. **booth-lakehouse's `HttpBroker` still nests `access` inside `scope`**
   (`client/src/booth_lakehouse/broker.py`), its provisional pre-ADR-0088 shape. Real core requires
   top-level `access` (`validate()` → 400), so that client will fail against the real broker. Seen
   while reading it as reference; not this module's to fix.
4. **Client distribution** (ARCHITECTURE §7 item 43) now has a second consumer.

## How this was verified

- **Real PostgreSQL 16** (`internal/provision`, `internal/server`): workspace isolation (catalog
  invisibility, CONNECT refused across workspaces and to maintenance databases, SET ROLE into
  another workspace refused), no slug in shared catalogs, read leases can't write (including by
  turning off read-only mode or via temp tables), the bare lease role has no privileges, expiry
  (new login refused past expiry, open session terminated by the reaper, role dropped, created
  objects survive), live leases untouched by the reaper, concurrent first-use provisioning, and the
  full HTTP provider path with core's exact `providerRequest` JSON.
- **Client end to end** (`client/tests/test_end_to_end.py`): `booth_database` → a stand-in for core's
  broker (same authorize/clamp/forward/relay behaviour as `internal/credentialbroker/service.go`,
  with a fixed token table in place of OIDC) → the real compiled provider binary → real PostgreSQL.
  Covers connect/create/read, viewer refused read-write (403 from the broker's own rule), isolation,
  expiry, SQLAlchemy `engine()`, and no issued password in the provider's log.
- **Real cluster** (`hack/kind-integration.sh`, run locally on kind): the chart installed with
  `--wait` in bundled mode, the manifest accepted by **booth-core's real `BoothModule` CRD schema**,
  and an in-cluster Job running the published client over cluster DNS (4/4 checks). A
  CronJob-triggered backup, the admin password kept across `helm upgrade`, and data surviving a
  Postgres pod restart were also checked.
- **Not done: booth-core's actual broker process in the loop.** Its routing needs its CRD controller,
  its OIDC verifier, and a real caller identity: the booth-e2e tier (ADR 0055), where this module
  should be added. booth-storage recorded the same limit for its provider. The stand-in reproduces
  core's documented behaviour, but it is a reproduction.
