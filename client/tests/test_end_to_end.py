"""End to end: ``booth_database.connect()`` -> (a faithful stand-in for) booth-core's broker -> the
real booth-database provider binary -> real PostgreSQL, and back to a working connection.

Needs Go (to build the provider) and ``BOOTH_TEST_POSTGRES_ADMIN_DSN``; skips without them unless
``BOOTH_TEST_REQUIRE_POSTGRES`` is set, in which case it fails (CI sets it — never skip green)."""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

import pytest

from booth_database import Database, DatabaseError

from .fakecore import FakeCore

REPO = Path(__file__).resolve().parents[2]
PROVIDER_CREDENTIAL = "bcbp.database.e2e-test"


def _require(reason: str):
    if os.environ.get("BOOTH_TEST_REQUIRE_POSTGRES"):
        pytest.fail(reason + " (BOOTH_TEST_REQUIRE_POSTGRES is set: refusing to skip)")
    pytest.skip(reason)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


@pytest.fixture(scope="module")
def stack(tmp_path_factory):
    dsn = os.environ.get("BOOTH_TEST_POSTGRES_ADMIN_DSN")
    if not dsn:
        _require("BOOTH_TEST_POSTGRES_ADMIN_DSN not set")
    if not shutil.which("go"):
        _require("go not installed")
    pytest.importorskip("psycopg")

    binary = tmp_path_factory.mktemp("bin") / ("booth-database" + (".exe" if os.name == "nt" else ""))
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/database"], cwd=REPO, check=True)

    u = urllib.parse.urlparse(dsn)
    port = _free_port()
    env = {
        **os.environ,
        "BOOTH_HTTP_ADDR": f"127.0.0.1:{port}",
        "BOOTH_DATABASE_MODE": "external",
        "BOOTH_DATABASE_ADMIN_HOST": u.hostname,
        "BOOTH_DATABASE_ADMIN_PORT": str(u.port or 5432),
        "BOOTH_DATABASE_ADMIN_USER": urllib.parse.unquote(u.username),
        "BOOTH_DATABASE_ADMIN_PASSWORD": urllib.parse.unquote(u.password),
        "BOOTH_DATABASE_ADMIN_DBNAME": u.path.lstrip("/") or "postgres",
        "BOOTH_DATABASE_ADMIN_SSLMODE": "disable",
        "BOOTH_DATABASE_RESTRICT_MAINTENANCE_ACCESS": "true",
        "BOOTH_DATABASE_REAP_INTERVAL": "1s",
        "BOOTH_CREDENTIAL_BROKER_CREDENTIAL": PROVIDER_CREDENTIAL,
    }
    proc = subprocess.Popen([str(binary)], env=env, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    provider = f"http://127.0.0.1:{port}"
    deadline = time.time() + 30
    while True:
        try:
            with urllib.request.urlopen(provider + "/healthz", timeout=2) as r:
                if r.status == 200:
                    break
        except OSError:
            pass
        if time.time() > deadline or proc.poll() is not None:
            proc.kill()
            pytest.fail("provider never became healthy:\n" + proc.stdout.read().decode(errors="replace"))
        time.sleep(0.2)

    ws_a, ws_b = f"e2e-a-{uuid.uuid4().hex[:8]}", f"e2e-b-{uuid.uuid4().hex[:8]}"
    core = FakeCore(
        provider,
        PROVIDER_CREDENTIAL,
        {
            "editor-a": ("user-editor", ws_a, "editor"),
            "viewer-a": ("user-viewer", ws_a, "viewer"),
            "editor-b": ("user-b", ws_b, "editor"),
        },
    )
    yield {"core": core, "ws_a": ws_a, "ws_b": ws_b, "proc": proc}
    core.close()
    proc.terminate()
    out, _ = proc.communicate(timeout=10)
    log = out.decode(errors="replace")
    # The provider's own audit lines must never contain an issued password (ADR 0080).
    for g in _issued:
        assert g not in log, "an issued password appeared in the provider's log"


_issued: list[str] = []


def database(stack, token: str, workspace: str, ttl: int = 300) -> Database:
    return Database(stack["core"].url + "/api/credentials", workspace, token, ttl_seconds=ttl)


def test_connect_create_and_read_back(stack):
    d = database(stack, "editor-a", stack["ws_a"])
    _issued.append(d.credentials().password)
    with d.connect() as conn:
        conn.execute("CREATE TABLE notes (id serial PRIMARY KEY, body text)")
        conn.execute("INSERT INTO notes (body) VALUES (%s)", ["hello from e2e"])
    with d.connect(read_only=True) as conn:
        assert conn.execute("SELECT body FROM notes").fetchall() == [("hello from e2e",)]


def test_viewer_reads_but_cannot_write(stack):
    editor = database(stack, "editor-a", stack["ws_a"])
    with editor.connect() as conn:
        conn.execute("CREATE TABLE IF NOT EXISTS viewer_check (v int)")
        conn.execute("INSERT INTO viewer_check VALUES (7)")
    viewer = database(stack, "viewer-a", stack["ws_a"])
    with pytest.raises(DatabaseError, match="read_only=True") as exc:
        viewer.connect()  # readwrite: refused by the broker's role rule, never reaches the provider
    assert exc.value.status == 403
    import psycopg

    with viewer.connect(read_only=True) as conn:
        assert conn.execute("SELECT v FROM viewer_check").fetchone() == (7,)
        with pytest.raises(psycopg.Error):
            conn.execute("INSERT INTO viewer_check VALUES (8)")


def test_workspaces_are_isolated(stack):
    import psycopg

    a = database(stack, "editor-a", stack["ws_a"])
    with a.connect() as conn:
        conn.execute("CREATE TABLE IF NOT EXISTS a_only (v text)")
    b = database(stack, "editor-b", stack["ws_b"])
    grant_b = b.credentials()
    with psycopg.connect(**grant_b.conninfo()) as conn:
        assert conn.execute("SELECT count(*) FROM information_schema.tables WHERE table_name = 'a_only'").fetchone() == (0,)
    # B's credential, aimed at A's database, is refused by PostgreSQL itself.
    a_db = a.credentials().database
    with pytest.raises(psycopg.OperationalError):
        psycopg.connect(**{**grant_b.conninfo(), "dbname": a_db})
    # And asking the broker for A's workspace from B's session is refused before any credential exists.
    with pytest.raises(DatabaseError):
        database(stack, "editor-b", stack["ws_a"]).credentials()


def test_credential_expires(stack):
    import psycopg

    d = database(stack, "editor-a", stack["ws_a"], ttl=2)
    grant = d.credentials()
    assert grant.seconds_left() <= 2.5
    conn = psycopg.connect(**grant.conninfo())
    time.sleep(3)
    with pytest.raises(psycopg.OperationalError):
        psycopg.connect(**grant.conninfo())  # new logins refused past expiry
    time.sleep(2)  # the running provider's reaper (1s interval here) ends the open session
    with pytest.raises(psycopg.Error):
        conn.execute("SELECT 1")
    conn.close()


def test_engine_with_pandas_style_usage(stack):
    sqlalchemy = pytest.importorskip("sqlalchemy")
    d = database(stack, "editor-a", stack["ws_a"])
    eng = d.engine()
    with eng.begin() as conn:
        conn.execute(sqlalchemy.text("CREATE TABLE IF NOT EXISTS via_engine (v int)"))
        conn.execute(sqlalchemy.text("INSERT INTO via_engine VALUES (1)"))
    with eng.connect() as conn:
        assert conn.execute(sqlalchemy.text("SELECT count(*) FROM via_engine")).scalar() >= 1
    eng.dispose()
