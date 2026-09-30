# 0005: Platform operators identified by the `/platform/operator` claim (ADR 0094)

Status: **implemented** (2026-09-30). Supersedes the operator-workspace allowlist of 0004 §1; the
rest of 0004 stands.

## What changed

- **An operator is anyone whose verified token has `/platform/operator` in its groups claim**
  (originally "a workspace owner whose…"; see "Resolved" below). The claim is read from the same verified token and groups claim as every workspace role
  (`auth.Identity.IsPlatformOperator`), never from a header. The match is exact: `/platform/operator/`,
  `/Platform/Operator`, `platform/operator`, `/platform/operators` and
  `/workspaces/platform/operator` all don't count (tested).
- **ADR 0093's scoping is otherwise unchanged**: an owner sees their own workspace, and an operator
  sees every workspace's database. It fails
  closed: with no operator claim, `/api/databases` is refused and the listing isn't even read.
- **The allowlist is gone**: `adminView.operatorWorkspaces` (chart) and
  `BOOTH_DATABASE_OPERATOR_WORKSPACES` (env). **Both are refused loudly rather than ignored.** A
  chart render with the old value fails with a message naming ADR 0094, and the service won't start
  with the old env var. Silently ignoring it would leave whoever set it believing it still grants,
  or limits, access.
- Owning a workspace named `platform`, the old stopgap's shape, grants nothing now (tested).
- `@projectbooth/database-ui` 0.1.1 changes one line of text ("Visible because you're a platform
  operator"). The props and API shape are unchanged.

## Resolved: operator status stands on its own (ADR 0094 clarification)

The first version of this change (`bb7f090`) kept "owner of the active workspace" as a precondition
for operators, the literal reading of "ADR 0093 scoping unchanged". I flagged that here, and it was
ruled the other way. ADR 0094's clarification says operator status is orthogonal to workspace
role, which is also how booth-lakehouse built it. So:

- **`GET /api/databases` has its own gate, `requireOperator`**: the `/platform/operator` claim
  alone, independent of the caller's role in the workspace they're acting in. An operator acting
  as a viewer or editor gets the full listing (tested).
- **`GET /api/status` keeps `requireOwner` exactly as before**, operator or not. The two routes
  used to share one blanket owner gate, which would have refused an operator before the operator
  check ever ran. Each route now has its own gate, so neither can shadow the other.
- Everything else is unchanged: the claim is still an exact match read from the verified token, a
  forged role header is still rejected (ADR 0041), and it still fails closed.
- **UI (0.1.2):** a non-owner can't call `/api/status`, the only route that reported the operator
  hint. So for non-owners the view now makes one probe of `/api/databases`. A 200 shows the
  listing; a 403 (the normal case) shows exactly what it showed before, with no error. The server
  enforces the rule either way. The probe only decides what to render.
