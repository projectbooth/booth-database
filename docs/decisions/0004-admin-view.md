# 0004: The read-only admin view (ADR 0093)

Status: **implemented** (2026-09-30). Closes 0001 §8 ("no UI in v0") per ADR 0093.

## What was built

- **`GET /api/status`**, for owners of the active workspace. It returns the workspace's own database:
  whether it's provisioned yet, size, table count, active credentials, open connections, and
  created-at. Nothing is shown for a workspace until someone in it first asks for a credential
  (0001 §4), and looking never provisions anything (tested).
- **`GET /api/databases`**, for operators only (§1). It lists every workspace database on the
  server with the same figures, minus the table count.
- **`@projectbooth/database-ui`** (`web/`): one native view (`DatabaseApp`, props per ADR
  0031/0033), with no outer padding (ADR 0072). It shows the owner's status card, the operator table
  when allowed, and a one-line connect snippet for every role.
- **Manifest**: `hasOwnUi: true`, `uiIntegrationMode: native`, `navGroup: manage`,
  `navPath: /database`, and no `adminNavPath`, following ADR 0093's single-view default.
- **Nothing destructive, anywhere.** No route or button creates, alters or drops a database. A UI
  test asserts the only button is "Refresh". ADR 0089's "deletion unhandled" stands.

## §1 Judgment call: an owner sees only their own workspace; the cross-workspace list is operator-gated

ADR 0093 says "list the workspaces with a provisioned database." Taken literally for any workspace
owner, that would let the owner of workspace A enumerate every other workspace on the deployment.
That's exactly what ADR 0052 rules out for the user directory, and what this module's hashed
database names exist to prevent (0001 §2, ratified in ADR 0089 §2). ADR 0025 roles are
per-workspace, and the platform has no deployment-wide operator role.

So:

- **Default: owners see their own active workspace only.** That is the one workspace their token
  says they own. There's no parameter to ask about another.
- **An opt-in operator listing**, using booth-logging's existing stopgap for the same missing role
  (ADR 0067's `access.workspaces`). `adminView.operatorWorkspaces` (default empty) names workspaces
  whose *owners* may see every database. With it unset, nobody can call `/api/databases`.
- **The listing still doesn't store or reveal slugs.** The module never records a workspace's name
  (0001 §2), so databases are listed by hashed name. The only exceptions are workspaces the viewer
  is a member of: those are labelled with their slug, computed from the viewer's own token groups,
  not from storage. An operator can map any other workspace by hashing its slug (README).

**For the coordinator:** this is the second module needing "someone who can see across
workspaces" (booth-logging was the first). ADR 0067 said a real operator role would be worth an ADR
"if and when a second module needs the same distinction". This is that second module. I reused
the same allowlist rather than inventing anything, so a future operator role would replace both in
one place.

## §2 Judgment call: owner-only, not owner+editor

The figures are infrastructure status (size, credential and connection counts). They're useful for
the person who administers the workspace and don't help anyone write code. Editors already see what
matters to them through the database itself. So both routes require the owner role, derived from
the token. The UI mirrors that: editors and viewers get a short explanation and the connect snippet,
with no API call made.

## §3 Judgment call: human tokens only

The admin API trusts the OIDC issuer only, not booth-core's workload-token issuer (ADR 0056).
booth-storage accepts both for its data API. Here the view is for people, and a pipeline run's
workload token can legitimately carry the owner role, so it shouldn't open an admin view. Tested:
a genuine token from a second, real issuer is refused.

## §4 Judgment call: the admin view can't break credential issuance

- The OIDC settings are optional in the chart. With them unset, `/api/*` answers **503** and nothing
  gets through. The view explains the 503 as "no identity provider configured".
- OIDC discovery runs in the background with retries, so an unreachable identity provider never
  stops the pod starting or the broker path working.
- Readiness (`/healthz`) doesn't depend on it either.

## §5 What's reported, and why it's cheap

| Field | Source | Cost |
|---|---|---|
| created-at | Recorded in the database's ready marker at provisioning time (PostgreSQL keeps no creation time). Databases provisioned earlier show "Unknown". | Catalog read |
| size | `pg_database_size` | Stats the database's files, never reads rows |
| active credentials | Unexpired `bdb_lease_*` roles in the workspace's groups | Catalog read |
| open connections | `pg_stat_activity` | Catalog read |
| tables | Count from `pg_class`, own workspace only | One short connection into that one database; omitted from the cross-workspace list |

The created-at timestamp is in the database's comment, which any session on the server can read
(`pg_shdescription`). It reveals when some unnamed workspace first used its database, the same
class of metadata as the database count 0001 §2 already accepts, and no slug.

## How this was verified

- **Auth, with real signed tokens** through go-oidc's real verification path (a test IdP with
  discovery, JWKS and RSA signing, same approach as booth-storage). Covered:
  - valid, expired, unknown-key, wrong-issuer and missing-subject tokens;
  - a real second issuer, which is refused;
  - audience checks and the configurable groups claim;
  - an editor token with a forged `X-Booth-Role: owner`, which is refused (ADR 0041);
  - a gateway narrowing an owner, and a missing workspace header.
- **API**:
  - the owner/editor/viewer matrix, where only the owner gets in;
  - active-workspace-only, where a query parameter can't redirect it;
  - the operator listing, which a non-operator can't call, and which isn't even read for them;
  - slugs labelled only for the viewer's own workspaces, with no other slug in the response;
  - 503 with no verifier, read-only routes (405 on write verbs), and 502 without leaking detail.
- **Real PostgreSQL**:
  - status for a never-used workspace says "not provisioned", and looking doesn't create a database;
  - after a lease and two tables: created-at ≈ now, `tables = 2`, size > 0, ≥1 active credential;
  - the operator listing includes it, unlabelled, and without connecting into it.
- **UI** (vitest): owner card, not-provisioned, non-owner makes no requests, headers (X-Workspace,
  fresh bearer, no header for a null token), operator table labels, "Refresh" as the only button,
  the 503/403 messages, refresh, and re-fetch on a workspace switch. I also rendered it in a real
  browser via the dev harness against a mock backend, in light and dark mode.
- **Not done:** the view mounted inside booth-design's real shell (that's booth-design's routing
  item), and the API against a real Keycloak on a cluster (booth-e2e's tier).
