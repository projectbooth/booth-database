# booth-database

Project Booth's optional workspace database (ADR 0081): one real PostgreSQL database per workspace,
on a bundled server or one you run, reached by a workspace's own code (notebooks, pipeline tasks)
with a single call and no password to handle:

```python
import booth_database

with booth_database.connect() as conn:                    # read-write: editor/owner
    conn.execute("CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, body text)")
    conn.execute("INSERT INTO notes (body) VALUES (%s)", ["hello"])

booth_database.connect(read_only=True)                    # any workspace role
pandas.read_sql("SELECT * FROM notes", booth_database.url())
df.to_sql("notes_copy", booth_database.engine())           # SQLAlchemy; renews credentials itself
```

Each call asks booth-core's credential broker (ADR 0080/0088) for a `postgres` credential good for a
short time (one hour by default: booth-database's lease floor, ADR 0089), for this workspace's database only, as the caller's own identity. This module is the
broker's `postgres`-kind **provider**: it mints a fresh short-lived login role per request and never
hands out its own admin credential. Design, trade-offs and open items:
[`docs/decisions/0001-first-pass-design.md`](docs/decisions/0001-first-pass-design.md).

**Not** booth-core's bundled Postgres (ADR 0053/0054), which holds each *module's* small internal
state. This module runs its own server (or uses yours) and never declares the manifest's `database`
field.

## Layout

| Path | What |
|---|---|
| `cmd/database`, `internal/` | The provider service (Go): `POST /internal/credentials`, `/healthz`, `/livez` |
| `internal/provision` | Per-workspace databases, lease roles, the expiry reaper |
| `internal/nodepin` | Pins the bundled StatefulSet to its node (ADR 0090) |
| `charts/booth-database` | Helm chart: `mode: bundled` or `mode: external`, backup CronJob, `BoothModule` |
| `client/` | `booth-database-client` (Python, `import booth_database`) |
| `test/contract`, `test/integration`, `hack/` | Chart contract tests, kind-cluster integration |

## Install

```sh
# Bundled: this chart's own PostgreSQL StatefulSet + nightly backups.
helm install booth-database charts/booth-database -n booth-database --create-namespace

# External: your cluster. The admin role needs CREATEDB, CREATEROLE and pg_signal_backend
# (a superuser works; the least-privilege setup is not yet tested; see docs/decisions/0001).
kubectl -n booth-database create secret generic pg-admin --from-literal=password=...
helm install booth-database charts/booth-database -n booth-database --create-namespace \
  --set mode=external --set external.host=pg.example.internal \
  --set external.username=booth_admin --set external.passwordSecret.name=pg-admin
```

In bundled mode the server is also pinned to its node once first scheduled (ADR 0090; a Role
scoped to that one StatefulSet and pod), and a NetworkPolicy admits only this module, its backup Job,
and namespaces labelled `booth.projectbooth.io/database-client=true`, which booth-core applies to
every module declaring `database`. **A client pod's own egress policy must also allow port 5432**:
booth-notebooks' and booth-pipeline's defaults don't yet (docs/decisions/0002 §1).

booth-core then delivers `booth-credential-broker-provider-credentials` into the namespace and starts
routing `postgres` requests here. The pod waits in `CreateContainerConfigError` until it does.

Workloads find the broker from `BOOTH_CREDENTIAL_BROKER_URL` (or derive it from `BOOTH_GATEWAY_URL`),
their workspace from `BOOTH_WORKSPACE`, and their identity from `BOOTH_TOKEN` or, inside a
booth-notebooks kernel, the notebook's own `booth.platform_token`: the same variables `booth_lakehouse` reads.

## The `postgres` credential kind

```
POST <core>/api/credentials   Authorization: Bearer <token>   X-Workspace: <slug>
{"kind": "postgres", "access": "read" | "readwrite", "ttlSeconds": 300, "scope": {"workspace": "<slug>"}}

201 {"leaseId", "kind": "postgres", "expiresAt",
     "scope": {"workspace", "database", "access"},
     "credential": {"host", "port", "database", "username", "password", "sslMode"}}
```

`scope.workspace` is optional and must match the request's workspace. Any other scope field, or any
`options`, is refused with 422 `scope_not_supported` rather than ignored. `readwrite` needs
editor/owner (core's rule).

**Lifetime.** The broker caps what a caller may *ask* for at 5 minutes (ADR 0088), and this provider
clamps every shorter request **up to its floor, `leases.minTTL` (default 1 hour)**. That's the same
mechanism booth-storage uses for MinIO's 15-minute floor (ADR 0089 §3). `expiresAt` in the response
is always the real expiry. A credential stops authenticating at `expiresAt`, and any session still
open on it is ended within `leases.reapInterval` (default 10s). So **no single session outlives its
credential (one hour by default)**: longer work should open new connections, which `engine()`
does for you. Why one hour: docs/decisions/0003.

## Backup and restore (bundled mode)

A CronJob (`backup.schedule`, default nightly) writes `/backups/<UTC timestamp>/` on the StatefulSet's
`backups-<release>-booth-database-postgres-0` volume: `globals.sql` (roles and grants, no passwords)
plus `<database>.dump` per workspace, kept `backup.retentionDays` (7). This is a baseline like
ADR 0054's, not point-in-time recovery, and the backups sit on the same storage as the database.

Workspace databases are named by hash (docs/decisions/0001 §2). To find one:

```sh
printf 'booth-database/workspace/%s' acme-analytics | sha256sum | cut -c1-24   # -> bdb_ws_<that>
```

To restore one workspace, from a shell in the Postgres pod as the admin role:

```sh
psql -d postgres -c 'DROP DATABASE IF EXISTS "bdb_ws_<hash>"'   # if it still exists
pg_restore -C -d postgres /backups/<timestamp>/bdb_ws_<hash>.dump
```

`-C` recreates the database with its owner, ACL (no PUBLIC access) and ready marker. The group roles
it references still exist on a surviving server. On a rebuilt server, apply `globals.sql` first.
Warnings about missing `bdb_lease_*` roles are expected and harmless: those were short-lived
credentials that existed at backup time.

## Development

```sh
docker compose -f hack/docker-compose.yml up -d --wait     # real PostgreSQL 16
eval "$(sh hack/test-env.sh)"
go test ./...                                              # unit + contract + real-Postgres tests
(cd client && pip install -e ".[dev,sqlalchemy]" && pytest) # client unit + end to end (builds the provider)
bash hack/kind-integration.sh                              # layer 3: the chart on a kind cluster
```

CI (`.github/workflows/ci.yml`) runs all but the last on every push/PR with
`BOOTH_TEST_REQUIRE_POSTGRES=1`, so a missing database fails rather than skips. `integration.yml`
runs the kind script on merge to `main` and nightly.
