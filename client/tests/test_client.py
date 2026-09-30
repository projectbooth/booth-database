"""Unit tests for the client: the broker wire shape, echo checking, refusal messages and environment
wiring, against a fake opener — no network, no database."""

from __future__ import annotations

import io
import json
import time
import urllib.error

import pytest

import booth_database
from booth_database import Database, DatabaseError, PostgresGrant
from booth_database.database import broker_url_from_env

BROKER = "http://core.booth.svc/api/credentials"


def response(workspace="acme", access="readwrite", **over):
    doc = {
        "leaseId": "11111111-2222-3333-4444-555555555555",
        "kind": "postgres",
        "expiresAt": "2026-09-29T12:05:00.123456789Z",
        "scope": {"workspace": workspace, "database": "bdb_ws_0123456789abcdef01234567", "access": access},
        "credential": {
            "host": "booth-database-postgres.booth-database.svc.cluster.local",
            "port": 5432,
            "database": "bdb_ws_0123456789abcdef01234567",
            "username": "bdb_lease_11111111222233334444555555555555",
            "password": "p@ss/word:with?chars",
            "sslMode": "disable",
        },
    }
    doc.update(over)
    return doc


class FakeOpener:
    def __init__(self, doc=None, error=None):
        self.doc, self.error, self.requests = doc, error, []

    def __call__(self, req, timeout=None):
        self.requests.append(req)
        if self.error:
            raise self.error
        return io.BytesIO(json.dumps(self.doc).encode())


def http_error(code, body):
    raw = body if isinstance(body, bytes) else json.dumps(body).encode()
    return urllib.error.HTTPError(BROKER, code, "err", {}, io.BytesIO(raw))


def db(opener, token="tok"):
    return Database(BROKER, "acme", token, opener=opener)


def test_request_shape():
    opener = FakeOpener(response())
    db(opener).credentials()
    req = opener.requests[0]
    assert req.full_url == BROKER and req.get_method() == "POST"
    assert req.get_header("Authorization") == "Bearer tok"
    assert req.get_header("X-workspace") == "acme"
    body = json.loads(req.data)
    # `access` is core's shared top-level field (ADR 0088), not nested in scope.
    assert body == {"kind": "postgres", "access": "readwrite", "ttlSeconds": 300, "scope": {"workspace": "acme"}}


def test_read_only_asks_for_read():
    opener = FakeOpener(response(access="read"))
    g = db(opener).credentials(read_only=True)
    assert json.loads(opener.requests[0].data)["access"] == "read"
    assert g.access == "read"


def test_grant_parsing_and_conninfo():
    g = db(FakeOpener(response())).credentials()
    assert g.lease_id.startswith("1111")
    assert g.conninfo() == {
        "host": "booth-database-postgres.booth-database.svc.cluster.local",
        "port": 5432,
        "dbname": "bdb_ws_0123456789abcdef01234567",
        "user": "bdb_lease_11111111222233334444555555555555",
        "password": "p@ss/word:with?chars",
        "sslmode": "disable",
    }
    # Nanosecond timestamps from Go parse on every supported Python version.
    assert abs(g.expires_at - 1790683500.123456) < 1e-3


def test_url_escapes_the_password():
    g = db(FakeOpener(response())).credentials()
    assert "p%40ss%2Fword%3Awith%3Fchars@" in g.url
    assert g.url.startswith("postgresql://bdb_lease_") and g.url.endswith("?sslmode=disable")


def test_repr_never_shows_the_password():
    g = db(FakeOpener(response())).credentials()
    assert "p@ss" not in repr(g) and "p@ss" not in str(g)


def test_token_callable_is_called_per_request():
    calls = []

    def token():
        calls.append(1)
        return f"tok{len(calls)}"

    opener = FakeOpener(response())
    d = db(opener, token=token)
    d.credentials()
    d.credentials()
    assert [r.get_header("Authorization") for r in opener.requests] == ["Bearer tok1", "Bearer tok2"]


@pytest.mark.parametrize(
    "doc",
    [
        response(workspace="globex"),  # another workspace
        response(access="read"),  # asked for readwrite
        response(kind="s3"),
        {**response(), "credential": {**response()["credential"], "database": "bdb_ws_other"}},
        {k: v for k, v in response().items() if k != "credential"},
    ],
)
def test_refuses_anything_but_what_was_asked_for(doc):
    with pytest.raises(DatabaseError):
        db(FakeOpener(doc)).credentials()


@pytest.mark.parametrize(
    "code,body,access,expect",
    [
        (403, b"caller's role does not permit the requested access\n", False, "read_only=True"),
        (404, b"no module provides this credential kind\n", False, "isn't installed"),
        (422, {"error": "scope_not_supported", "message": "the postgres kind takes no options"}, False, "takes no options"),
        (503, b"credential provider is temporarily unavailable\n", False, "unavailable"),
        (502, {"error": "mint_failed"}, True, "unavailable"),
        (401, b"invalid token\n", False, "token"),
    ],
)
def test_refusal_messages(code, body, access, expect):
    d = db(FakeOpener(error=http_error(code, body)))
    with pytest.raises(DatabaseError) as exc:
        d.credentials(read_only=access)
    assert expect in str(exc.value) and exc.value.status == code


def test_unreachable_broker():
    with pytest.raises(DatabaseError, match="couldn't reach"):
        db(FakeOpener(error=urllib.error.URLError("refused"))).credentials()


def test_broker_url_from_env():
    assert broker_url_from_env({"BOOTH_CREDENTIAL_BROKER_URL": "http://x/api/credentials"}) == "http://x/api/credentials"
    assert broker_url_from_env({"BOOTH_GATEWAY_URL": "http://core.booth.svc:8080/modules/"}) == "http://core.booth.svc:8080/api/credentials"
    assert broker_url_from_env({"BOOTH_GATEWAY_URL": "http://elsewhere"}) == ""
    assert broker_url_from_env({}) == ""


def test_from_env():
    d = Database.from_env({"BOOTH_TOKEN": "t", "BOOTH_WORKSPACE": "acme", "BOOTH_GATEWAY_URL": "http://core/modules"})
    assert d.workspace == "acme"
    with pytest.raises(DatabaseError, match="no platform token"):
        Database.from_env({"BOOTH_WORKSPACE": "acme", "BOOTH_GATEWAY_URL": "http://core/modules"})
    with pytest.raises(DatabaseError, match="no workspace"):
        Database.from_env({"BOOTH_TOKEN": "t", "BOOTH_GATEWAY_URL": "http://core/modules"})
    with pytest.raises(DatabaseError, match="broker URL"):
        Database.from_env({"BOOTH_TOKEN": "t", "BOOTH_WORKSPACE": "acme"})


def test_from_env_uses_the_notebook_identity(monkeypatch):
    import sys
    import types

    fake = types.ModuleType("booth")
    fake.platform_token = lambda: "notebook-token"
    fake.workspace = "acme"
    monkeypatch.setitem(sys.modules, "booth", fake)
    opener = FakeOpener(response())
    d = Database.from_env({"BOOTH_GATEWAY_URL": "http://core/modules"}, opener=opener)
    d.credentials()
    assert opener.requests[0].get_header("Authorization") == "Bearer notebook-token"


def test_import_never_fails_unconfigured(monkeypatch):
    # The module-level helpers build their Database lazily.
    monkeypatch.setattr(booth_database, "_default", None)
    for key in ("BOOTH_TOKEN", "BOOTH_WORKSPACE", "BOOTH_GATEWAY_URL", "BOOTH_CREDENTIAL_BROKER_URL"):
        monkeypatch.delenv(key, raising=False)
    with pytest.raises(DatabaseError):
        booth_database.url()


def test_seconds_left():
    g = PostgresGrant("l", "acme", "read", time.time() + 100, "h", 5432, "d", "u", "p")
    assert 99 < g.seconds_left() <= 100


@pytest.mark.parametrize(
    "value,expected",
    [
        ("2026-09-29T12:05:00Z", 1790683500.0),
        ("2026-09-29T12:05:00.5Z", 1790683500.5),
        ("2026-09-29T12:05:00.123456789Z", 1790683500.123456),
        ("2026-09-29T14:05:00.123456789+02:00", 1790683500.123456),  # the offset's digits aren't fraction digits
        (1790683500, 1790683500.0),
    ],
)
def test_timestamp_parsing(value, expected):
    from booth_database.broker import _timestamp

    assert abs(_timestamp(value) - expected) < 1e-3
