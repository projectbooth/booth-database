"""A Project Booth workspace's PostgreSQL database (ADR 0081), from OUTSIDE the cluster (ADR 0098).

**For use outside the cluster only**: a developer's machine, or an external job reaching the
workspace database through whatever network path the deployment provides. **In-cluster code
(notebooks, pipeline tasks, anything in a platform pod) does not use this package.** It uses
booth-core's credential sidecar and its ``DATABASE_URL`` (ADR 0095): ``booth.database.engine()`` in
a notebook, or any Postgres client on ``os.environ["DATABASE_URL"]`` in a pipeline task. The
notebook and task images don't ship this package, and it must not be installed into them, because
calling the broker from a kernel or task would put credential handling back into user code.

    import booth_database
    with booth_database.connect() as conn:                     # read-write (editor/owner)
        conn.execute("CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, body text)")

    booth_database.connect(read_only=True)                     # any workspace role
    pandas.read_sql("SELECT * FROM notes", booth_database.url())
    df.to_sql("notes_copy", booth_database.engine())            # with SQLAlchemy installed

Each call asks booth-core's credential broker (ADR 0080) for a short-lived credential for this
workspace's database only (an hour by default, booth-database's lease floor), as your own identity.
Configure it with ``BOOTH_TOKEN``, ``BOOTH_WORKSPACE`` and ``BOOTH_CREDENTIAL_BROKER_URL`` (see
``Database.from_env``).

**Not verified end to end from outside a cluster.** The tests cover this package against a real
booth-database provider and real PostgreSQL, with a stand-in for booth-core's broker, all on one
machine. Reaching the broker and the database from outside a real deployment, including whether the
credential's host is reachable from where you are, hasn't been tested (ADR 0098, open items).
"""

from __future__ import annotations

from .broker import READ, READWRITE, DatabaseError, HttpBroker, PostgresGrant
from .database import Database

__all__ = [
    "READ",
    "READWRITE",
    "Database",
    "DatabaseError",
    "HttpBroker",
    "PostgresGrant",
    "connect",
    "credentials",
    "engine",
    "url",
]
__version__ = "0.1.0"

_default: Database | None = None


def _db() -> Database:
    # Built on first use, not import, so importing never fails in an unconfigured environment.
    global _default
    if _default is None:
        _default = Database.from_env()
    return _default


def connect(read_only: bool = False, **kwargs):
    """A psycopg connection to this workspace's database. See ``Database.connect``."""
    return _db().connect(read_only, **kwargs)


def url(read_only: bool = False) -> str:
    """A short-lived ``postgresql://`` URL for this workspace's database. See ``Database.url``."""
    return _db().url(read_only)


def engine(read_only: bool = False, **kwargs):
    """A SQLAlchemy engine that renews credentials itself. See ``Database.engine``."""
    return _db().engine(read_only, **kwargs)


def credentials(read_only: bool = False) -> PostgresGrant:
    """The raw short-lived credential, for a driver this package doesn't wrap."""
    return _db().credentials(read_only)
