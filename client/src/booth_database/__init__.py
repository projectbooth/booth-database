"""Your Project Booth workspace's own PostgreSQL database, with simple commands (ADR 0081):

    import booth_database
    with booth_database.connect() as conn:                     # read-write (editor/owner)
        conn.execute("CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, body text)")
        conn.execute("INSERT INTO notes (body) VALUES (%s)", ["hello"])

    booth_database.connect(read_only=True)                     # any workspace role
    pandas.read_sql("SELECT * FROM notes", booth_database.url())
    df.to_sql("notes_copy", booth_database.engine())            # with SQLAlchemy installed

No password to handle: each call asks booth-core's credential broker (ADR 0080) for a credential
good for a few minutes, for this workspace's database only, as this notebook or pipeline run's own
identity. Configuration comes from the environment a platform workload already has — see
``Database.from_env``.
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
