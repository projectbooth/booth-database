# booth-database-client (`booth_database`)

A Python client for a Project Booth workspace's PostgreSQL database (ADR 0081), **for use from
outside the cluster only** (ADR 0098): a developer's machine, or an external job reaching the
workspace database through whatever network path the deployment provides.

## In the cluster, don't use this package

Notebooks, pipeline tasks and anything else running in a platform pod use booth-core's credential
sidecar (ADR 0095) and its `DATABASE_URL`, a credential-free loopback address:

```python
# In a notebook (booth-notebooks' `booth` package):
import booth.database, pandas as pd
engine = booth.database.engine()
pd.read_sql("SELECT now()", engine)

# In a pipeline task with platform access (the task image ships psycopg 3):
import os, psycopg
with psycopg.connect(os.environ["DATABASE_URL"]) as conn:
    conn.execute("SELECT now()").fetchone()
```

The notebook and pipeline-task images don't ship `booth_database`, and it must not be installed into
them. Calling the broker from inside a kernel or task would put credential handling back into user
code, which ADR 0095 removed. See the repository README's "Sessions, renewal and the sidecar" for
how long those connections last.

## Outside the cluster

```python
import booth_database

with booth_database.connect() as conn:                     # read-write: editor/owner
    conn.execute("CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, body text)")

booth_database.connect(read_only=True)                     # any workspace role
pandas.read_sql("SELECT * FROM notes", booth_database.url())
df.to_sql("notes_copy", booth_database.engine())            # pip install 'booth-database-client[sqlalchemy]'
```

Each call asks booth-core's credential broker (`POST /api/credentials`, ADR 0080/0088) for a
`postgres` credential for this workspace's database only, as your own identity. It's good for an
hour by default (booth-database's lease floor), and a connection ends when the credential it opened
with expires. Every `connect()` gets a fresh credential, and `engine()` replaces pooled connections
before theirs expire.

Configuration, from the environment:

| Variable | What |
|---|---|
| `BOOTH_TOKEN` | Your platform bearer token (the broker authenticates you with it) |
| `BOOTH_WORKSPACE` | The workspace slug |
| `BOOTH_CREDENTIAL_BROKER_URL` | booth-core's `<core>/api/credentials`; or set `BOOTH_GATEWAY_URL` (`<core>/modules`) and it's derived |

## Not verified end to end

The tests (`tests/`) exercise this package against a real booth-database provider binary and real
PostgreSQL, with a stand-in for booth-core's broker, all on one machine. **Using it from outside a
real deployment hasn't been tested** (ADR 0098 lists it as an open item). Two things in this repo's
own chart are known to stand in the way unless the deployment provides for them:

- The credential's `host` is the bundled server's in-cluster DNS name unless the chart sets
  `client.host`/`client.port` to an address reachable from outside.
- The bundled server's NetworkPolicy admits only labelled in-cluster namespaces.

Whether a given deployment exposes booth-core's broker and the database to outside callers is its
own decision. This package doesn't provide that path.
