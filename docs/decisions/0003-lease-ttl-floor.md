# 0003: A one-hour provider floor on lease lifetime (ADR 0089 §3)

Status: **implemented** (2026-09-30). Closes the session-limit item left open in 0001 §3.

## What changed

The provider now clamps every lease's lifetime into a window `[MinTTL, MaxTTL]` of its own, in
`internal/credentialbroker/provider.go`. This is the mechanism ADR 0088 already allows and
booth-storage's MinIO provider already uses:

- **`MinTTL`, the floor, defaults to 1 hour** (`BOOTH_DATABASE_MIN_TTL`, chart `leases.minTTL: 1h`).
  A request asking for less is clamped **up** to it. The broker's own ceiling is 5 minutes
  (ADR 0088's `DefaultMaxTTL`), so in practice **every** request is clamped up, and every lease
  lives exactly one hour. That's the same situation as MinIO's 15-minute floor, which is also
  always above the broker ceiling.
- **`MaxTTL`, the cap, defaults to the floor** (`BOOTH_DATABASE_MAX_TTL`, chart `leases.maxTTL`
  empty). It only matters if booth-core's ceiling is ever raised above one hour. A cap configured
  below the floor is a startup error in the service, and is raised to the floor if it reaches the
  handler anyway.
- **booth-core's broker is unchanged.** Its ceiling still bounds what a caller may *ask* for.
  `Response.ExpiresAt` carries the real, clamped expiry, so core's audit trail records what was
  actually issued (ADR 0088: "`Response.ExpiresAt`, not the request's `ttlSeconds`, is always the
  source of truth").
- **Enforcement is unchanged.** PostgreSQL refuses new logins at `VALID UNTIL`, and the reaper ends
  any open session within `leases.reapInterval` of it. Only the number changed, not the invariant
  that an expired credential stops working (ADR 0089 §3).

## Why one hour

A lease's lifetime is simultaneously two things:

1. **The longest a single connection can live.** The reaper ends sessions at expiry, so this is the
   usability side, and the reason the 5-minute figure was too short (0001 §3).
2. **How long a leaked credential stays usable.** A raw Postgres password can't be revoked
   mid-flight (ADR 0080), which is the security side and the reason not to go arbitrarily high.

One hour sits at the point where the platform's own defaults already draw that line:

- **booth-pipeline's default task timeout is 3600s** (`DEFAULT_TIMEOUT_SECONDS`,
  `src/booth_pipeline/model.py`). A default-configured task can open one connection and keep it
  for its entire allowed run. A task configured longer (max 24h) is the exception, and it opts into
  that knowingly. It either opens connections per unit of work, or uses `booth_database.engine()`,
  which replaces connections before their credential expires (below).
- **booth-notebooks culls idle servers after 3600s** (`cullIdleSeconds: 3600`). A notebook
  connection therefore lasts as long as the server it lives in would survive idle, so an ordinary
  work session never hits the expiry.
- **It stays inside what the platform already accepts for native credentials.** MinIO's floor
  makes every s3 grant 15 minutes. An hour is 4× that, justified by a database session being a
  much longer-lived object than an object-store request batch. It's still a short, bounded window.
  Going to, say, 8–24h to cover long pipeline tasks outright would make a leaked credential usable
  for a working day, for a case `engine()` already handles.
- **What one hour doesn't cover:** a *single statement or transaction* running longer than an hour
  is ended at expiry. That's rare for this module's stated use case (ADR 0081: "smaller pieces of
  data"; large analytical work belongs in booth-lakehouse). An operator with a real need can raise
  `leases.minTTL`, which is the documented knob.

## Client follow-through

`booth_database` still *asks* for 300s (the broker's ceiling; asking for more is clamped down
anyway), and never assumes the lifetime it gets. `engine()` previously recycled pooled connections
based on the *requested* TTL. Under the floor, that would have minted a new lease every 4 minutes
for no reason. It now records each pooled connection's real `expires_at` from its grant, and retires
it at checkout only once it is within 60s of expiry. Docstrings and the README now say "an hour by
default" instead of "a few minutes".

## How this was verified

- Provider unit tests: the broker-sized 300s request, a 30s request, and an absent TTL all clamp up
  to 1h; over-cap requests clamp down; a configured floor/cap window is honoured inside, below and
  above; a cap below the floor can't undercut it.
- Config tests: default 1h/1h, cap defaults to the floor, cap-below-floor refused, zero floor
  refused.
- Real PostgreSQL (`internal/server`): a request carrying core's 300s ceiling comes back with
  `expiresAt` ≈ 1h, and PostgreSQL's own `pg_roles.rolvaliduntil` for the lease matches it to the
  second. The floor is what the server enforces, not just what the response says.
- Chart contract test: `BOOTH_DATABASE_MIN_TTL=1h` is rendered by default, and no cap.
- Client end to end (real provider binary + PostgreSQL): `engine()` reuses one lease across
  checkouts while it has time left, and replaces it with a fresh lease once within 60s of expiry.
  The expiry tests set the floor to 1s (and the cap to 1h) so expiry is observable in seconds, both
  here and in `hack/kind-integration.sh`.
- Found while doing this: the client end-to-end fixture piped the provider's stdout without
  draining it, so once enough audit lines accumulated the provider's logging blocked and requests
  hung (only in the full run). Fixed by logging to a file. This was a test-harness bug only: a
  deployed pod's stdout is always drained by the container runtime.
