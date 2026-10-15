# Design note: a table-scoped write role (ADR 0117 point 4)

Date: 2026-10-14. Status: **built** with the coordinator's rulings (ADR 0117 "Reconciled
design"); what was built and where it differs from this note: `docs/decisions/0008`. Builds on 0001 §3 (groups and lease roles), 0003 (TTL floor), 0006 (Tables view,
`booth_database` state) and 0007 (ignore list).

## §1 Role structure, and keeping it correct across migrations

**One NOLOGIN write group per grant**, named `bdb_w_<db hash>_<16 hex of sha256(db + grant id)>`.
The names are hashes, as in 0001 §2, because `pg_roles` is a shared catalog that every workspace can
read. The group is a member of nothing: not `_rw`, and not `_ro` either.

A write lease is an ordinary `bdb_lease_<id>` role that belongs to that group only. Like every other
lease it is `NOINHERIT`, has `SET role = <group>`, and gets CONNECT on the database directly. It is
not granted TEMPORARY on the database.

Write leases also get per-role session limits that read leases don't need:
- `statement_timeout 30s`;
- `lock_timeout 5s`;
- `idle_in_transaction_session_timeout 60s`.

UPDATE and DELETE privileges are enough to `LOCK TABLE … ACCESS EXCLUSIVE`, so without these limits
a stuck client could block the owner's own pipeline.

The group's privileges, per declared table:

- `SELECT` on the table, which `RETURNING`, `WHERE` and `ON CONFLICT` need.
- `DELETE` on the table, only if the declared operations include delete.
- `INSERT (cols)` and `UPDATE (cols)` as **column-level** grants on the declared writable columns,
  only if the operations include them. The database then enforces booth-api's column list as well.
- `USAGE` on the schema, and on every sequence the table's columns draw from. Those are the sequences
  `pg_depend` links to the table: serial and identity sequences, plus sequences named in a column
  default.
- Nothing else: no TRUNCATE, REFERENCES or TRIGGER, no CREATE on any schema, and no TEMPORARY.

**Keeping it correct: the grants are re-derived on every issuance, not maintained by hooks.** Each
lease request runs one transaction in the workspace database. It resolves the named tables to their
current OIDs and checks them (§2). It then revokes every privilege the group currently holds, found
through `aclexplode` over `pg_class` and `pg_namespace`, and grants exactly the requested set.

How that handles the owner's migrations:

- **Table dropped.** PostgreSQL drops its grants with it, so access ends at once.
- **Table recreated under the same name.** It has a new OID and no grant until the next issuance.
  booth-api should treat a 42501 on a declared table as "request a fresh lease once, then retry".
- **Column added.** It is readable at once, because SELECT is table-level. It is not writable until
  the owner declares it writable.
- **Sequence added or changed.** It is picked up at the next issuance.
- **Owner's GRANTs to the group.** A grant the owner makes to the group by hand is wiped at the next
  issuance.

I rejected event triggers, which would regrant inside the migration's own transaction. They need
superuser, which external mode may not have, and they would put this module's code into the owner's
migrations.

**A limitation to document:** a trigger function on a declared table runs with the write group's
privileges. A trigger that writes to another table therefore fails unless it is `SECURITY DEFINER`.
Foreign-key checks are unaffected, because they run as the table's owner.

## §2 How a caller asks for it

```
access: "tablewrite"           # new top-level value (see "Needs from booth-core")
scope: {"workspace": "<slug>",  # optional, as today
        "write": {"grant": "<opaque id, 1–128 chars, e.g. api:<api uuid>>",
                  "tables": [{"schema": "finance_input", "name": "transactions",
                              "operations": ["insert", "update", "delete"],
                              "columns": ["amount", "booked_on", "entered_by"]}]}}
options: none
201 scope echo: {workspace, database, access, grant, tables: [{schema, name, operations, columns, sequences}]}
```

I recommend a **new `access` value rather than `readwrite` plus a scope**, which means a change in
core.

- `readwrite` keeps meaning `_rw`, and the provider never issues `_rw` for `tablewrite`.
- `tablewrite` without `scope.write` is refused.
- So a request built badly anywhere along the path can only fail. It can never widen.

The other `access` values behave as they do today. A `read` or `readwrite` request carrying
`scope.write` is a 422.

**Verification**, all or nothing. A failure is a 422 `scope_not_supported` that names the first
offending table. The workspace always comes from the broker's `requester.workspace`, never from the
scope. Each named table must:

- exist now, matched by exact name, never by `search_path`;
- be an ordinary or partitioned table, and not a partition (as in the Tables view);
- be owned by the workspace's `_rw` group;
- not be in an excluded schema (`tables.ExcludedSchemas`: `pg_*`, `information_schema`);
- not match `tableEvents.ignore`, so a migration bookkeeping table is never writable (an open
  question below).

In addition:
- every named column must exist, must not be dropped, and must not be `GENERATED ALWAYS`;
- operations must be a subset of `insert`, `update` and `delete`;
- `grant` must be non-empty and contain no NUL;
- at most 50 tables per grant.

The provider does **not** check that the owner declared these targets. That is the broker's and
booth-api's job. ADR 0088 has the provider trust the broker's authorization; it only refuses what it
cannot scope narrowly.

## §3 Revocation

Privileges live on the group, not on the lease, so **narrowing applies to every live lease of that
grant at its next statement** (a statement already running finishes). That gives three paths:

- **A table or column removed from the declaration.** booth-api requests a lease with the new scope
  as the owner saves. The group is reset, and access ends within a second or so. The new lease's
  credential is simply used from then on.
- **Writes turned off.** booth-api sends one last request with `"tables": []`. This is allowed only
  together with `options: {"revoke": true}`, and it returns 201 with `credential: null`. The provider
  revokes everything from the group, ends the sessions of the grant's leases and drops those leases.
  This needs core's broker to relay a null credential; if it can't, the fallback is a credential that
  can do nothing.
- **booth-api gone or broken.** Leases expire within their lifetime (§4). Once a group has no members
  left, the reaper drops it and its privileges, so no grant outlives its last lease. An owner can
  also revoke from the Tables view (§5) without booth-api.

## §4 TTL and the reaper

The same provider window applies, `[leases.minTTL, leases.maxTTL]`, so one hour today.
**`DefaultMinTTL` is unchanged.** Revocation doesn't depend on the TTL (§3), so a shorter write TTL
would buy little and cost a role create and drop per renewal.

The reaper changes as follows:
- **Expired write leases** end the same way as any other lease.
- **The 2BP01 fallback** maps a write group back to its database through the db hash in the group's
  name.
- **One new sweep:** write groups with no members and no issuance for 10 minutes are dropped.
  Dropping one means `DROP OWNED BY` in the workspace database, then `DROP ROLE`.

Grants are recorded in `booth_database.write_grants (workspace, grant, group_role, tables jsonb,
last_issued, last_subject)`. That is the same private schema as 0006 §1, which leases cannot read.
The record exists for the UI and for audit. Authority stays with PostgreSQL's ACLs.

## §5 Tables view: yes

I recommend that the Tables view show, for any member, a "Writable through an API" badge on a
table, and on the table page the grant, its operations and its writable columns. The view should
read these from the live ACLs, so it shows what the database will actually allow, not what was
asked for. Owners also get a **Revoke** action (the §3 revoke, through the owner-only API of
0004).

The Database page stays the truthful browser. It already shows everything (0007 §2), and a member
looking at `finance_input.transactions` should see that an app can change it.

## §6 Tests (real PostgreSQL, as in `provision_test.go` and `state_test.go`)

All of these run for a `tablewrite` lease with one declared table, plus one declared with update
only on two columns.

**What it may do:**
- INSERT, UPDATE and DELETE on the declared table;
- `RETURNING`;
- `nextval` through a serial or identity column.

**What must fail with 42501:**
- UPDATE of an undeclared column;
- DELETE when it is not declared;
- SELECT, INSERT or DELETE on an undeclared table in the same schema, and in another schema;
- the `booth_database` schema;
- TRUNCATE;
- `CREATE TABLE`, `ALTER TABLE`, `CREATE INDEX` and `DROP TABLE`;
- `CREATE TEMP TABLE`;
- `SET ROLE` to `_rw` or `_ro`.

**Isolation and timeouts:**
- after `SET ROLE NONE`, the lease can do nothing;
- it cannot CONNECT to another workspace's database or to the maintenance database;
- `LOCK … ACCESS EXCLUSIVE` gives up after `lock_timeout`.

**"Cannot read what `_ro` cannot":** for every relation, the write group's SELECT is a subset of
`_ro`'s, checked with `has_table_privilege` across the whole database.

**Migrations:**
- after drop and recreate, writes fail until the next issuance and work after it;
- an added column is not writable;
- an extra GRANT the owner made by hand is gone after reissue.

**Revocation:** a scope narrowed while a lease is open stops its next statement; the `revoke`
request ends live sessions; the reaper drops a group with no members.

**Refusals:** 422 for each verification rule in §2; `tablewrite` without `scope.write`;
`readwrite` with `scope.write`.

## Needs from booth-core

1. The new top-level `access: "tablewrite"`, admitted by the app-scoped rule core designs. It must
   never be translated into `readwrite`.
2. The provider request's `requester` carrying, for audit only, the app subject that booth-api acts
   for (for example `onBehalfOf`).
3. A relayed 201 with `credential: null` for the revoke request (or its ruling against that).
4. A `contracts/credential-broker.md` update for the postgres `scope.write` shape.

## Needs from booth-api

1. A stable `grant` id per API, holding no workspace slug.
2. A full-scope request whenever the owner saves the write settings, and the revoke request when
   writes are turned off.
3. A one-time reissue on 42501.
4. Pooled connections that tolerate being cut (ADR 0095, fifth amendment).
5. Never asking for `readwrite`.

## Open questions for the user

1. Should a table matching `tableEvents.ignore` be unwritable, or does that need its own list?
2. Column-level INSERT and UPDATE grants (recommended), or table-level only?
3. Should the owner's **Revoke** in the Database view ship with the first build, or later?
