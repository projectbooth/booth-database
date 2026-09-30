"""The workspace database, as simple commands: ``connect()``, ``url()``, ``engine()``.

Every call asks the credential broker for a fresh short-lived credential (ADR 0080) for *this*
workspace's database, as *this* caller, and uses it immediately. A credential lives for
booth-database's lease floor — one hour by default (ADR 0089 §3; the broker's 5-minute ceiling only
bounds what is *asked* for, and the provider clamps it up). Nothing is cached: a connection's usable
lifetime is bounded by the credential it was opened with (booth-database ends sessions whose
credential has expired), so a fresh credential per connection gives every connection the longest
possible life, and there is never a stale secret sitting around.
"""

from __future__ import annotations

import os
import threading
import time
from collections.abc import Callable

from .broker import READ, READWRITE, DatabaseError, HttpBroker, PostgresGrant

# What to *ask* for: booth-core's broker ceiling (ADR 0088) — asking for more is clamped down anyway,
# and booth-database clamps it back up to its own floor (one hour by default). Always read a grant's
# real lifetime from its expires_at, never from this.
DEFAULT_TTL_SECONDS = 300

# engine() retires a pooled connection this long before its credential expires.
EXPIRY_MARGIN_SECONDS = 60


class Database:
    def __init__(
        self,
        broker_url: str,
        workspace: str,
        token: Callable[[], str] | str,
        ttl_seconds: int = DEFAULT_TTL_SECONDS,
        opener=None,
    ) -> None:
        if not workspace:
            raise DatabaseError("no workspace: set BOOTH_WORKSPACE, or run inside a Project Booth notebook")
        self.workspace = workspace
        self._token = token if callable(token) else (lambda: token)
        self._ttl = int(ttl_seconds)
        self._broker = HttpBroker(broker_url, self._token, opener)

    @classmethod
    def from_env(cls, env=None, **kwargs) -> Database:
        """Configured from what a platform workload already has: ``BOOTH_WORKSPACE``,
        ``BOOTH_CREDENTIAL_BROKER_URL`` (or ``BOOTH_GATEWAY_URL``, from which it is derived), and a
        token from ``BOOTH_TOKEN`` or — inside a booth-notebooks kernel — the notebook's own
        platform token (``booth.platform_token``). The same variables booth_lakehouse reads."""
        env = os.environ if env is None else env
        notebook = _notebook()
        token: Callable[[], str] | str | None = env.get("BOOTH_TOKEN") or (notebook[0] if notebook else None)
        if not token:
            raise DatabaseError("no platform token: set BOOTH_TOKEN, or run inside a Project Booth notebook")
        workspace = env.get("BOOTH_WORKSPACE") or (notebook[1] if notebook else "")
        return cls(broker_url_from_env(env), workspace, token, **kwargs)

    def credentials(self, read_only: bool = False) -> PostgresGrant:
        """A fresh credential for this workspace's database. ``read_only=True`` asks for read access,
        which any workspace role may have; read-write needs editor or owner."""
        return self._broker.issue_postgres(self.workspace, READ if read_only else READWRITE, self._ttl)

    def connect(self, read_only: bool = False, **kwargs):
        """A psycopg connection to this workspace's database, e.g.::

            with booth_database.connect() as conn:
                conn.execute("CREATE TABLE IF NOT EXISTS notes (id serial PRIMARY KEY, body text)")

        Extra keyword arguments go to ``psycopg.connect``. The connection is usable for up to the
        credential's lifetime (an hour by default); open a new one for later work rather than holding one."""
        return self._open(read_only, **kwargs)[0]

    def _open(self, read_only: bool, **kwargs):
        try:
            import psycopg
        except ImportError:
            raise DatabaseError("connect() needs psycopg: pip install 'psycopg[binary]'") from None
        grant = self.credentials(read_only)
        params = {**grant.conninfo(), "application_name": "booth-database-client", **kwargs}
        return psycopg.connect(**params), grant

    def url(self, read_only: bool = False) -> str:
        """A ``postgresql://`` URL with a fresh credential embedded, for tools that take one (e.g.
        ``pandas.read_sql(query, booth_database.url())``). Valid until the grant expires (an hour by default)."""
        return self.credentials(read_only).url

    def engine(self, read_only: bool = False, **kwargs):
        """A SQLAlchemy engine whose every new pooled connection gets its own fresh credential, and
        which retires each connection shortly before *its own* credential expires (read from the
        grant, not assumed) — so it can be held for as long as you like (e.g. passed to
        ``DataFrame.to_sql``). Needs SQLAlchemy."""
        try:
            import sqlalchemy
            from sqlalchemy import event, exc
        except ImportError:
            raise DatabaseError("engine() needs SQLAlchemy: pip install 'booth-database-client[sqlalchemy]'") from None

        # The creator can't see the pool record, so hand the new connection's expiry to the
        # "connect" event (which fires right after it, on the same thread) via a thread-local.
        pending = threading.local()

        def creator():
            conn, grant = self._open(read_only)
            pending.expires_at = grant.expires_at
            return conn

        eng = sqlalchemy.create_engine("postgresql+psycopg://", creator=creator, **{"pool_pre_ping": True, **kwargs})

        @event.listens_for(eng, "connect")
        def _record_expiry(dbapi_conn, record):
            record.info["booth_expires_at"] = getattr(pending, "expires_at", None)

        @event.listens_for(eng, "checkout")
        def _retire_expiring(dbapi_conn, record, proxy):
            expires_at = record.info.get("booth_expires_at")
            if expires_at is not None and expires_at - time.time() < EXPIRY_MARGIN_SECONDS:
                # The pool discards this connection and opens a fresh one (with a fresh credential).
                raise exc.DisconnectionError("booth-database credential about to expire")

        return eng


def broker_url_from_env(env) -> str:
    """``BOOTH_CREDENTIAL_BROKER_URL`` if set; otherwise derived from ``BOOTH_GATEWAY_URL``
    (``<core>/modules`` -> ``<core>/api/credentials``) — both are booth-core's."""
    url = env.get("BOOTH_CREDENTIAL_BROKER_URL", "")
    if url:
        return url
    gateway = env.get("BOOTH_GATEWAY_URL", "").rstrip("/")
    if gateway.endswith("/modules"):
        return gateway[: -len("/modules")] + "/api/credentials"
    return ""


def _notebook() -> tuple[Callable[[], str], str] | None:
    """Inside a booth-notebooks kernel, the notebook's own platform identity via its ``booth``
    package's public ``platform_token`` (ADR 0084) — never the person's browser login."""
    try:
        import booth  # type: ignore[import-not-found]
    except ImportError:
        return None
    token = getattr(booth, "platform_token", None)
    if not callable(token):
        return None
    return token, str(getattr(booth, "workspace", "") or "")
