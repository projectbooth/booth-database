# 0008: Table-scoped write leases (`access: "tablewrite"`, ADR 0117)

Date: 2026-10-14. Status: **built, not yet run against booth-core's real broker** (its change was
in progress, not yet a PR, when this was written).

This was built from `docs/design-app-writes.md`, with the rulings in ADR 0117's "Reconciled
design" items 4 and 6. The code is in:
- `internal/provision/write.go`;
- `internal/credentialbroker/provider.go` (`serveTableWrite`);
- `internal/naming` (`WriteGroup`);
- `internal/api/tables.go` (`writableThrough`);
- `web/src` (`WritableBadge`).

## §1 Wire shape

`access: "tablewrite"` with a `scope.write` like this:

```
{"grant": "<id, 1–128 chars, no control characters>",
 "tables": [{"schema", "name", "operations": ["insert"|"update"|"delete"…], "columns": […]}]}
```

The other fields:
- **`requester.onBehalfOf`** is accepted on every access value. It goes into the audit line only.
- **`options`** accepts only `{"revoke": true}`, and only with `tablewrite`.

The 201 echo adds `scope.write: {grant, tables: [{schema, name, operations, columns,
sequences}]}`, which says what was actually granted.

**Revoke** is `"tables": []` with `options.revoke`. It answers 201 with:
- `credential: null`;
- `scope.write.revoked: true`;
- a fresh `leaseId`;
- `expiresAt` set to now, since nothing was issued.

booth-core's in-progress broker change matches this: it relays the null credential for exactly
this request shape and forwards `onBehalfOf` into `requester`.

**Refused with 422 `scope_not_supported`:** each of the following refusals is checked before any
database work:
- `tablewrite` without `scope.write`;
- `scope.write` on `read` or `readwrite`;
- a missing `tables`;
- `[]` without revoke, or revoke with tables;
- unknown fields at any level;
- operations outside insert, update and delete, or duplicates;
- insert or update with no columns, or columns with delete only;
- more than 50 tables.

Database refusals name the table, for any of these reasons:
- **the schema:** a system schema (`pg_*`, `information_schema`) or `booth_database`;
- **`tableEvents.ignore`:** a table that matches the ignore list (the same list as 0007);
- **missing:** the table doesn't exist;
- **not a table:** a view or other non-table;
- **a partition:** grant its partitioned table instead;
- **not the workspace's:** not owned by its `_rw`;
- **bad columns:** unknown, generated, or `GENERATED ALWAYS AS IDENTITY`.

One bad table refuses the whole request, and nothing changes.

## §2 Privileges

As in the note's §1.

**The group.** There is one NOLOGIN group per (database, grant), named
`bdb_w_<db hash>_<grant hash>`. It is a member of no role; any membership added by hand is revoked
at issuance.

**What each issuance does.** It runs one transaction in the workspace database, under an advisory
lock on the group, as `_rw` (so grants are recorded as the owner's). It:
1. revokes every privilege the group holds on relations, their columns, schemas and the database;
2. checks that nothing is left (if something is, the issuance fails);
3. grants SELECT, DELETE if declared, and `INSERT (cols)` / `UPDATE (cols)`;
4. grants USAGE on the table's schema and on its owned, identity and default-named sequences.

**The lease.** The lease is then created as before (0001 §3). It is a member of the group only,
with CONNECT and no TEMPORARY.

**Session defaults.** It gets per-role `statement_timeout 30s`, `lock_timeout 5s` and
`idle_in_transaction_session_timeout 60s`. A session can override these with SET, so they are
defaults for booth-api, not enforcement. The lease's expiry is the enforced bound.

## §3 Revocation, TTL and the reaper

- **Narrowing** (a reissue with a smaller scope) reaches every live lease of the grant at its next
  statement, including inside an open transaction. This is tested.
- **Revoke** runs `DROP OWNED BY` the group in the workspace database, which revokes everything
  whoever granted it. It then ends and drops the member leases, and drops the group.
- **TTL** is unchanged: `[leases.minTTL, leases.maxTTL]`, one hour by default. `DefaultMinTTL` is
  unchanged.
- **The reaper** also sweeps write groups that have no members and were last issued more than 10
  minutes ago (`DefaultWriteGroupIdle`). The issuance time is in the group's role comment, which
  holds a timestamp only, never the grant id.
- **Known race, accepted:** a sweep that read a group's old timestamp can drop it while an issuance
  is restamping it. That issuance then fails (502) and booth-api retries.

## §4 State and the Tables view

The table `booth_database.write_grants (group_role, workspace, grant_id, tables, last_issued,
last_subject, last_on_behalf_of)` maps a group back to its grant id, since the role name is a
one-way hash. It also records who last asked. Nothing authorizes from it.

**What the view shows.** `GET /api/tables` adds `writableThrough: [{grant, api, operations,
columns}]` to a table that a write group can change. It is read from the live ACLs.
- `api` is the grant id after `api:`, when the id has that form.
- Table events don't carry this field; they still carry `tables.Relation` alone.

**The UI** shows a read-only badge, "Writable through API <api>" or "Writable through an API". It
links to booth-api's page at **`/apis`**, its manifest `navPath`. The brief said `/api`, but that is
booth-core's own API path. The table page also lists the operations and columns. There is no
revoke control in this round.

## §5 Not tested

- **booth-core's real broker in the loop.** The provider was tested with fakes and the privileges
  against real PostgreSQL. The full chain (booth-api's sidecar, then the broker, then this
  provider) waits for booth-core's change and booth-api's step B.
- **External mode with a non-superuser admin.** The tests use the bundled-style superuser. Revoke's
  `DROP OWNED BY <group>` needs the admin to have the group's privileges. A non-superuser admin gets
  ADMIN on roles it creates, but not necessarily membership. This is the same requirement the
  existing lease drop already has.
- **A trigger on a declared table that writes elsewhere** fails unless it is `SECURITY DEFINER`.
  This is documented, not tested.
