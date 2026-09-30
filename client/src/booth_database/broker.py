"""The requester side of the ADR 0080 credential broker for the ``postgres`` kind: the only place in
this package that knows the broker's wire shape (contracts/credential-broker.md, ADR 0088).

The request carries ``access`` as the shared top-level field core authorizes on, and a ``scope`` that
names the workspace only so a mismatch is refused rather than silently served — the workspace itself
is fixed by core from the caller's own token and ``X-Workspace`` header, never by the scope.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass, field
from datetime import datetime

READ, READWRITE = "read", "readwrite"


class DatabaseError(Exception):
    """Getting a credential failed. ``status`` is the HTTP status when there was one."""

    def __init__(self, message: str, status: int = 0) -> None:
        super().__init__(message)
        self.status = status


@dataclass(frozen=True)
class PostgresGrant:
    """One issued, short-lived credential for this workspace's database. ``repr`` never shows the
    password: a grant ends up in tracebacks and notebook output, and the contract says the value is
    never logged anywhere."""

    lease_id: str
    workspace: str
    access: str
    expires_at: float
    host: str
    port: int
    database: str
    username: str
    password: str = field(repr=False)
    sslmode: str = "prefer"

    def conninfo(self) -> dict[str, object]:
        """Keyword arguments for ``psycopg.connect`` (libpq parameter names)."""
        return {
            "host": self.host,
            "port": self.port,
            "dbname": self.database,
            "user": self.username,
            "password": self.password,
            "sslmode": self.sslmode,
        }

    @property
    def url(self) -> str:
        """A ``postgresql://`` URL for tools that want one (pandas, SQLAlchemy, psql). It embeds the
        password: pass it straight to the tool, don't print or store it."""
        q = urllib.parse.quote
        return f"postgresql://{q(self.username, safe='')}:{q(self.password, safe='')}@{self.host}:{self.port}/{q(self.database, safe='')}?sslmode={q(self.sslmode, safe='')}"

    def seconds_left(self, now: float | None = None) -> float:
        return self.expires_at - (now if now is not None else time.time())


class HttpBroker:
    """booth-core's ``POST /api/credentials``, authenticated as the caller (its own bearer token), so
    the broker's audit trail names who actually asked (ADR 0080)."""

    def __init__(self, url: str, token: Callable[[], str], opener=None, timeout: float = 30) -> None:
        if not url:
            raise DatabaseError(
                "no credential broker URL is configured: set BOOTH_CREDENTIAL_BROKER_URL "
                "(booth-core's <core>/api/credentials), or BOOTH_GATEWAY_URL"
            )
        self._url = url
        self._token = token
        self._open = opener or urllib.request.urlopen
        self._timeout = timeout

    def issue_postgres(self, workspace: str, access: str, ttl_seconds: int) -> PostgresGrant:
        if access not in (READ, READWRITE):
            raise ValueError(f"unknown access mode {access!r}")
        body = {"kind": "postgres", "access": access, "ttlSeconds": int(ttl_seconds), "scope": {"workspace": workspace}}
        req = urllib.request.Request(self._url, data=json.dumps(body).encode(), method="POST")
        req.add_header("Authorization", "Bearer " + self._token())
        req.add_header("X-Workspace", workspace)
        req.add_header("Content-Type", "application/json")
        try:
            with self._open(req, timeout=self._timeout) as resp:
                doc = json.loads(resp.read())
        except urllib.error.HTTPError as e:
            raise DatabaseError(_refusal(e, access), e.code) from None
        except (urllib.error.URLError, OSError) as e:
            raise DatabaseError(f"couldn't reach the credential broker: {e}") from None
        return _parse(doc, workspace, access)


def _refusal(e: urllib.error.HTTPError, access: str) -> str:
    try:
        raw = e.read().decode("utf-8", "replace")
    except OSError:
        raw = ""
    detail = raw.strip()
    try:  # a provider refusal is JSON {error, message}; core's own refusals are plain text
        doc = json.loads(raw)
        if isinstance(doc, dict):
            detail = str(doc.get("message") or doc.get("error") or raw)
    except ValueError:
        pass
    detail = detail[:300]
    if e.code == 401:
        return f"the platform didn't accept this session's token (HTTP 401): {detail}"
    if e.code == 403 and access == READWRITE:
        return (
            "your role in this workspace can't write to its database (viewers get read-only access): "
            "use read_only=True"
        )
    if e.code == 404:
        return "this deployment has no workspace database: the booth-database module isn't installed (HTTP 404)"
    if e.code in (502, 503):
        return f"the workspace database is unavailable right now (HTTP {e.code}): {detail}"
    return f"the credential broker refused a database credential (HTTP {e.code}): {detail}"


def _parse(doc: dict, workspace: str, access: str) -> PostgresGrant:
    """Turn a broker response into a grant, refusing anything that isn't what was asked for — the
    provider must refuse rather than widen (credential-broker.md), and this side checks the echo
    rather than trusting it blindly, the same discipline booth-lakehouse's client applies."""
    try:
        if doc.get("kind") != "postgres":
            raise DatabaseError(f"broker returned a {doc.get('kind')!r} credential for a postgres request")
        scope = doc.get("scope") or {}
        if (scope.get("workspace"), scope.get("access")) != (workspace, access):
            raise DatabaseError("broker returned a credential for a different workspace or access level than requested; refusing to use it")
        cred = doc["credential"]
        if cred["database"] != scope.get("database"):
            raise DatabaseError("broker response is inconsistent about which database it grants; refusing to use it")
        return PostgresGrant(
            lease_id=str(doc["leaseId"]),
            workspace=workspace,
            access=access,
            expires_at=_timestamp(doc["expiresAt"]),
            host=str(cred["host"]),
            port=int(cred["port"]),
            database=str(cred["database"]),
            username=str(cred["username"]),
            password=str(cred["password"]),
            sslmode=str(cred.get("sslMode") or "prefer"),
        )
    except (KeyError, TypeError, ValueError, AttributeError) as e:
        raise DatabaseError(f"malformed credential-broker response (missing or invalid {e})") from None


def _timestamp(value) -> float:
    if isinstance(value, (int, float)):
        return float(value)
    s = str(value).replace("Z", "+00:00")
    # Go emits up to nanosecond precision; Python < 3.11's fromisoformat only takes microseconds.
    if "." in s:
        head, _, rest = s.partition(".")
        n = len(rest) - len(rest.lstrip("0123456789"))  # the fraction's own digits, not the offset's
        s = head + "." + rest[:n][:6] + rest[n:]
    return datetime.fromisoformat(s).timestamp()
