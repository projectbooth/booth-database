# 0005: Platform operators identified by the `/platform/operator` claim (ADR 0094)

Status: **implemented** (2026-09-30). Supersedes the operator-workspace allowlist of 0004 §1; the
rest of 0004 stands.

## What changed

- **An operator is a workspace owner whose verified token has `/platform/operator` in its groups
  claim.** The claim is read from the same verified token and groups claim as every workspace role
  (`auth.Identity.IsPlatformOperator`), never from a header. The match is exact: `/platform/operator/`,
  `/Platform/Operator`, `platform/operator`, `/platform/operators` and
  `/workspaces/platform/operator` all don't count (tested).
- **ADR 0093's scoping is unchanged**, as ADR 0094 requires: both routes stay owner-only, an owner
  sees their own workspace by default, and an operator sees every workspace's database. It fails
  closed: with no operator claim, `/api/databases` is refused and the listing isn't even read.
- **The allowlist is gone**: `adminView.operatorWorkspaces` (chart) and
  `BOOTH_DATABASE_OPERATOR_WORKSPACES` (env). **Both are refused loudly rather than ignored.** A
  chart render with the old value fails with a message naming ADR 0094, and the service won't start
  with the old env var. Silently ignoring it would leave whoever set it believing it still grants,
  or limits, access.
- Owning a workspace named `platform`, the old stopgap's shape, grants nothing now (tested).
- `@projectbooth/database-ui` 0.1.1 changes one line of text ("Visible because you're a platform
  operator"). The props and API shape are unchanged.

## One thing worth a look

ADR 0094 calls operator status "a property of the person, not of any workspace they happen to be
acting in", but also keeps ADR 0093's owner-only scoping unchanged. Following the instruction
literally, an operator still has to be an **owner of whichever workspace they're viewing the page
from** to get the listing. An operator who is only an editor of their current workspace, or holds
no workspace role at all, can't reach it.

I kept the literal reading, since it's the stricter one and the one asked for. If operator status
should stand on its own (for example, `/api/databases` for any verified operator regardless of
active-workspace role), that's a small, contained change to `isOperator` plus the owner gate. I'd
want it ruled on rather than assumed, because it widens who can reach the route.
