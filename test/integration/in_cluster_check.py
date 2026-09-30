"""Runs inside the kind cluster as a Job (hack/kind-integration.sh): the deployed chart's real module
and real bundled PostgreSQL, reached over real cluster DNS with the credential's own `host`, exactly
as a notebook or pipeline pod would. Stands in for booth-core's broker with the same FakeCore the
client's end-to-end test uses (core's routing needs its full control plane; see
docs/decisions/0001). Exit 0 = every check passed."""

from __future__ import annotations

import os
import sys
import time

import psycopg

from booth_database import Database, DatabaseError
from fakecore import FakeCore

provider = os.environ["PROVIDER_URL"]
core = FakeCore(
    provider,
    os.environ["PROVIDER_CREDENTIAL"],
    {"editor-a": ("u-a", "it-a", "editor"), "viewer-a": ("u-v", "it-a", "viewer"), "editor-b": ("u-b", "it-b", "editor")},
)
broker = core.url + "/api/credentials"
failures = []


def check(name, fn):
    try:
        fn()
        print(f"PASS {name}", flush=True)
    except Exception as e:  # noqa: BLE001
        failures.append(name)
        print(f"FAIL {name}: {type(e).__name__}: {e}", flush=True)


a = Database(broker, "it-a", "editor-a")
viewer = Database(broker, "it-a", "viewer-a")
b = Database(broker, "it-b", "editor-b")


def rw_roundtrip():
    g = a.credentials()
    assert g.host.endswith(".svc.cluster.local"), g.host
    with a.connect() as conn:
        conn.execute("CREATE TABLE IF NOT EXISTS it_notes (body text)")
        conn.execute("INSERT INTO it_notes VALUES ('in-cluster')")
    with viewer.connect(read_only=True) as conn:
        assert ("in-cluster",) in conn.execute("SELECT body FROM it_notes").fetchall()


def viewer_cannot_write():
    try:
        viewer.connect()
        raise AssertionError("viewer got a readwrite credential")
    except DatabaseError as e:
        assert e.status == 403, e
    with viewer.connect(read_only=True) as conn:
        try:
            conn.execute("INSERT INTO it_notes VALUES ('nope')")
            raise AssertionError("read-only session wrote")
        except psycopg.Error:
            pass


def isolation():
    ga, gb = a.credentials(), b.credentials()
    assert ga.database != gb.database
    try:
        psycopg.connect(**{**gb.conninfo(), "dbname": ga.database}).close()
        raise AssertionError("workspace B's credential opened workspace A's database")
    except psycopg.OperationalError:
        pass
    for db in ("postgres", "template1"):
        try:
            psycopg.connect(**{**gb.conninfo(), "dbname": db}).close()
            raise AssertionError(f"a lease opened maintenance database {db}")
        except psycopg.OperationalError:
            pass


def expiry():
    short = Database(broker, "it-a", "editor-a", ttl_seconds=3)
    g = short.credentials()
    conn = psycopg.connect(**g.conninfo())
    time.sleep(4)
    try:
        psycopg.connect(**g.conninfo()).close()
        raise AssertionError("expired credential opened a new connection")
    except psycopg.OperationalError:
        pass
    deadline = time.time() + 30  # the deployed reaper (reapInterval set short by the script)
    while time.time() < deadline:
        try:
            conn.execute("SELECT 1")
        except psycopg.Error:
            return
        time.sleep(1)
    raise AssertionError("session outlived its credential by 30s: reaper not running?")


check("readwrite round trip over cluster DNS, visible to read-only", rw_roundtrip)
check("viewer refused readwrite, read-only session can't write", viewer_cannot_write)
check("workspace and maintenance-database isolation", isolation)
check("credential expiry: new logins refused, open session reaped", expiry)
core.close()
print(f"{4 - len(failures)}/4 passed", flush=True)
sys.exit(1 if failures else 0)
