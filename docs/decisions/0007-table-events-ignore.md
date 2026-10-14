# 0007: An ignore list for table events

Date: 2026-10-14

ADR 0116's reconcile loop published every relation in a workspace database, including a migration
tool's bookkeeping table (`schema_migrations` and the like), which is noise in the Catalog. The
chart value `tableEvents.ignore` lists relations that are never published. Code:
`internal/tables/ignore.go`, `Reconciler.Ignore`, `internal/config`.

## §1 Pattern syntax

A list of `schema.table` glob patterns. `*` matches any run of characters, including none and
including `.`. Every other character matches only itself, case-sensitively. A pattern must match
the whole `schema.name`, so `*.schema_migrations` matches `public.schema_migrations` and
`app.schema_migrations`, not `public.schema_migrations_old` or `public.Schema_Migrations`.

The default:

```yaml
tableEvents:
  ignore:
    - "*.schema_migrations"          # Rails, golang-migrate, Ecto, dbmate, …
    - "*.alembic_version"            # Alembic
    - "*.flyway_schema_history"      # Flyway
    - "*.django_migrations"          # Django
    - "*.__diesel_schema_migrations" # Diesel
```

`[]` ignores nothing. There is no `?` or `[…]`: PostgreSQL names can contain those characters,
and treating them literally keeps every pattern unambiguous.

The chart passes the list to the process as a JSON array in `BOOTH_DATABASE_TABLE_EVENTS_IGNORE`
(a name can contain a comma, so a comma-separated string would be ambiguous). Unset, the process
uses the same default list.

**Validated at startup.** A pattern that is empty, has leading or trailing whitespace, contains a
NUL, or has no `.` stops the module with an error naming it. A pattern with no `.` can't be a
`schema.table` pattern, so it is almost certainly a mistake (`schema_migrations` meant as
`*.schema_migrations`). A pattern that matches nothing is valid.

## §2 Events only

The list applies in the reconcile path only: relations matching it are left out of the listing
before it is compared with the last-published state. **The Tables tab, the preview and the SQL
console are not filtered.** The Database page is the truthful browser of the database and keeps
showing everything, migration tables included.

## §3 A published table that becomes ignored is a drop

Adding a pattern that matches a table already published (a config change) makes that table look
dropped to the comparison: `database.table.deleted` is published once, and the table is removed
from `booth_database.published_tables`. Later passes publish nothing for it, whatever happens to
the table. Removing the pattern again republishes it as `created`.

booth-catalog handles that delete like any drop (ADR 0116 fork C): it marks the row **missing**,
and a workspace owner deletes the row by hand. The module can't tell the Catalog "ignored, remove
it", and adding a reason to the event would change ADR 0116's event shape, so this release keeps
the shape and states the consequence in the chart comment instead.
